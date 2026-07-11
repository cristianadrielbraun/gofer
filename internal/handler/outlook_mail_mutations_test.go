package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
	"golang.org/x/oauth2"
)

func TestOutlookGraphMutationUsesProviderMessageIDAndCachesMovedID(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Write().ExecContext(ctx, `INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address)
		VALUES ('acc', 'default', ?, 'subject-id', 'user@example.com')`, providers.ProviderOutlook); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "graph-inbox", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_archive", AccountID: "acc", RemoteID: "Archive", ProviderRemoteID: "graph-archive", Name: "Archive", Role: "archive", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	ids, err := db.UpsertProviderSyncMessages(ctx, []storage.ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "graph-message-1",
		InternetMessageID: "<graph-mutation@example.com>",
		Subject:           "Graph mutation",
		FromEmail:         "sender@example.com",
		DateSent:          time.Now(),
		DateReceived:      time.Now(),
		IsRead:            true,
	}})
	if err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
	msgID := ids["graph-message-1"]
	if msgID == 0 {
		t.Fatal("provider message was not inserted")
	}
	expires := time.Now().Add(time.Hour)
	manager := auth.NewManager(&auth.Config{}, db)
	if err := manager.UpsertOAuthAccount(ctx, "default", providers.OAuthMicrosoft, "subject-id", "stale-token", "refresh-token", "Bearer", &expires, ""); err != nil {
		t.Fatalf("UpsertOAuthAccount() error = %v", err)
	}

	var sawReadPatch bool
	var sawMove bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm() error = %v", err)
			}
			if got := r.FormValue("refresh_token"); got != "refresh-token" {
				t.Fatalf("refresh_token = %q, want refresh-token", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"graph-token","refresh_token":"refresh-token","token_type":"Bearer","expires_in":3600}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/me/messages/graph-message-1":
			if got := r.Header.Get("Authorization"); got != "Bearer graph-token" {
				t.Fatalf("Authorization = %q", got)
			}
			if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
				t.Fatalf("Prefer = %q, want immutable ID preference", r.Header.Get("Prefer"))
			}
			var payload map[string]bool
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode read patch: %v", err)
			}
			if payload["isRead"] {
				t.Fatalf("read patch payload = %#v, want isRead false", payload)
			}
			sawReadPatch = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/me/messages/graph-message-1/move":
			if got := r.Header.Get("Authorization"); got != "Bearer graph-token" {
				t.Fatalf("Authorization = %q", got)
			}
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode move payload: %v", err)
			}
			if payload["destinationId"] != "graph-archive" {
				t.Fatalf("move payload = %#v, want graph archive destination", payload)
			}
			sawMove = true
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "graph-message-2"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	manager = auth.NewManager(&auth.Config{MicrosoftClient: &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: server.URL + "/token"}}}, db)
	previousGraphBase := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousGraphBase })

	h := &Handler{db: db, auth: manager}
	info, err := db.GetMessageMutationInfo(ctx, msgID)
	if err != nil || info == nil {
		t.Fatalf("GetMessageMutationInfo() = %#v, %v", info, err)
	}
	if err := db.SetMessageReadAndQueue(ctx, msgID, false); err != nil {
		t.Fatalf("SetMessageReadAndQueue() error = %v", err)
	}
	h.runDueMessageMutations(ctx)
	h.moveRemoteMessage(ctx, msgID, *info, "acc_archive", "Archive")

	if !sawReadPatch {
		t.Fatal("Graph read PATCH was not observed")
	}
	if !sawMove {
		t.Fatal("Graph move request was not observed")
	}
	var providerID string
	if err := db.Read().QueryRowContext(ctx, `SELECT COALESCE(remote_message_id, '') FROM messages WHERE id = ?`, msgID).Scan(&providerID); err != nil {
		t.Fatalf("query provider id: %v", err)
	}
	if providerID != "graph-message-2" {
		t.Fatalf("remote_message_id = %q, want moved graph id", providerID)
	}
}

