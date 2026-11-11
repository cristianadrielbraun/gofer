package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	gomail "github.com/emersion/go-message/mail"

	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

func queuedComposeMIME(t *testing.T, h *Handler, db *storage.DB, form url.Values) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handleCompose(rec, composeIdentityRequest(t, "/compose", form))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
	var response map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&response)
	queued, err := db.GetOutgoingSend(t.Context(), response["send_id"])
	if err != nil {
		t.Fatalf("GetOutgoingSend() error = %v", err)
	}
	return queued.MIMEData
}

func mimeContentType(t *testing.T, raw []byte) string {
	t.Helper()
	reader, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("read MIME: %v", err)
	}
	defer reader.Close()
	contentType, _, _ := reader.Header.ContentType()
	return contentType
}

func TestHandleComposeSendsPlainTextAlone(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	base := url.Values{"account_id": {"victim-account"}, "to": {"recipient@example.com"}, "subject": {"Lunch"}, "body": {"See you at noon"}}

	plain := url.Values{}
	for key, value := range base {
		plain[key] = value
	}
	plain.Set("compose_format", "plain")
	// A stale HTML field is ignored: plain text mode sends the text alone.
	plain.Set("html_body", "<p><b>See you at noon</b></p>")
	raw := queuedComposeMIME(t, h, db, plain)
	if contentType := mimeContentType(t, raw); contentType != "text/plain" {
		t.Fatalf("plain text Content-Type = %q, MIME %q", contentType, raw)
	}
	if bytes.Contains(bytes.ToLower(raw), []byte("text/html")) {
		t.Fatalf("plain text message has an HTML part: %q", raw)
	}

	rich := url.Values{}
	for key, value := range base {
		rich[key] = value
	}
	rich.Set("compose_format", "html")
	rich.Set("html_body", "<p><b>See you at noon</b></p>")
	if contentType := mimeContentType(t, queuedComposeMIME(t, h, db, rich)); contentType != "multipart/alternative" {
		t.Fatalf("HTML Content-Type = %q", contentType)
	}
}

func TestComposeDraftInPlainTextSavesNoHTML(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	h.blobStore = store.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	h.syncer = mailpkg.NewSyncOrchestrator(db, h.accountStore, h.blobStore, nil)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{
		ID: "victim-drafts", AccountID: "victim-account", RemoteID: "Drafts", Name: "Drafts", Role: "drafts", Selectable: true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	rec := httptest.NewRecorder()
	h.handleComposeDraft(rec, composeIdentityRequest(t, "/compose/draft", url.Values{
		"account_id": {"victim-account"}, "to": {"recipient@example.com"}, "subject": {"Plain draft"},
		"body": {"Notes for the trip"}, "html_body": {"<p>Notes for the trip</p>"}, "compose_format": {"plain"},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("draft status = %d body = %q", rec.Code, rec.Body.String())
	}
	var operationMIME []byte
	if err := db.Read().QueryRow(`SELECT mime_data FROM imap_draft_operations WHERE account_id = 'victim-account'`).Scan(&operationMIME); err != nil {
		t.Fatalf("queued draft: %v", err)
	}
	if contentType := mimeContentType(t, operationMIME); contentType != "text/plain" {
		t.Fatalf("draft Content-Type = %q", contentType)
	}

	// The draft comes back with text alone, which compose reopens as plain text.
	var localID int64
	if err := db.Read().QueryRow(`SELECT id FROM messages WHERE account_id = 'victim-account' AND subject = 'Plain draft'`).Scan(&localID); err != nil {
		t.Fatalf("draft row: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/drafts/"+strconv.FormatInt(localID, 10), nil)
	req.SetPathValue("id", strconv.FormatInt(localID, 10))
	rec = httptest.NewRecorder()
	h.handleGetDraft(rec, ownerRequest(req))
	var draft map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&draft); err != nil {
		t.Fatalf("decode draft: %v", err)
	}
	if html, _ := draft["html_body"].(string); strings.TrimSpace(html) != "" {
		t.Fatalf("draft html_body = %q", html)
	}
	if body, _ := draft["body"].(string); !strings.Contains(body, "Notes for the trip") {
		t.Fatalf("draft body = %q", body)
	}
}
