package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

func newAccountOwnershipTestHandler(t *testing.T) (*Handler, *storage.DB) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users (id, email, name)
		VALUES ('owner', 'owner@example.com', 'Owner'),
		       ('attacker', 'attacker@example.com', 'Attacker')`); err != nil {
		t.Fatalf("insert users: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (
			id, user_id, provider, email_address, display_name,
			imap_host, imap_port, imap_tls_mode, smtp_host, smtp_port, smtp_tls_mode, username
		) VALUES
		(
			'victim-account', 'owner', 'imap', 'owner@example.com', 'Owner',
			'127.0.0.1', 1, 'tls', '127.0.0.1', 1, 'tls', 'owner@example.com'
		),
		(
			'attacker-account', 'attacker', 'imap', 'attacker@example.com', 'Attacker',
			'127.0.0.1', 1, 'tls', '127.0.0.1', 1, 'tls', 'attacker@example.com'
		)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	accountStore, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("config.NewAccountStore() error = %v", err)
	}
	blobStore := store.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	return New(db, accountStore, nil, blobStore, nil, ""), db
}

func attackerRequest(req *http.Request) *http.Request {
	return attackerRequestWithAdmin(req, false)
}

func attackerRequestWithAdmin(req *http.Request, isAdmin bool) *http.Request {
	ctx := auth.ContextWithUser(req.Context(), &auth.User{ID: "attacker", Email: "attacker@example.com", IsAdmin: isAdmin})
	return req.WithContext(ctx)
}

func ownerRequest(req *http.Request) *http.Request {
	ctx := auth.ContextWithUser(req.Context(), &auth.User{ID: "owner", Email: "owner@example.com", IsAdmin: true})
	return req.WithContext(ctx)
}

type victimMessageFixture struct {
	messageID    int64
	threadID     string
	attachmentID int64
	contentID    string
	assetName    string
}

func insertVictimReadableMessage(t *testing.T, h *Handler, db *storage.DB) victimMessageFixture {
	t.Helper()
	const (
		messageID = int64(101)
		threadID  = "victim-thread"
		contentID = "victim-inline"
	)
	ctx := t.Context()
	bodyPath := filepath.Join(t.TempDir(), "victim-body.html")
	if err := os.WriteFile(bodyPath, []byte("<p>victim body</p>"), 0o600); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO folders (id, account_id, remote_id, name, role)
		VALUES ('victim-inbox', 'victim-account', 'INBOX', 'Inbox', 'inbox')`); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO messages (
			id, account_id, internet_message_id, thread_id, subject, from_email,
			snippet, body_html_path, has_attachments
		) VALUES (?, 'victim-account', '<victim-secret@example.com>', ?, 'Victim secret',
		          'sender@example.com', 'victim preview', ?, 1)`,
		messageID, threadID, bodyPath,
	); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO message_folder_state (message_id, folder_id, remote_uid)
		VALUES (?, 'victim-inbox', 42)`, messageID,
	); err != nil {
		t.Fatalf("insert folder state: %v", err)
	}
	attachmentPath := filepath.Join(t.TempDir(), "victim-inline.png")
	if err := os.WriteFile(attachmentPath, []byte("victim attachment"), 0o600); err != nil {
		t.Fatalf("write attachment: %v", err)
	}
	result, err := db.Write().ExecContext(ctx, `
		INSERT INTO attachments (
			message_id, filename, content_type, content_id, size_bytes, storage_path, inline
		) VALUES (?, 'victim-inline.png', 'image/png', ?, 17, ?, 1)`,
		messageID, contentID, attachmentPath,
	)
	if err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
	attachmentID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("attachment id: %v", err)
	}
	assetPath, err := h.blobStore.StoreRemoteAsset("victim-account", messageID, "https://assets.example/victim.png", []byte("victim asset"))
	if err != nil {
		t.Fatalf("store remote asset: %v", err)
	}
	return victimMessageFixture{
		messageID:    messageID,
		threadID:     threadID,
		attachmentID: attachmentID,
		contentID:    contentID,
		assetName:    filepath.Base(assetPath),
	}
}

func insertVictimAttachment(t *testing.T, db *storage.DB) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Write().ExecContext(ctx, `
		INSERT OR IGNORE INTO messages (id, account_id, internet_message_id, subject, from_email)
		VALUES (1, 'victim-account', '<secret@example.com>', 'Secret', 'sender@example.com')`); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	path := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write attachment: %v", err)
	}
	result, err := db.Write().ExecContext(ctx, `
		INSERT INTO attachments (message_id, filename, content_type, size_bytes, storage_path)
		VALUES (1, 'secret.txt', 'text/plain', 6, ?)`, path)
	if err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
	attachmentID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("attachment id: %v", err)
	}
	return attachmentID
}