func TestOutlookGraphBodyFetchUsesProviderMessageID(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Write().ExecContext(ctx, `INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address)
		VALUES ('acc', 'default', ?, 'subject-id', 'user@example.com')`, providers.ProviderOutlook); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "graph-inbox", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	ids, err := db.UpsertProviderSyncMessages(ctx, []storage.ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "graph-message-1",
		InternetMessageID: "<graph-body@example.com>",
		Subject:           "Graph body",
		FromEmail:         "sender@example.com",
		DateSent:          time.Now(),
		DateReceived:      time.Now(),
		IsRead:            true,
	}})
	if err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
	msgID := ids["graph-message-1"]
	if msgID == 0 {
		t.Fatal("provider message was not inserted")
	}
	expires := time.Now().Add(time.Hour)
	manager := auth.NewManager(&auth.Config{}, db)
	if err := manager.UpsertOAuthAccount(ctx, "default", providers.OAuthMicrosoft, "subject-id", "stale-token", "refresh-token", "Bearer", &expires, ""); err != nil {
		t.Fatalf("UpsertOAuthAccount() error = %v", err)
	}

	const rawMIME = "From: Sender <sender@example.com>\r\nSubject: Graph body\r\n\r\nGraph body text\r\n"
	var sawFetch bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"graph-token","refresh_token":"refresh-token","token_type":"Bearer","expires_in":3600}`))
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages/graph-message-1/$value":
			if got := r.Header.Get("Authorization"); got != "Bearer graph-token" {
				t.Fatalf("Authorization = %q", got)
			}
			if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
				t.Fatalf("Prefer = %q, want immutable ID preference", r.Header.Get("Prefer"))
			}
			sawFetch = true
			w.Header().Set("Content-Type", "message/rfc822")
			_, _ = w.Write([]byte(rawMIME))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	manager = auth.NewManager(&auth.Config{MicrosoftClient: &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: server.URL + "/token"}}}, db)
	previousGraphBase := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousGraphBase })

	h := &Handler{db: db, auth: manager}
	info, err := db.GetMessageFetchInfo(ctx, msgID)
	if err != nil || info == nil {
		t.Fatalf("GetMessageFetchInfo() = %#v, %v", info, err)
	}
	body, err := h.fetchBodyRemote(ctx, msgID, info)
	if err != nil {
		t.Fatalf("fetchBodyRemote() error = %v", err)
	}
	if !sawFetch {
		t.Fatal("Graph body fetch was not observed")
	}
	if string(body) != rawMIME {
		t.Fatalf("body = %q, want %q", string(body), rawMIME)
	}
}

func TestOutlookGraphAttachmentFetchMaterializesProviderAttachment(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Write().ExecContext(ctx, `INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address)
		VALUES ('acc', 'default', ?, 'subject-id', 'user@example.com')`, providers.ProviderOutlook); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "graph-inbox", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	ids, err := db.UpsertProviderSyncMessages(ctx, []storage.ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "graph-message-1",
		InternetMessageID: "<graph-attachment@example.com>",
		Subject:           "Graph attachment",
		FromEmail:         "sender@example.com",
		DateSent:          time.Now(),
		DateReceived:      time.Now(),
		IsRead:            true,
		HasAttachments:    true,
	}})
	if err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
	msgID := ids["graph-message-1"]
	if msgID == 0 {
		t.Fatal("provider message was not inserted")
	}
	if err := db.ReplaceAttachments(ctx, msgID, []storage.AttachmentRow{{
		Filename:         "graph.txt",
		ContentType:      "text/plain",
		SizeBytes:        18,
		ProviderRemoteID: "graph-attachment-1",
	}}); err != nil {
		t.Fatalf("ReplaceAttachments() error = %v", err)
	}
	var attID int64
	if err := db.Read().QueryRowContext(ctx, `SELECT id FROM attachments WHERE message_id = ?`, msgID).Scan(&attID); err != nil {
		t.Fatalf("query attachment id: %v", err)
	}
	expires := time.Now().Add(time.Hour)
	manager := auth.NewManager(&auth.Config{}, db)
	if err := manager.UpsertOAuthAccount(ctx, "default", providers.OAuthMicrosoft, "subject-id", "stale-token", "refresh-token", "Bearer", &expires, ""); err != nil {
		t.Fatalf("UpsertOAuthAccount() error = %v", err)
	}

	const attachmentBody = "graph attachment data"
	var sawFetch bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"graph-token","refresh_token":"refresh-token","token_type":"Bearer","expires_in":3600}`))
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages/graph-message-1/attachments/graph-attachment-1/$value":
			if got := r.Header.Get("Authorization"); got != "Bearer graph-token" {
				t.Fatalf("Authorization = %q", got)
			}
			if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
				t.Fatalf("Prefer = %q, want immutable ID preference", r.Header.Get("Prefer"))
			}
			sawFetch = true
			_, _ = w.Write([]byte(attachmentBody))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	manager = auth.NewManager(&auth.Config{MicrosoftClient: &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: server.URL + "/token"}}}, db)
	previousGraphBase := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousGraphBase })

	h := &Handler{db: db, auth: manager, blobStore: store.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))}
	info, err := db.GetAttachmentFetchInfo(ctx, attID)
	if err != nil || info == nil {
		t.Fatalf("GetAttachmentFetchInfo() = %#v, %v", info, err)
	}
	path, err := h.ensureAttachmentStorage(ctx, info)
	if err != nil {
		t.Fatalf("ensureAttachmentStorage() error = %v", err)
	}
	if !sawFetch {
		t.Fatal("Graph attachment fetch was not observed")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if string(data) != attachmentBody {
		t.Fatalf("attachment body = %q, want %q", string(data), attachmentBody)
	}
	var storedPath string
	if err := db.Read().QueryRowContext(ctx, `SELECT storage_path FROM attachments WHERE id = ?`, attID).Scan(&storedPath); err != nil {
		t.Fatalf("query stored path: %v", err)
	}
	if storedPath != path {
		t.Fatalf("stored path = %q, want %q", storedPath, path)
	}
}

