package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/policy"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
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

// The administrator limits the servers and domains mailboxes use from the
// Policies pages: Mail security lists the mailboxes outside the lists,
// those stop connecting, and a person a policy exempts connects anywhere.
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
	// change sends a list's dialog its change, as the Policies page does.
	change := func(path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		dialog := f.request(path)
		if dialog.Code != 200 {
			t.Fatal(path, dialog.Code, dialog.Body.String())
		}
		return ownedSecurityPost(t, f, dialog.Body.String(), path, form)
	}
	changed := func(path string, form url.Values) {
		t.Helper()
		if response := change(path, form); response.Code != 303 || response.Header().Get("Location") != path || adminFlashFailed(t, response) {
			t.Fatal(path, form, response.Code, response.Header(), response.Body.String())
		}
	}
	servers := views.AdminPolicySettingPath(policy.MailAllowedServers.Key)
	domains := views.AdminPolicySettingPath(policy.MailAllowedDomains.Key)

	page := f.request("/admin/security")
	if body := page.Body.String(); page.Code != 200 || !strings.Contains(body, "Allowed mail servers and domains") || !strings.Contains(body, "Nothing is limited.") ||
		strings.Contains(body, `action="/admin/security/allowlist`) || !strings.Contains(body, `href="/admin/policies/default/mail"`) {
		t.Fatal("allowed mail page", page.Code, body)
	}
	changed(servers, url.Values{"op": {"add"}, "value": {"*.Example.com"}})
	// Alice's contacts come from a CardDAV server on this address.
	changed(servers, url.Values{"op": {"add"}, "value": {"127.0.0.1"}})
	body := f.request("/admin/security").Body.String()
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

	// Bob is exempted by a policy that sets every list to nothing.
	exempt, err := f.manager.CreatePolicy(davTestContext(t), auth.CreatePolicyOptions{ActorUserID: "administrator", ActorSessionID: f.session.ID, Name: "No limits"})
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []*policy.Setting{policy.MailAllowedServers, policy.MailAllowedDomains, policy.MailAllowedProviders} {
		changed(views.AdminNamedPolicySettingPath(exempt.ID, setting.Key), url.Values{"op": {"set"}})
	}
	if _, err := f.manager.AssignPolicy(davTestContext(t), auth.ChangePolicyAssignmentsOptions{ActorUserID: "administrator", ActorSessionID: f.session.ID, PolicyID: exempt.ID, Targets: []policy.Target{{Kind: policy.TargetUser, ID: "bob"}}}); err != nil {
		t.Fatal(err)
	}
	if err := connects("bob"); err != nil {
		t.Fatal("exempt person refused:", err)
	}
	if body := f.request("/admin/security").Body.String(); !strings.Contains(body, "Every mailbox is within the lists.") {
		t.Fatal("exempt person's mailbox still listed", body)
	}

	changed(domains, url.Values{"op": {"add"}, "value": {"@example.org"}})
	if err := connects("alice"); !storage.IsMailAllowlistError(err) || !strings.Contains(err.Error(), "@example.com") {
		t.Fatal("address outside the domains connected:", err)
	}

	refused := change(servers, url.Values{"op": {"add"}, "value": {"imap.example.com:993"}})
	if body := refused.Body.String(); refused.Code != 400 || !strings.Contains(body, "Enter a host name or IP address") || !strings.Contains(body, `value="imap.example.com:993"`) {
		t.Fatal("server with a port accepted", refused.Code, body)
	}
	for _, server := range []string{"*.example.com", "127.0.0.1"} {
		changed(servers, url.Values{"op": {"remove"}, "value": {server}})
	}
	changed(domains, url.Values{"op": {"remove"}, "value": {"example.org"}})
	if err := connects("alice"); err != nil {
		t.Fatal("refused with empty lists:", err)
	}
	var recorded int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'policy.settings_changed'`).Scan(&recorded); err != nil || recorded != 9 {
		t.Fatalf("recorded %d settings changes, %v", recorded, err)
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
