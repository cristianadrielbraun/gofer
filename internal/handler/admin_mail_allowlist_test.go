package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/policy"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// setTestMailList sets one of the Default policy's allowed mail lists.
func setTestMailList(t *testing.T, db *storage.DB, setting *policy.Setting, entries ...string) {
	t.Helper()
	inTestPolicyTx(t, db, func(ctx context.Context, tx *sql.Tx) error {
		current, err := policy.Get(ctx, tx, policy.DefaultID)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(entries)
		if err != nil {
			return err
		}
		_, err = policy.SetValues(ctx, tx, policy.DefaultID, current.Version, []policy.ValueChange{{Key: setting.Key, JSON: raw}}, "", time.Now())
		return err
	})
}

// exemptFromTestMailLists assigns someone a policy that sets every list to
// nothing, as exempting them does.
func exemptFromTestMailLists(t *testing.T, db *storage.DB, userID string) {
	t.Helper()
	inTestPolicyTx(t, db, func(ctx context.Context, tx *sql.Tx) error {
		created, err := policy.Create(ctx, tx, policy.NewPolicy{ID: policy.MailExemptionID, Name: policy.MailExemptionName, At: time.Now()})
		if err != nil {
			return err
		}
		var none []policy.ValueChange
		for _, setting := range []*policy.Setting{policy.MailAllowedServers, policy.MailAllowedDomains, policy.MailAllowedProviders} {
			none = append(none, policy.ValueChange{Key: setting.Key, JSON: json.RawMessage(`[]`)})
		}
		if _, err := policy.SetValues(ctx, tx, created.ID, created.Version, none, "", time.Now()); err != nil {
			return err
		}
		_, err = policy.Assign(ctx, tx, created.ID, []policy.Target{{Kind: policy.TargetUser, ID: userID}}, "", time.Now())
		return err
	})
}

func inTestPolicyTx(t *testing.T, db *storage.DB, change func(context.Context, *sql.Tx) error) {
	t.Helper()
	tx, err := db.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := change(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

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
		if err := f.h.withUserDB(davTestContext(t), owner, func(db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE accounts SET provider='imap',email_address=?,imap_host=?,imap_port=993,imap_tls_mode='tls',smtp_host=?,smtp_port=465,smtp_tls_mode='tls'`, mailbox[0], mailbox[1], mailbox[1])
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	connects := func(owner string) error {
		t.Helper()
		return f.h.userAccounts.WithAccountForUser(davTestContext(t), owner, f.accounts[owner].ID, func(local *config.AccountStore, _ *storage.DB) error {
			_, err := local.GetConfig(davTestContext(t), f.accounts[owner].ID)
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
		if response.Code != 303 || adminFlashFailed(t, response) {
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
	if response.Code != 303 || !adminFlashFailed(t, response) {
		t.Fatal("server with a port accepted", response.Code, response.Header())
	}
	lists, err := policy.DefaultMailAllowlist(davTestContext(t), f.system.Read())
	if err != nil || len(lists.Servers) != 2 || len(lists.Domains) != 1 {
		t.Fatal(lists, err)
	}
	for _, server := range lists.Servers {
		post("/admin/security/allowlist/servers/remove", url.Values{"value": {server}})
	}
	post("/admin/security/allowlist/domains/remove", url.Values{"value": {"example.org"}})
	if err := connects("alice"); err != nil {
		t.Fatal("refused with empty lists:", err)
	}
	// Each change is a policy change: the Default policy's lists, and the
	// exemption policy made for Bob.
	var changed, created, assigned int
	if err := f.system.Read().QueryRow(`SELECT
		(SELECT COUNT(*) FROM audit_events WHERE action = 'policy.settings_changed' AND target_id = 'default'),
		(SELECT COUNT(*) FROM audit_events WHERE action = 'policy.created' AND target_id = ?),
		(SELECT COUNT(*) FROM audit_events WHERE action = 'policy.assigned' AND target_id = ?)`, policy.MailExemptionID, policy.MailExemptionID).Scan(&changed, &created, &assigned); err != nil || changed != 6 || created != 1 || assigned != 1 {
		t.Fatalf("recorded = %d settings changes, %d created, %d assigned, %v", changed, created, assigned, err)
	}
	if exempt, err := policy.Assignments(davTestContext(t), f.system.Read(), policy.MailExemptionID); err != nil || len(exempt) != 1 || exempt[0].ID != "bob" {
		t.Fatalf("exempt = %#v, %v", exempt, err)
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
	setTestMailList(t, db, policy.MailAllowedServers, "dav.example.test")
	exemptFromTestMailLists(t, db, "ben")
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
	ana := withMailAllowlistOwner(davTestContext(t), "ana")
	if err := get(ana, "https://dav.example.test/carddav/"); err != nil {
		t.Fatal("allowed server refused:", err)
	}
	if err := get(ana, "https://dav.example.test/moved"); err == nil || !strings.Contains(err.Error(), "other.example.test") {
		t.Fatal("redirect to a server outside the list followed:", err)
	}
	if err := get(davTestContext(t), "https://other.example.test/dav/"); err == nil {
		t.Fatal("request made for nobody reached a server outside the list")
	}
	if err := get(withMailAllowlistOwner(davTestContext(t), "ben"), "https://other.example.test/dav/"); err != nil {
		t.Fatal("exempt person refused:", err)
	}
}
