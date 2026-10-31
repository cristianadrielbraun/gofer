package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// assertEmailPrintDocument checks a body document asked for to print: it
// starts with the message's details, may open the print dialog and does so
// itself, and still runs only the reader's own scripts.
func assertEmailPrintDocument(t *testing.T, rec *httptest.ResponseRecorder, subject string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
	policy := assertOnlyNoncedScripts(t, rec)
	if !strings.HasPrefix(policy, "sandbox allow-scripts allow-modals allow-popups allow-popups-to-escape-sandbox;") || strings.Contains(policy, "allow-same-origin") {
		t.Fatalf("Content-Security-Policy = %q, want the reader's sandbox and the print dialog", policy)
	}
	body := rec.Body.String()
	header := strings.Index(body, "data-gofer-print-header")
	if header == -1 || !strings.Contains(body[header:], subject) || header > strings.Index(body, "</body>") && strings.Contains(body, "</body>") {
		t.Fatalf("print document has no details heading the body:\n%s", body)
	}
	if !strings.Contains(body, "window.print()") {
		t.Fatalf("print document doesn't open the print dialog:\n%s", body)
	}
}

func TestSharedEmailBodyPrints(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	insertVictimReadableMessage(t, h, db)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, ownerRequest(httptest.NewRequest(http.MethodGet, "/email/101/body?print=1&theme=light", nil)))
	assertEmailPrintDocument(t, rec, "Victim secret")
	if body := rec.Body.String(); !strings.Contains(body, "sender@example.com") || strings.Contains(body, "victim-inline.png") {
		t.Fatalf("details: sender missing or an image in the body listed as an attachment:\n%s", body)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, ownerRequest(httptest.NewRequest(http.MethodGet, "/email/101/body?theme=light", nil)))
	if body := rec.Body.String(); strings.Contains(body, "data-gofer-print-header") || strings.Contains(body, "window.print()") ||
		strings.Contains(rec.Header().Get("Content-Security-Policy"), "allow-modals") {
		t.Fatal("the reader's body document prints")
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, attackerRequest(httptest.NewRequest(http.MethodGet, "/email/101/body?print=1", nil)))
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "Victim secret") {
		t.Fatalf("someone else's message printed: %d %q", rec.Code, rec.Body.String())
	}
}

func TestOwnedEmailBodyPrints(t *testing.T) {
	f := newOwnedMessageContentFixture(t)
	rec := httptest.NewRecorder()
	f.h.handleUserEmailBody(rec, ownedContentRequest("alice", "/email/1/body?print=1&theme=light", ""))
	assertEmailPrintDocument(t, rec, "")
	if !strings.Contains(rec.Body.String(), "alice private body") {
		t.Fatalf("print document lost the message: %q", rec.Body.String())
	}
}

func TestInsertAfterBodyOpen(t *testing.T) {
	for doc, want := range map[string]string{
		`<html><head></head><BODY bgcolor="#fff"><p>x</p></BODY></html>`: `<html><head></head><BODY bgcolor="#fff">H<p>x</p></BODY></html>`,
		`<p>no body tag</p>`: `H<p>no body tag</p>`,
	} {
		if got := string(insertAfterBodyOpen([]byte(doc), []byte("H"))); got != want {
			t.Errorf("insertAfterBodyOpen(%q) = %q, want %q", doc, got, want)
		}
	}
}
