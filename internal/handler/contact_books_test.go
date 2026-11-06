package handler

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// Export can take only the contacts whose main location is one place, and
// an address book Gofer stops syncing says how it is used, then lets go of
// its contacts as asked.
func TestContactBookExportUsageAndRelease(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	if _, err := db.Write().Exec(`INSERT INTO account_contact_address_books(account_id,user_id,id,url) VALUES('acc','default','book-1','https://dav.test/book-1/')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveContact(ctx, "default", models.Contact{Name: "Lia Local", Email: "lia@example.com"}); err != nil {
		t.Fatal(err)
	}
	inBook, err := db.SaveContact(ctx, "default", models.Contact{Name: "Bram Book", Email: "bram@example.com", StoredIn: "book:book-1"})
	if err != nil {
		t.Fatal(err)
	}
	as := func(method, target, body string, id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetPathValue("id", id)
		req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "default"}))
		w := httptest.NewRecorder()
		switch {
		case strings.HasPrefix(target, "/api/contacts/export"):
			h.handleExportContacts(w, req)
		case strings.HasSuffix(target, "/usage"):
			h.handleContactBookUsage(w, req)
		default:
			h.handleReleaseContactBook(w, req)
		}
		return w
	}
	export := as("GET", "/api/contacts/export?stored_in="+url.QueryEscape("book:book-1"), "", "")
	if body := export.Body.String(); export.Code != 200 || strings.Count(body, "BEGIN:VCARD") != 1 || !strings.Contains(body, "Bram Book") {
		t.Fatalf("export of the book = %d %q", export.Code, body)
	}
	usage := as("GET", "/api/contacts/books/book-1/usage", "", "book-1")
	var counts map[string]int
	if err := json.Unmarshal(usage.Body.Bytes(), &counts); err != nil || counts["living"] != 1 || counts["only_there"] != 1 {
		t.Fatalf("usage = %d %s", usage.Code, usage.Body.String())
	}
	release := as("POST", "/api/contacts/books/book-1/release", "release=keep", "book-1")
	if release.Code != 200 || !strings.Contains(release.Body.String(), `"moved":1`) {
		t.Fatalf("release = %d %s", release.Code, release.Body.String())
	}
	if contact, err := db.GetContact(ctx, "default", inBook.ID); err != nil || contact == nil || contact.StoredIn != "local" {
		t.Fatalf("kept contact = %+v, %v", contact, err)
	}
	if bad := as("POST", "/api/contacts/books/book-1/release", "release=whatever", "book-1"); bad.Code != 400 {
		t.Fatalf("unknown release = %d", bad.Code)
	}
}
