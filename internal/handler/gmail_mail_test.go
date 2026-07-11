package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

func newGmailAPITestHandler(t *testing.T, ctx context.Context) (*Handler, *storage.DB) {
	t.Helper()
	t.Setenv("GOFER_GMAIL_API_SYNC", "1")
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
		VALUES ('acc', 'default', ?, 'google-subject', 'user@example.com')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	expires := time.Now().Add(time.Hour)
	manager := auth.NewManager(&auth.Config{}, db)
	if err := manager.UpsertOAuthAccount(ctx, "default", providers.OAuthGoogle, "google-subject", "gmail-token", "refresh-token", "Bearer", &expires, "https://mail.google.com/"); err != nil {
		t.Fatalf("UpsertOAuthAccount() error = %v", err)
	}
	return &Handler{
		db:        db,
		auth:      manager,
		blobStore: store.NewBlobStore(filepath.Join(t.TempDir(), "blobs")),
		syncer:    mailpkg.NewSyncOrchestrator(db, nil, nil, nil),
	}, db
}

func seedGmailAPIMessage(t *testing.T, ctx context.Context, db *storage.DB, folders []storage.UpsertFolderInput) int64 {
	t.Helper()
	if err := db.UpsertFolders(ctx, folders); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	ids, err := db.UpsertProviderSyncMessages(ctx, []storage.ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          folders[0].ID,
		ProviderMessageID: "gmail-msg-1",
		InternetMessageID: "<gmail-handler@example.com>",
		Subject:           "Gmail handler",
		FromEmail:         "sender@example.com",
		DateSent:          time.Now(),
		DateReceived:      time.Now(),
		IsRead:            true,
	}})
	if err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
	msgID := ids["gmail-msg-1"]
	if msgID == 0 {
		t.Fatal("provider message was not inserted")
	}
	return msgID
}

func TestGmailAPIMutationUsesProviderMessageIDAndLabels(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	msgID := seedGmailAPIMessage(t, ctx, db, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_projects", AccountID: "acc", RemoteID: "Projects", ProviderRemoteID: "Label_Projects", Name: "Projects", Role: "custom", Selectable: true},
	})

	var sawUnread bool
	var sawMove bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/users/me/messages/gmail-msg-1/modify" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		var payload map[string][]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode modify payload: %v", err)
		}
		switch {
		case containsString(payload["addLabelIds"], "UNREAD"):
			sawUnread = true
		case containsString(payload["addLabelIds"], "Label_Projects"):
			if !containsString(payload["removeLabelIds"], "INBOX") {
				t.Fatalf("move payload = %#v, want INBOX removed", payload)
			}
			sawMove = true
		default:
			t.Fatalf("unexpected modify payload: %#v", payload)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	previousBase := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBase })

	if err := db.SetMessageReadAndQueue(ctx, msgID, false); err != nil {
		t.Fatalf("SetMessageReadAndQueue() error = %v", err)
	}
	h.runDueMessageMutations(ctx)
	if err := db.MoveMessageAndQueue(ctx, msgID, "acc_inbox", "acc_projects"); err != nil {
		t.Fatalf("MoveMessageAndQueue() error = %v", err)
	}
	h.runDueMessageMutations(ctx)

	if !sawUnread {
		t.Fatal("Gmail unread mutation was not observed")
	}
	if !sawMove {
		t.Fatal("Gmail label move mutation was not observed")
	}
}

func TestGmailAPIBodyFetchUsesProviderMessageID(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	msgID := seedGmailAPIMessage(t, ctx, db, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
	})

	const rawMIME = "From: Sender <sender@example.com>\r\nSubject: Gmail body\r\n\r\nGmail body text\r\n"
	var sawFetch bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if r.Method != http.MethodGet || r.URL.Path != "/users/me/messages/gmail-msg-1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		if got := r.URL.Query().Get("format"); got != "raw" {
			t.Fatalf("format = %q, want raw", got)
		}
		sawFetch = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id":  "gmail-msg-1",
			"raw": base64.RawURLEncoding.EncodeToString([]byte(rawMIME)),
		})
	}))
	defer server.Close()
	previousBase := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBase })

	info, err := db.GetMessageFetchInfo(ctx, msgID)
	if err != nil || info == nil {
		t.Fatalf("GetMessageFetchInfo() = %#v, %v", info, err)
	}
	body, err := h.fetchBodyRemote(ctx, msgID, info)
	if err != nil {
		t.Fatalf("fetchBodyRemote() error = %v", err)
	}
	if !sawFetch {
		t.Fatal("Gmail raw body fetch was not observed")
	}
	if string(body) != rawMIME {
		t.Fatalf("body = %q, want %q", string(body), rawMIME)
	}
}

