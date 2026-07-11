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

	goimap "github.com/emersion/go-imap/v2"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	imapclient "github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

type fakeSentCopyIMAPClient struct {
	appendResult    imapclient.AppendResult
	appendErr       error
	findUID         uint32
	findUIDValidity uint32
	findErr         error
	appendCalls     int
	findCalls       int
	deleteCalls     int
	mailbox         string
	raw             []byte
	flags           []goimap.Flag
	date            time.Time
	findHeader      string
	findValue       string
	deleteUIDs      []uint32
	deleteValidity  uint32
	deleteErr       error
}

func (c *fakeSentCopyIMAPClient) AppendMessage(_ context.Context, mailbox string, raw []byte, flags []goimap.Flag, date time.Time) (imapclient.AppendResult, error) {
	c.appendCalls++
	c.mailbox = mailbox
	c.raw = append([]byte(nil), raw...)
	c.flags = append([]goimap.Flag(nil), flags...)
	c.date = date
	return c.appendResult, c.appendErr
}

func (c *fakeSentCopyIMAPClient) FindUIDByMessageIDWithValidity(context.Context, string, string) (uint32, uint32, error) {
	c.findCalls++
	return c.findUID, c.findUIDValidity, c.findErr
}

func (c *fakeSentCopyIMAPClient) FindUIDByHeaderWithValidity(_ context.Context, _ string, headerName, headerValue string) (uint32, uint32, error) {
	c.findCalls++
	c.findHeader = headerName
	c.findValue = headerValue
	return c.findUID, c.findUIDValidity, c.findErr
}

func (c *fakeSentCopyIMAPClient) DeleteMessagesIfUIDValidity(_ context.Context, _ string, uids []uint32, expectedUIDValidity uint32) (bool, error) {
	c.deleteCalls++
	c.deleteUIDs = append([]uint32(nil), uids...)
	c.deleteValidity = expectedUIDValidity
	return false, c.deleteErr
}

func (c *fakeSentCopyIMAPClient) Close() error { return nil }

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
	if !strings.Contains(string(queued.MIMEData), "Subject: Durable send") || !strings.Contains(string(queued.MIMEData), "This body must survive a restart.") {
		t.Fatalf("queued MIME = %q", string(queued.MIMEData))
	}
	assertMIMEHeaders(t, queued.MIMEData, "Durable send", "", "")
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
	if err != nil || completed.Status != storage.OutgoingSendSent || completed.SentMessageID != "<durable@example.com>" || completed.SentCopyStatus != storage.SentCopyNotRequired {
		t.Fatalf("completed send = %#v, %v", completed, err)
	}
	localID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<durable@example.com>")
	if err != nil || localID == 0 {
		t.Fatalf("sent message local id = %d, %v", localID, err)
	}
}

