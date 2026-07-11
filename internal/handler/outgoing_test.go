package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

func TestHandleComposePersistsMIMEBeforeReturningAccepted(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	form := url.Values{
		"account_id": {"victim-account"},
		"to":         {"recipient@example.com"},
		"bcc":        {"hidden@example.com"},
		"subject":    {"Durable send"},
		"body":       {"This body must survive a restart."},
	}
	req := httptest.NewRequest(http.MethodPost, "/compose", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "owner", Email: "owner@example.com"}))
	rec := httptest.NewRecorder()

	h.handleCompose(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %q, want 202", rec.Code, rec.Body.String())
	}
	var response map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["send_id"] == "" {
		t.Fatalf("response = %#v, want durable send id", response)
	}
	queued, err := db.GetOutgoingSend(t.Context(), response["send_id"])
	if err != nil {
		t.Fatalf("GetOutgoingSend() error = %v", err)
	}
	if queued.Status != storage.OutgoingSendPending || queued.AccountID != "victim-account" || queued.Transport != storage.OutgoingTransportSMTP {
		t.Fatalf("queued send = %#v", queued)
	}
	if !strings.Contains(string(queued.MIMEData), "Subject: Durable send") || !strings.Contains(string(queued.MIMEData), "This body must survive a restart.") || !strings.Contains(string(queued.MIMEData), "Message-ID: <") {
		t.Fatalf("queued MIME = %q", string(queued.MIMEData))
	}
	if strings.Contains(string(queued.MIMEData), "\r\nBcc:") {
		t.Fatalf("SMTP MIME leaked Bcc header: %q", string(queued.MIMEData))
	}
	if len(queued.EnvelopeRecipients) != 2 || queued.EnvelopeRecipients[0] != "recipient@example.com" || queued.EnvelopeRecipients[1] != "hidden@example.com" {
		t.Fatalf("envelope recipients = %#v", queued.EnvelopeRecipients)
	}
}

func TestOutgoingWorkerDeliversStoredGmailSnapshot(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address)
		VALUES ('acc', 'default', ?, 'google-subject', 'user@example.com')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{{ID: "acc_sent", AccountID: "acc", RemoteID: "Sent", ProviderRemoteID: "SENT", Name: "Sent", Role: "sent", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	expires := time.Now().Add(time.Hour)
	authManager := auth.NewManager(&auth.Config{}, db)
	if err := authManager.UpsertOAuthAccount(ctx, "default", providers.OAuthGoogle, "google-subject", "gmail-token", "refresh-token", "Bearer", &expires, "https://mail.google.com/"); err != nil {
		t.Fatalf("UpsertOAuthAccount() error = %v", err)
	}
	accountStore, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewAccountStore() error = %v", err)
	}
	h := New(db, accountStore, mailpkg.NewSyncOrchestrator(db, accountStore, nil, nil), store.NewBlobStore(filepath.Join(t.TempDir(), "blobs")), authManager, "")

	var delivered []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/users/me/messages/send" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		delivered, err = base64.RawURLEncoding.DecodeString(payload["raw"])
		if err != nil {
			t.Fatalf("decode MIME: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gmail-sent-id"}`))
	}))
	defer server.Close()
	previousBase := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBase })

	to, _ := message.ParseAddressList("recipient@example.com")
	msg := &message.OutgoingMessage{
		FromEmail: "user@example.com", To: to, Subject: "Stored snapshot", TextBody: "Original body",
		MessageID: "<durable@example.com>", Date: time.Now().UTC(),
	}
	queued, err := h.queueOutgoingMessage(ctx, "acc", 0, "", msg, time.Now().Add(-time.Second), false)
	if err != nil {
		t.Fatalf("queueOutgoingMessage() error = %v", err)
	}
	msg.Subject = "Changed after queueing"
	msg.TextBody = "Changed body"

	h.runDueOutgoingSends(ctx)

	if !strings.Contains(string(delivered), "Subject: Stored snapshot") || !strings.Contains(string(delivered), "Original body") || strings.Contains(string(delivered), "Changed after queueing") {
		t.Fatalf("delivered MIME = %q", string(delivered))
	}
	completed, err := db.GetOutgoingSend(ctx, queued.ID)
	if err != nil || completed.Status != storage.OutgoingSendSent || completed.SentMessageID != "<durable@example.com>" {
		t.Fatalf("completed send = %#v, %v", completed, err)
	}
	localID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<durable@example.com>")
	if err != nil || localID == 0 {
		t.Fatalf("sent message local id = %d, %v", localID, err)
	}
}