func TestHandleTestAccountRejectsForeignAccount(t *testing.T) {
	h, _ := newAccountOwnershipTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/victim-account/test", nil)
	req.SetPathValue("id", "victim-account")
	rec := httptest.NewRecorder()

	h.handleTestAccount(rec, attackerRequest(req))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %q, want 404", rec.Code, rec.Body.String())
	}
}

func TestHandleAccountDeletionStatusTracksOwnedAccountWithoutLeakingForeignState(t *testing.T) {
	h, _ := newAccountOwnershipTestHandler(t)
	if err := h.accountStore.MarkAccountDeleting(t.Context(), "victim-account"); err != nil {
		t.Fatalf("MarkAccountDeleting() error = %v", err)
	}

	ownerReq := httptest.NewRequest(http.MethodGet, "/api/accounts/victim-account/deletion-status", nil)
	ownerReq.SetPathValue("id", "victim-account")
	ownerReq = ownerReq.WithContext(auth.ContextWithUser(ownerReq.Context(), &auth.User{ID: "owner", Email: "owner@example.com"}))
	ownerRec := httptest.NewRecorder()
	h.handleAccountDeletionStatus(ownerRec, ownerReq)
	if ownerRec.Code != http.StatusOK || !strings.Contains(ownerRec.Body.String(), `"status":"deleting"`) {
		t.Fatalf("owner status = %d body = %q, want deleting", ownerRec.Code, ownerRec.Body.String())
	}

	foreignReq := httptest.NewRequest(http.MethodGet, "/api/accounts/victim-account/deletion-status", nil)
	foreignReq.SetPathValue("id", "victim-account")
	foreignRec := httptest.NewRecorder()
	h.handleAccountDeletionStatus(foreignRec, attackerRequest(foreignReq))
	if foreignRec.Code != http.StatusOK || !strings.Contains(foreignRec.Body.String(), `"status":"deleted"`) {
		t.Fatalf("foreign status = %d body = %q, want privacy-preserving deleted", foreignRec.Code, foreignRec.Body.String())
	}
}

func TestHandleComposeRejectsForeignAccount(t *testing.T) {
	h, _ := newAccountOwnershipTestHandler(t)
	form := url.Values{"account_id": {"victim-account"}, "to": {"recipient@example.com"}}
	req := httptest.NewRequest(http.MethodPost, "/compose", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	h.handleCompose(rec, attackerRequest(req))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %q, want 404", rec.Code, rec.Body.String())
	}
}

func TestHandleComposeRejectsForeignExistingAttachment(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	attachmentID := insertVictimAttachment(t, db)
	form := url.Values{
		"account_id":             {"attacker-account"},
		"to":                     {"recipient@example.com"},
		"existing_attachment_id": {strconv.FormatInt(attachmentID, 10)},
	}
	req := httptest.NewRequest(http.MethodPost, "/compose", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	h.handleCompose(rec, attackerRequest(req))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %q, want 404", rec.Code, rec.Body.String())
	}
}

func TestHandleAttachmentDownloadRejectsForeignAttachment(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	attachmentID := insertVictimAttachment(t, db)

	req := httptest.NewRequest(http.MethodGet, "/api/attachments/1", nil)
	req.SetPathValue("id", strconv.FormatInt(attachmentID, 10))
	rec := httptest.NewRecorder()

	h.handleAttachmentDownload(rec, attackerRequest(req))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %q, want 404", rec.Code, rec.Body.String())
	}
}

