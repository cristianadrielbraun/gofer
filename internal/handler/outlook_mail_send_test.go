package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
	"golang.org/x/oauth2"
)

func TestSendOutlookGraphMessageHandlesOutlookWithoutAuth(t *testing.T) {
	h := &Handler{}
	handled, status, errText := h.sendOutlookGraphMessage(context.Background(), &models.AccountConfig{AccountID: "acc", Provider: providers.ProviderOutlook}, &message.OutgoingMessage{}, "")
	if !handled || status != "failed" || !strings.Contains(errText, "microsoft oauth not configured") {
		t.Fatalf("sendOutlookGraphMessage() = handled %v status %q err %q, want handled Graph failure", handled, status, errText)
	}
}

func TestSendOutlookGraphMessageIgnoresNonOutlook(t *testing.T) {
	h := &Handler{}
	handled, status, errText := h.sendOutlookGraphMessage(context.Background(), &models.AccountConfig{AccountID: "acc", Provider: providers.ProviderIMAP}, &message.OutgoingMessage{}, "")
	if handled || status != "" || errText != "" {
		t.Fatalf("sendOutlookGraphMessage() = handled %v status %q err %q, want non-Outlook ignored", handled, status, errText)
	}
}

func TestSaveOutlookGraphDraftCreatesMIMEDraftAndCachesProviderID(t *testing.T) {
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
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{{ID: "acc_drafts", AccountID: "acc", RemoteID: "Drafts", ProviderRemoteID: "graph-drafts", Name: "Drafts", Role: "drafts", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	msgID, err := db.SaveDraftMessage(ctx, storage.DraftMessageInput{
		AccountID:         "acc",
		FolderID:          "acc_drafts",
		InternetMessageID: "<draft@example.com>",
		Subject:           "Graph draft",
		FromEmail:         "user@example.com",
		Date:              time.Now(),
	})
	if err != nil {
		t.Fatalf("SaveDraftMessage() error = %v", err)
	}
	expires := time.Now().Add(time.Hour)
	manager := auth.NewManager(&auth.Config{}, db)
	if err := manager.UpsertOAuthAccount(ctx, "default", providers.OAuthMicrosoft, "subject-id", "stale-token", "refresh-token", "Bearer", &expires, ""); err != nil {
		t.Fatalf("UpsertOAuthAccount() error = %v", err)
	}

	var sawDraftCreate bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"graph-token","refresh_token":"refresh-token","token_type":"Bearer","expires_in":3600}`))
		case r.Method == http.MethodPost && r.URL.Path == "/me/messages":
			if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
				t.Fatalf("Content-Type = %q, want text/plain", got)
			}
			if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
				t.Fatalf("Prefer = %q, want immutable ID preference", r.Header.Get("Prefer"))
			}
			body := readRequestBody(t, r)
			raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(body))
			if err != nil {
				t.Fatalf("draft MIME is not base64: %v", err)
			}
			assertMIMEHeaders(t, raw, "Graph draft", "<draft@example.com>", "hidden@example.com")
			sawDraftCreate = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "graph-draft-1", "internetMessageId": "<draft@example.com>"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	manager = auth.NewManager(&auth.Config{MicrosoftClient: &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: server.URL + "/token"}}}, db)
	previousGraphBase := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousGraphBase })

	to, _ := message.ParseAddressList("recipient@example.com")
	bcc, _ := message.ParseAddressList("hidden@example.com")
	h := &Handler{db: db, auth: manager}
	err = h.saveOutlookGraphDraft(ctx, "acc", msgID, &message.OutgoingMessage{
		FromEmail: "user@example.com",
		To:        to,
		Bcc:       bcc,
		Subject:   "Graph draft",
		TextBody:  "Draft body",
		MessageID: "<draft@example.com>",
		Date:      time.Now(),
	})
	if err != nil {
		t.Fatalf("saveOutlookGraphDraft() error = %v", err)
	}
	if !sawDraftCreate {
		t.Fatal("Graph draft create was not observed")
	}
	var providerID string
	if err := db.Read().QueryRowContext(ctx, `SELECT COALESCE(remote_message_id, '') FROM messages WHERE id = ?`, msgID).Scan(&providerID); err != nil {
		t.Fatalf("query provider id: %v", err)
	}
	if providerID != "graph-draft-1" {
		t.Fatalf("remote_message_id = %q, want graph-draft-1", providerID)
	}
}

func TestSendOutlookGraphMessageUsesSendMailMIMEAndCachesSentID(t *testing.T) {
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
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{{ID: "acc_sent", AccountID: "acc", RemoteID: "Sent Items", ProviderRemoteID: "graph-sent", Name: "Sent", Role: "sent", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	expires := time.Now().Add(time.Hour)
	manager := auth.NewManager(&auth.Config{}, db)
	if err := manager.UpsertOAuthAccount(ctx, "default", providers.OAuthMicrosoft, "subject-id", "stale-token", "refresh-token", "Bearer", &expires, ""); err != nil {
		t.Fatalf("UpsertOAuthAccount() error = %v", err)
	}

	var sawSend bool
	var sawReconcile bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"graph-token","refresh_token":"refresh-token","token_type":"Bearer","expires_in":3600}`))
		case r.Method == http.MethodPost && r.URL.Path == "/me/sendMail":
			if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
				t.Fatalf("Content-Type = %q, want text/plain", got)
			}
			raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(readRequestBody(t, r)))
			if err != nil {
				t.Fatalf("send MIME is not base64: %v", err)
			}
			assertMIMEHeaders(t, raw, "Graph send", "<sent@example.com>", "hidden@example.com")
			sawSend = true
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages":
			filter, _ := url.QueryUnescape(r.URL.Query().Get("$filter"))
			if !strings.Contains(filter, "internetMessageId eq '<sent@example.com>'") {
				t.Fatalf("$filter = %q, want sent message id lookup", filter)
			}
			sawReconcile = true
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]string{{"id": "graph-sent-1"}}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	manager = auth.NewManager(&auth.Config{MicrosoftClient: &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: server.URL + "/token"}}}, db)
	previousGraphBase := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousGraphBase })

	to, _ := message.ParseAddressList("recipient@example.com")
	bcc, _ := message.ParseAddressList("hidden@example.com")
	h := &Handler{db: db, auth: manager, blobStore: store.NewBlobStore(filepath.Join(t.TempDir(), "blobs")), syncer: mailpkg.NewSyncOrchestrator(db, nil, nil, nil)}
	handled, status, errText := h.sendOutlookGraphMessage(ctx, &models.AccountConfig{AccountID: "acc", Provider: providers.ProviderOutlook}, &message.OutgoingMessage{
		FromEmail: "user@example.com",
		To:        to,
		Bcc:       bcc,
		Subject:   "Graph send",
		TextBody:  "Send body",
		MessageID: "<sent@example.com>",
		Date:      time.Now(),
	}, "")
	if !handled || status != "sent" || errText != "" {
		t.Fatalf("sendOutlookGraphMessage() = handled %v status %q err %q", handled, status, errText)
	}
	if !sawSend {
		t.Fatal("Graph sendMail was not observed")
	}
	if !sawReconcile {
		t.Fatal("Graph sent reconciliation was not observed")
	}
	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<sent@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}
	var providerID string
	if err := db.Read().QueryRowContext(ctx, `SELECT COALESCE(remote_message_id, '') FROM messages WHERE id = ?`, msgID).Scan(&providerID); err != nil {
		t.Fatalf("query provider id: %v", err)
	}
	if providerID != "graph-sent-1" {
		t.Fatalf("remote_message_id = %q, want graph-sent-1", providerID)
	}
}

func readRequestBody(t *testing.T, r *http.Request) string {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	return string(raw)
}