func TestOutlookGraphInlineContentMaterializesProviderAttachment(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Write().ExecContext(ctx, `INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address)
		VALUES ('acc', 'default', ?, 'subject-id', 'user@example.com')`, providers.ProviderOutlook); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "graph-inbox", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	ids, err := db.UpsertProviderSyncMessages(ctx, []storage.ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "graph-message-1",
		InternetMessageID: "<graph-inline@example.com>",
		Subject:           "Graph inline",
		FromEmail:         "sender@example.com",
		DateSent:          time.Now(),
		DateReceived:      time.Now(),
		IsRead:            true,
		HasAttachments:    true,
	}})
	if err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
	msgID := ids["graph-message-1"]
	if msgID == 0 {
		t.Fatal("provider message was not inserted")
	}
	if err := db.ReplaceAttachments(ctx, msgID, []storage.AttachmentRow{{
		Filename:         "logo.png",
		ContentType:      "image/png",
		SizeBytes:        7,
		ContentID:        "logo@example.com",
		Inline:           true,
		ProviderRemoteID: "graph-inline-1",
	}}); err != nil {
		t.Fatalf("ReplaceAttachments() error = %v", err)
	}
	expires := time.Now().Add(time.Hour)
	manager := auth.NewManager(&auth.Config{}, db)
	if err := manager.UpsertOAuthAccount(ctx, "default", providers.OAuthMicrosoft, "subject-id", "stale-token", "refresh-token", "Bearer", &expires, ""); err != nil {
		t.Fatalf("UpsertOAuthAccount() error = %v", err)
	}

	const inlineBody = "pngdata"
	var sawFetch bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"graph-token","refresh_token":"refresh-token","token_type":"Bearer","expires_in":3600}`))
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages/graph-message-1/attachments/graph-inline-1/$value":
			if got := r.Header.Get("Authorization"); got != "Bearer graph-token" {
				t.Fatalf("Authorization = %q", got)
			}
			if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
				t.Fatalf("Prefer = %q, want immutable ID preference", r.Header.Get("Prefer"))
			}
			sawFetch = true
			_, _ = w.Write([]byte(inlineBody))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	manager = auth.NewManager(&auth.Config{MicrosoftClient: &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: server.URL + "/token"}}}, db)
	previousGraphBase := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousGraphBase })

	h := &Handler{db: db, auth: manager, blobStore: store.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))}
	req := httptest.NewRequest(http.MethodGet, "/api/inline-content/"+strconv.FormatInt(msgID, 10)+"/logo@example.com", nil)
	req.SetPathValue("messageID", strconv.FormatInt(msgID, 10))
	req.SetPathValue("contentID", "logo@example.com")
	rr := httptest.NewRecorder()

	h.handleInlineContent(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("handleInlineContent status = %d, body %q", rr.Code, rr.Body.String())
	}
	if !sawFetch {
		t.Fatal("Graph inline attachment fetch was not observed")
	}
	if got := rr.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
	if rr.Body.String() != inlineBody {
		t.Fatalf("inline body = %q, want %q", rr.Body.String(), inlineBody)
	}
	var storedPath string
	if err := db.Read().QueryRowContext(ctx, `SELECT storage_path FROM attachments WHERE message_id = ? AND content_id = ?`, msgID, "logo@example.com").Scan(&storedPath); err != nil {
		t.Fatalf("query stored path: %v", err)
	}
	if storedPath == "" {
		t.Fatal("inline attachment storage path was not persisted")
	}
}