func TestPrivateMessageReadsRejectForeignUserAndAdmin(t *testing.T) {
	for _, isAdmin := range []bool{false, true} {
		role := "user"
		if isAdmin {
			role = "admin"
		}
		t.Run(role, func(t *testing.T) {
			h, db := newAccountOwnershipTestHandler(t)
			fixture := insertVictimReadableMessage(t, h, db)

			tests := []struct {
				name       string
				path       string
				pathValues map[string]string
				handle     http.HandlerFunc
			}{
				{
					name:       "email partial",
					path:       "/email/101",
					pathValues: map[string]string{"id": strconv.FormatInt(fixture.messageID, 10)},
					handle:     h.handleEmailPartial,
				},
				{
					name:       "email body",
					path:       "/email/101/body",
					pathValues: map[string]string{"id": strconv.FormatInt(fixture.messageID, 10)},
					handle:     h.handleEmailBody,
				},
				{
					name:       "translated email body",
					path:       "/email/101/body/translated",
					pathValues: map[string]string{"id": strconv.FormatInt(fixture.messageID, 10)},
					handle:     h.handleTranslatedEmailBody,
				},
				{
					name:       "thread subitems",
					path:       "/mail/thread/victim-thread/subitems",
					pathValues: map[string]string{"threadId": fixture.threadID},
					handle:     h.handleThreadSubItems,
				},
				{
					name:       "attachment download",
					path:       "/api/attachments/1/download",
					pathValues: map[string]string{"id": strconv.FormatInt(fixture.attachmentID, 10)},
					handle:     h.handleAttachmentDownload,
				},
				{
					name:       "attachment preview",
					path:       "/api/attachments/1/preview",
					pathValues: map[string]string{"id": strconv.FormatInt(fixture.attachmentID, 10)},
					handle:     h.handleAttachmentPreview,
				},
				{
					name: "inline content",
					path: "/api/inline-content/101/victim-inline",
					pathValues: map[string]string{
						"messageID": strconv.FormatInt(fixture.messageID, 10),
						"contentID": fixture.contentID,
					},
					handle: h.handleInlineContent,
				},
				{
					name: "remote asset",
					path: "/api/remote-assets/101/" + fixture.assetName,
					pathValues: map[string]string{
						"messageID": strconv.FormatInt(fixture.messageID, 10),
						"filename":  fixture.assetName,
					},
					handle: h.handleRemoteAsset,
				},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					req := httptest.NewRequest(http.MethodGet, tt.path, nil)
					for key, value := range tt.pathValues {
						req.SetPathValue(key, value)
					}
					rec := httptest.NewRecorder()

					tt.handle(rec, attackerRequestWithAdmin(req, isAdmin))

					if rec.Code != http.StatusNotFound {
						t.Fatalf("status = %d body = %q, want 404", rec.Code, rec.Body.String())
					}
					if strings.Contains(rec.Body.String(), "victim") {
						t.Fatalf("foreign response leaked victim content: %q", rec.Body.String())
					}
				})
			}
		})
	}
}

func TestPrivateMessageBodyAndRemoteAssetAllowOwner(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	fixture := insertVictimReadableMessage(t, h, db)

	bodyReq := httptest.NewRequest(http.MethodGet, "/email/101/body", nil)
	bodyReq.SetPathValue("id", strconv.FormatInt(fixture.messageID, 10))
	bodyRec := httptest.NewRecorder()
	h.handleEmailBody(bodyRec, ownerRequest(bodyReq))
	if bodyRec.Code != http.StatusOK || !strings.Contains(bodyRec.Body.String(), "victim body") {
		t.Fatalf("owner body status = %d body = %q, want owned body", bodyRec.Code, bodyRec.Body.String())
	}

	assetReq := httptest.NewRequest(http.MethodGet, "/api/remote-assets/101/"+fixture.assetName, nil)
	assetReq.SetPathValue("messageID", strconv.FormatInt(fixture.messageID, 10))
	assetReq.SetPathValue("filename", fixture.assetName)
	assetRec := httptest.NewRecorder()
	h.handleRemoteAsset(assetRec, ownerRequest(assetReq))
	if assetRec.Code != http.StatusOK || assetRec.Body.String() != "victim asset" {
		t.Fatalf("owner asset status = %d body = %q, want owned asset", assetRec.Code, assetRec.Body.String())
	}
	if cacheControl := assetRec.Header().Get("Cache-Control"); !strings.HasPrefix(cacheControl, "private,") {
		t.Fatalf("owner asset Cache-Control = %q, want private", cacheControl)
	}
}

func TestHandleRemoteAssetRejectsNonGeneratedFilename(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	fixture := insertVictimReadableMessage(t, h, db)

	req := httptest.NewRequest(http.MethodGet, "/api/remote-assets/101/%2e%2e%2fvictim-body.html", nil)
	req.SetPathValue("messageID", strconv.FormatInt(fixture.messageID, 10))
	req.SetPathValue("filename", "../victim-body.html")
	rec := httptest.NewRecorder()

	h.handleRemoteAsset(rec, ownerRequest(req))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %q, want 404", rec.Code, rec.Body.String())
	}
}

func TestVisibleMailListSelectionDoesNotInspectForeignMessage(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	fixture := insertVictimReadableMessage(t, h, db)
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO messages (id, account_id, thread_id, subject)
		VALUES (102, 'attacker-account', ?, 'Attacker message')`, fixture.threadID,
	); err != nil {
		t.Fatalf("insert attacker message: %v", err)
	}

	visible := []models.Email{{ID: "102", ThreadID: fixture.threadID}}
	got := h.visibleMailListSelectionID(t.Context(), "attacker", visible, strconv.FormatInt(fixture.messageID, 10))
	if got != strconv.FormatInt(fixture.messageID, 10) {
		t.Fatalf("visibleMailListSelectionID() = %q, want opaque foreign selection ID", got)
	}
}
