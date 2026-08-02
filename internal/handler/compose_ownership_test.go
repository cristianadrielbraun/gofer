package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func seedOwnedDraft(t *testing.T, db *storage.DB) int64 {
	t.Helper()
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{
		{ID: "victim-drafts", AccountID: "victim-account", RemoteID: "Drafts", Name: "Drafts", Role: "drafts", Selectable: true},
		{ID: "attacker-drafts", AccountID: "attacker-account", RemoteID: "Drafts", Name: "Drafts", Role: "drafts", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	messageID, err := db.SaveDraftMessage(t.Context(), storage.DraftMessageInput{
		AccountID:         "victim-account",
		FolderID:          "victim-drafts",
		InternetMessageID: "<private-draft@example.com>",
		Subject:           "Private draft",
		FromEmail:         "owner@example.com",
		Date:              time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("SaveDraftMessage() error = %v", err)
	}
	return messageID
}

func TestDraftReadAndDeleteRejectForeignUsersAndAdmins(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	messageID := seedOwnedDraft(t, db)
	id := strconv.FormatInt(messageID, 10)

	for _, test := range []struct {
		name    string
		handle  func(http.ResponseWriter, *http.Request)
		method  string
		isAdmin bool
	}{
		{name: "read as user", handle: h.handleGetDraft, method: http.MethodGet},
		{name: "read as admin", handle: h.handleGetDraft, method: http.MethodGet, isAdmin: true},
		{name: "delete as user", handle: h.handleDeleteDraft, method: http.MethodDelete},
		{name: "delete as admin", handle: h.handleDeleteDraft, method: http.MethodDelete, isAdmin: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, "/api/drafts/"+id, nil)
			req.SetPathValue("id", id)
			rec := httptest.NewRecorder()
			test.handle(rec, attackerRequestWithAdmin(req, test.isAdmin))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d body = %q, want 404", rec.Code, rec.Body.String())
			}
		})
	}

	email, err := db.GetEmailByID(t.Context(), id)
	if err != nil || email == nil || !email.IsDraft {
		t.Fatalf("foreign delete changed draft: email = %#v, err = %v", email, err)
	}
	var operations int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM imap_draft_operations WHERE account_id = 'victim-account'`).Scan(&operations); err != nil {
		t.Fatalf("count draft operations: %v", err)
	}
	if operations != 0 {
		t.Fatalf("foreign delete queued %d provider operations, want 0", operations)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/drafts/"+id, nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.handleGetDraft(rec, ownerRequest(req))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Private draft") {
		t.Fatalf("owner read status = %d body = %q", rec.Code, rec.Body.String())
	}
}

func TestComposeAttachmentsAreScopedToUploader(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	seedOwnedDraft(t, db)
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...)
	id, path, err := h.blobStore.StoreComposeAttachment(t.Context(), "owner", "private.png", bytes.NewReader(png))
	if err != nil {
		t.Fatalf("StoreComposeAttachment() error = %v", err)
	}

	preview := func(userOwner bool, isAdmin bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/compose/attachments/"+id+"/preview", nil)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		if userOwner {
			h.handleComposeAttachmentPreview(rec, ownerRequest(req))
		} else {
			h.handleComposeAttachmentPreview(rec, attackerRequestWithAdmin(req, isAdmin))
		}
		return rec
	}
	if rec := preview(false, false); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign preview status = %d body = %q, want 404", rec.Code, rec.Body.String())
	}
	if rec := preview(false, true); rec.Code != http.StatusNotFound {
		t.Fatalf("admin preview status = %d body = %q, want 404", rec.Code, rec.Body.String())
	}
	if rec := preview(true, false); rec.Code != http.StatusOK {
		t.Fatalf("owner preview status = %d body = %q", rec.Code, rec.Body.String())
	}

	form := url.Values{
		"account_id":          {"attacker-account"},
		"attachment_id":       {id},
		"attachment_filename": {"private.png"},
	}
	req := httptest.NewRequest(http.MethodPost, "/compose/draft", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.handleComposeDraft(rec, attackerRequest(req))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign attachment reuse status = %d body = %q, want 404", rec.Code, rec.Body.String())
	}
	var attackerDrafts int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE account_id = 'attacker-account'`).Scan(&attackerDrafts); err != nil {
		t.Fatalf("count attacker drafts: %v", err)
	}
	if attackerDrafts != 0 {
		t.Fatalf("foreign attachment reuse saved %d drafts, want 0", attackerDrafts)
	}

	req = httptest.NewRequest(http.MethodDelete, "/compose/attachments/"+id, nil)
	req.SetPathValue("id", id)
	rec = httptest.NewRecorder()
	h.handleComposeAttachmentDelete(rec, attackerRequestWithAdmin(req, true))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign delete status = %d body = %q, want 404", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("foreign delete removed owner attachment: %v", err)
	}

	req = httptest.NewRequest(http.MethodDelete, "/compose/attachments/"+id, nil)
	req.SetPathValue("id", id)
	rec = httptest.NewRecorder()
	h.handleComposeAttachmentDelete(rec, ownerRequest(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner delete status = %d body = %q", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("owner attachment still exists: %v", err)
	}
}