func TestGmailAPIAttachmentFetchMaterializesProviderAttachment(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	msgID := seedGmailAPIMessage(t, ctx, db, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
	})
	if err := db.ReplaceAttachments(ctx, msgID, []storage.AttachmentRow{{
		Filename:         "gmail.txt",
		ContentType:      "text/plain",
		SizeBytes:        21,
		ProviderRemoteID: "gmail-attachment-1",
	}}); err != nil {
		t.Fatalf("ReplaceAttachments() error = %v", err)
	}
	var attID int64
	if err := db.Read().QueryRowContext(ctx, `SELECT id FROM attachments WHERE message_id = ?`, msgID).Scan(&attID); err != nil {
		t.Fatalf("query attachment id: %v", err)
	}

	const attachmentBody = "gmail attachment data"
	var sawFetch bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if r.Method != http.MethodGet || r.URL.Path != "/users/me/messages/gmail-msg-1/attachments/gmail-attachment-1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		sawFetch = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": base64.RawURLEncoding.EncodeToString([]byte(attachmentBody)),
			"size": len(attachmentBody),
		})
	}))
	defer server.Close()
	previousBase := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBase })

	info, err := db.GetAttachmentFetchInfo(ctx, attID)
	if err != nil || info == nil {
		t.Fatalf("GetAttachmentFetchInfo() = %#v, %v", info, err)
	}
	path, err := h.ensureAttachmentStorage(ctx, info)
	if err != nil {
		t.Fatalf("ensureAttachmentStorage() error = %v", err)
	}
	if !sawFetch {
		t.Fatal("Gmail attachment fetch was not observed")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if string(data) != attachmentBody {
		t.Fatalf("attachment body = %q, want %q", string(data), attachmentBody)
	}
}

func TestSaveGmailAPIDraftCreatesMIMEDraftAndCachesProviderMessageID(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{{ID: "acc_drafts", AccountID: "acc", RemoteID: "[Gmail]/Drafts", ProviderRemoteID: "DRAFT", Name: "Drafts", Role: "drafts", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	msgID, err := db.SaveDraftMessage(ctx, storage.DraftMessageInput{
		AccountID:         "acc",
		FolderID:          "acc_drafts",
		InternetMessageID: "<gmail-draft@example.com>",
		Subject:           "Gmail draft",
		FromEmail:         "user@example.com",
		Date:              time.Now(),
	})
	if err != nil {
		t.Fatalf("SaveDraftMessage() error = %v", err)
	}

	var sawDraftCreate bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/users/me/drafts" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		var payload struct {
			Message struct {
				Raw string `json:"raw"`
			} `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode draft payload: %v", err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(payload.Message.Raw)
		if err != nil {
			t.Fatalf("draft MIME is not raw base64url: %v", err)
		}
		assertMIMEHeaders(t, raw, "Gmail draft", "<gmail-draft@example.com>", "hidden@example.com")
		sawDraftCreate = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gmail-draft-1",
			"message": map[string]string{
				"id": "gmail-draft-message-1",
			},
		})
	}))
	defer server.Close()
	previousBase := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBase })

	to, _ := message.ParseAddressList("recipient@example.com")
	bcc, _ := message.ParseAddressList("hidden@example.com")
	err = h.saveGmailAPIDraft(ctx, "acc", msgID, &message.OutgoingMessage{
		FromEmail: "user@example.com",
		To:        to,
		Bcc:       bcc,
		Subject:   "Gmail draft",
		TextBody:  "Draft body",
		MessageID: "<gmail-draft@example.com>",
		Date:      time.Now(),
	})
	if err != nil {
		t.Fatalf("saveGmailAPIDraft() error = %v", err)
	}
	if !sawDraftCreate {
		t.Fatal("Gmail draft create was not observed")
	}
	var providerID string
	if err := db.Read().QueryRowContext(ctx, `SELECT COALESCE(remote_message_id, '') FROM messages WHERE id = ?`, msgID).Scan(&providerID); err != nil {
		t.Fatalf("query provider id: %v", err)
	}
	if providerID != "gmail-draft-message-1" {
		t.Fatalf("remote_message_id = %q, want gmail-draft-message-1", providerID)
	}
}

func TestSendGmailAPIMessageUsesRawMIMEAndCachesSentID(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{{ID: "acc_sent", AccountID: "acc", RemoteID: "[Gmail]/Sent Mail", ProviderRemoteID: "SENT", Name: "Sent", Role: "sent", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	var sawSend bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/users/me/messages/send" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode send payload: %v", err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(payload["raw"])
		if err != nil {
			t.Fatalf("send MIME is not raw base64url: %v", err)
		}
		assertMIMEHeaders(t, raw, "Gmail send", "<gmail-sent@example.com>", "hidden@example.com")
		sawSend = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "gmail-sent-1"})
	}))
	defer server.Close()
	previousBase := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBase })

	to, _ := message.ParseAddressList("recipient@example.com")
	bcc, _ := message.ParseAddressList("hidden@example.com")
	handled, status, errText := h.sendGmailAPIMessage(ctx, &models.AccountConfig{AccountID: "acc", Provider: providers.ProviderGmail}, &message.OutgoingMessage{
		FromEmail: "user@example.com",
		To:        to,
		Bcc:       bcc,
		Subject:   "Gmail send",
		TextBody:  "Send body",
		MessageID: "<gmail-sent@example.com>",
		Date:      time.Now(),
	}, "")
	if !handled || status != "sent" || errText != "" {
		t.Fatalf("sendGmailAPIMessage() = handled %v status %q err %q", handled, status, errText)
	}
	if !sawSend {
		t.Fatal("Gmail messages.send was not observed")
	}
	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<gmail-sent@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}
	var providerID string
	if err := db.Read().QueryRowContext(ctx, `SELECT COALESCE(remote_message_id, '') FROM messages WHERE id = ?`, msgID).Scan(&providerID); err != nil {
		t.Fatalf("query provider id: %v", err)
	}
	if providerID != "gmail-sent-1" {
		t.Fatalf("remote_message_id = %q, want gmail-sent-1", providerID)
	}
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
