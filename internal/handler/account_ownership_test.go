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
	"github.com/cristianadrielbraun/gofer/internal/storage"
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
	return New(db, accountStore, nil, nil, nil, ""), db
}

func attackerRequest(req *http.Request) *http.Request {
	ctx := auth.ContextWithUser(req.Context(), &auth.User{ID: "attacker", Email: "attacker@example.com"})
	return req.WithContext(ctx)
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
