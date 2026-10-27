package handler

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// The administrator limits the servers and domains mailboxes use: the page
// lists the mailboxes outside the lists, those stop connecting, and a person
// the lists don't apply to connects anywhere.
func TestOwnedAdminMailAllowlist(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='bob'; UPDATE sessions SET step_up_at=CURRENT_TIMESTAMP,step_up_method='totp' WHERE user_id='administrator'`); err != nil {
		t.Fatal(err)
	}
	hosts := map[string][2]string{"alice": {"alice@example.com", "imap.mail.example.com"}, "bob": {"bob@example.com", "imap.elsewhere.test"}}
	for owner, mailbox := range hosts {
		if err := f.h.withUserDB(t.Context(), owner, func(db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE accounts SET provider='imap',email_address=?,imap_host=?,imap_port=993,imap_tls_mode='tls',smtp_host=?,smtp_port=465,smtp_tls_mode='tls'`, mailbox[0], mailbox[1], mailbox[1])
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	connects := func(owner string) error {
		t.Helper()
		return f.h.userAccounts.WithAccountForUser(t.Context(), owner, f.accounts[owner].ID, func(local *config.AccountStore, _ *storage.DB) error {
			_, err := local.GetConfig(t.Context(), f.accounts[owner].ID)
			return err
		})
	}
	post := func(path string, form url.Values) {
		t.Helper()
		page := f.request("/admin/security")
		if page.Code != 200 {
			t.Fatal(page.Code, page.Body.String())
		}
		response := ownedSecurityPost(t, f, page.Body.String(), path, form)
		if response.Code != 303 || strings.Contains(response.Header().Get("Location"), "error=") {
			t.Fatal(path, response.Code, response.Header())
		}
	}

	page := f.request("/admin/security")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Allowed mail servers and domains") || !strings.Contains(page.Body.String(), "Nothing is limited.") {
		t.Fatal("allowed mail page", page.Code, page.Body.String())
	}
	post("/admin/security/allowlist/servers", url.Values{"value": {"*.Example.com"}})
	// Alice's contacts come from a CardDAV server on this address.
	post("/admin/security/allowlist/servers", url.Values{"value": {"127.0.0.1"}})
	page = f.request("/admin/security")
	body := page.Body.String()
	if !strings.Contains(body, "*.example.com") || !strings.Contains(body, "imap.elsewhere.test isn&#39;t an allowed server") || strings.Contains(body, "alice@example.com") {
		i := strings.LastIndex(body, "Mailboxes outside the lists")
		t.Fatal("refused mailboxes", body[i:i+1500])
	}
	if err := connects("alice"); err != nil {
		t.Fatal("allowed mailbox refused:", err)
	}
	if err := connects("bob"); !storage.IsMailAllowlistError(err) {
		t.Fatal("mailbox outside the lists connected:", err)
	}

	post("/admin/security/allowlist/exempt", url.Values{"user_id": {"bob"}, "exempt": {"true"}})
	if err := connects("bob"); err != nil {
		t.Fatal("exempt person refused:", err)
	}
	if body := f.request("/admin/security").Body.String(); !strings.Contains(body, "Every mailbox is within the lists.") {
		t.Fatal("exempt person's mailbox still listed", body)
	}

	post("/admin/security/allowlist/domains", url.Values{"value": {"@example.org"}})
	if err := connects("alice"); !storage.IsMailAllowlistError(err) || !strings.Contains(err.Error(), "@example.com") {
		t.Fatal("address outside the domains connected:", err)
	}

	page = f.request("/admin/security")
	response := ownedSecurityPost(t, f, page.Body.String(), "/admin/security/allowlist/servers", url.Values{"value": {"imap.example.com:993"}})
	if response.Code != 303 || !strings.Contains(response.Header().Get("Location"), "error=") {
		t.Fatal("server with a port accepted", response.Code, response.Header())
	}
	entries, err := f.system.ListMailAllowlistEntries(t.Context())
	if err != nil || len(entries) != 3 {
		t.Fatal(entries, err)
	}
	for _, entry := range entries {
		post("/admin/security/allowlist/entries/"+entry.ID+"/delete", url.Values{})
	}
	if err := connects("alice"); err != nil {
		t.Fatal("refused with empty lists:", err)
	}
}

// CalDAV and CardDAV requests, redirects included, reach only the allowed
// servers, unless they are made for a person the lists don't apply to.
func TestDAVRequestsKeepToTheAllowedServers(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "allowlist.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('ana','ana','ana'),('ben','ben','ben')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddMailAllowlistEntry(t.Context(), storage.MailAllowlistServer, "dav.example.test", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMailAllowlistExempt(t.Context(), "ben", true, "admin"); err != nil {
		t.Fatal(err)
	}
	previousTransport, previousPolicy := davHTTPTransport, davServerPolicy.Load()
	t.Cleanup(func() { davHTTPTransport = previousTransport; davServerPolicy.Store(previousPolicy) })
	davHTTPTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}
		if req.URL.Path == "/moved" {
			response.StatusCode = http.StatusTemporaryRedirect
			response.Header.Set("Location", "https://other.example.test/dav/")
		}
		return response, nil
	})
	setDAVServerPolicy(davServerAllowed(db))
	get := func(ctx context.Context, address string) error {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		resp, err := (&http.Client{Transport: cardDAVHTTPTransport}).Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	ana := withMailAllowlistOwner(t.Context(), "ana")
	if err := get(ana, "https://dav.example.test/carddav/"); err != nil {
		t.Fatal("allowed server refused:", err)
	}
	if err := get(ana, "https://dav.example.test/moved"); err == nil || !strings.Contains(err.Error(), "other.example.test") {
		t.Fatal("redirect to a server outside the list followed:", err)
	}
	if err := get(t.Context(), "https://other.example.test/dav/"); err == nil {
		t.Fatal("request made for nobody reached a server outside the list")
	}
	if err := get(withMailAllowlistOwner(t.Context(), "ben"), "https://other.example.test/dav/"); err != nil {
		t.Fatal("exempt person refused:", err)
	}
}