func TestSentCopyWorkerAppendsExactMIMEAndLinksRemoteUID(t *testing.T) {
	h, db, queued, localID := seedPendingSentCopy(t)
	fake := &fakeSentCopyIMAPClient{appendResult: imapclient.AppendResult{UID: 77, UIDValidity: 123}}
	h.sentCopyIMAPFactory = func(context.Context, *models.AccountConfig, string) (sentCopyIMAPClient, error) {
		return fake, nil
	}

	h.runDueSentCopies(t.Context())

	if fake.appendCalls != 1 || fake.findCalls != 0 || fake.mailbox != "Sent" {
		t.Fatalf("fake IMAP calls append=%d find=%d mailbox=%q", fake.appendCalls, fake.findCalls, fake.mailbox)
	}
	if !strings.Contains(string(fake.raw), "Sent copy body") {
		t.Fatalf("appended MIME = %q", string(fake.raw))
	}
	assertMIMEHeaders(t, fake.raw, "Sent copy", "<sent-copy@example.com>", "")
	if len(fake.flags) != 1 || fake.flags[0] != goimap.FlagSeen {
		t.Fatalf("append flags = %#v, want Seen", fake.flags)
	}
	completed, err := db.GetOutgoingSend(t.Context(), queued.ID)
	if err != nil || completed.Status != storage.OutgoingSendSent || completed.SentCopyStatus != storage.SentCopyComplete || completed.SentCopyUID != 77 || len(completed.MIMEData) != 0 {
		t.Fatalf("completed Sent copy = %#v, %v", completed, err)
	}
	var remoteUID uint32
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT remote_uid FROM message_folder_state
		WHERE message_id = ? AND folder_id = 'victim-sent'`, localID).Scan(&remoteUID); err != nil {
		t.Fatalf("query linked remote UID: %v", err)
	}
	if remoteUID != 77 {
		t.Fatalf("linked remote UID = %d, want 77", remoteUID)
	}
}

func TestInterruptedSentCopySearchesBeforeAppendingAgain(t *testing.T) {
	h, db, queued, _ := seedPendingSentCopy(t)
	claimed, err := db.ClaimDueSentCopies(t.Context(), time.Now(), 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDueSentCopies() = %#v, %v", claimed, err)
	}
	if count, err := db.MarkInterruptedSentCopiesAmbiguous(t.Context(), "interrupted"); err != nil || count != 1 {
		t.Fatalf("MarkInterruptedSentCopiesAmbiguous() = %d, %v", count, err)
	}
	fake := &fakeSentCopyIMAPClient{findUID: 88}
	h.sentCopyIMAPFactory = func(context.Context, *models.AccountConfig, string) (sentCopyIMAPClient, error) {
		return fake, nil
	}

	h.runDueSentCopies(t.Context())

	if fake.findCalls != 1 || fake.appendCalls != 0 {
		t.Fatalf("ambiguous recovery find=%d append=%d, want search without APPEND", fake.findCalls, fake.appendCalls)
	}
	completed, err := db.GetOutgoingSend(t.Context(), queued.ID)
	if err != nil || completed.SentCopyStatus != storage.SentCopyComplete || completed.SentCopyUID != 88 {
		t.Fatalf("recovered Sent copy = %#v, %v", completed, err)
	}
}

func seedPendingSentCopy(t *testing.T) (*Handler, *storage.DB, storage.OutgoingSend, int64) {
	t.Helper()
	h, db := newAccountOwnershipTestHandler(t)
	h.blobStore = store.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	h.syncer = mailpkg.NewSyncOrchestrator(db, h.accountStore, h.blobStore, nil)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{
		ID: "victim-sent", AccountID: "victim-account", RemoteID: "Sent", Name: "Sent", Role: "sent", Selectable: true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	to, _ := message.ParseAddressList("recipient@example.com")
	msg := &message.OutgoingMessage{
		FromName: "Owner", FromEmail: "owner@example.com", To: to,
		Subject: "Sent copy", TextBody: "Sent copy body",
		MessageID: "<sent-copy@example.com>", Date: time.Now().UTC(),
	}
	queued, err := h.queueOutgoingMessage(t.Context(), "victim-account", 0, "", msg, time.Now().Add(-time.Second), false)
	if err != nil {
		t.Fatalf("queueOutgoingMessage() error = %v", err)
	}
	if sends, err := db.ClaimDueOutgoingSends(t.Context(), time.Now(), 1); err != nil || len(sends) != 1 {
		t.Fatalf("ClaimDueOutgoingSends() = %#v, %v", sends, err)
	}
	h.saveSentMessageSnapshot(t.Context(), "victim-account", msg, queued.MIMEData)
	if err := db.CompleteOutgoingSend(t.Context(), queued.ID, msg.MessageID, true); err != nil {
		t.Fatalf("CompleteOutgoingSend() error = %v", err)
	}
	localID, err := db.GetMessageLocalIDByInternetID(t.Context(), "victim-account", msg.MessageID)
	if err != nil || localID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", localID, err)
	}
	return h, db, queued, localID
}
