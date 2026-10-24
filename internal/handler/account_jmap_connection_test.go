package handler

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
)

// jmapSessionTestServer answers session requests for lena@example.test with
// a mail account; hits counts every request it receives.
func jmapSessionTestServer(t *testing.T, password string, hits *atomic.Int32) (*httptest.Server, string, int) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if user, pass, ok := r.BasicAuth(); !ok || user != "lena@example.test" || pass != password {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"capabilities": {"urn:ietf:params:jmap:core": {}, "urn:ietf:params:jmap:mail": {}},
			"accounts": {"a1": {"name": "lena@example.test"}}, "primaryAccounts": {"urn:ietf:params:jmap:mail": "a1"},
			"username": "lena@example.test", "apiUrl": "/jmap/", "state": "s1"}`)
	}))
	t.Cleanup(server.Close)
	host, portText, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	port, _ := strconv.Atoi(portText)
	return server, host, port
}

func jmapAccountTestForm(server, password string) url.Values {
	return url.Values{
		"provider": {"jmap"}, "email_address": {"lena@example.test"}, "display_name": {"Lena"},
		"password": {password}, "jmap_server": {server},
	}
}

// The add-account wizard finds a JMAP server from the address typed in,
// reports the session URL to save, and contacts plain HTTP only once an
// administrator approved it. Nothing is saved.
func TestNewJMAPAccountConnectionTestDiscoversTheSession(t *testing.T) {
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	h, db := newAccountOwnershipTestHandler(t)
	var hits atomic.Int32
	server, host, port := jmapSessionTestServer(t, "secret", &hits)
	address := net.JoinHostPort(host, strconv.Itoa(port))

	rec := postNewAccountTest(h, jmapAccountTestForm(address, "secret"))
	if rec.Header().Get(accountConnectionTestResultHeader) != "failed" || !strings.Contains(rec.Body.String(), "HTTP plaintext exception") {
		t.Fatalf("without exceptions = %q %s", rec.Header().Get(accountConnectionTestResultHeader), rec.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatal("the password went out over unapproved plain HTTP")
	}

	if err := db.AddPlaintextTransportException(t.Context(), "http", host, port, "administrator"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPrivateTargetException(t.Context(), "http", host, port, "administrator"); err != nil {
		t.Fatal(err)
	}
	rec = postNewAccountTest(h, jmapAccountTestForm(address, "secret"))
	if rec.Header().Get(accountConnectionTestResultHeader) != "ok" {
		t.Fatalf("with exceptions = %q %s", rec.Header().Get(accountConnectionTestResultHeader), rec.Body.String())
	}
	if got, want := rec.Header().Get(jmapSessionURLHeader), server.URL+"/.well-known/jmap"; got != want {
		t.Fatalf("session URL header = %q, want %q", got, want)
	}
	// The session offers no submission, so the test passes with a warning.
	if !strings.Contains(rec.Body.String(), "let this login send mail") {
		t.Fatalf("no sending warning: %s", rec.Body.String())
	}

	rec = postNewAccountTest(h, jmapAccountTestForm(address, "wrong"))
	if rec.Header().Get(accountConnectionTestResultHeader) != "failed" || !strings.Contains(rec.Body.String(), "rejected the username or password") {
		t.Fatalf("wrong password = %q %s", rec.Header().Get(accountConnectionTestResultHeader), rec.Body.String())
	}

	var accounts int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM accounts WHERE provider='jmap'`).Scan(&accounts); err != nil || accounts != 0 {
		t.Fatalf("saved %d JMAP accounts, %v", accounts, err)
	}
}

// A saved JMAP account tests its stored session URL and password, both from
// the account's test button and from the edit wizard with the password left
// blank.
func TestSavedJMAPAccountConnectionTests(t *testing.T) {
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	h, db := newAccountOwnershipTestHandler(t)
	var hits atomic.Int32
	server, host, port := jmapSessionTestServer(t, "saved-secret", &hits)
	if err := db.AddPlaintextTransportException(t.Context(), "http", host, port, "administrator"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPrivateTargetException(t.Context(), "http", host, port, "administrator"); err != nil {
		t.Fatal(err)
	}
	sessionURL := server.URL + "/.well-known/jmap"
	account, err := h.accountStore.CreateAccount(t.Context(), "owner", &models.CreateAccountRequest{
		Provider: "jmap", EmailAddress: "lena@example.test", DisplayName: "Lena",
		JMAPSessionURL: sessionURL, Username: "lena@example.test", Password: "saved-secret",
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/accounts/"+account.ID+"/test?wizard=check", nil)
	req.SetPathValue("id", account.ID)
	rec := httptest.NewRecorder()
	h.handleTestAccount(rec, ownerRequest(req))
	if rec.Header().Get(accountConnectionTestResultHeader) != "ok" || !strings.Contains(rec.Body.String(), "mail account a1") {
		t.Fatalf("saved account test = %d %q %s", rec.Code, rec.Header().Get(accountConnectionTestResultHeader), rec.Body.String())
	}

	edit := func(asOwner bool) *httptest.ResponseRecorder {
		form := url.Values{"provider": {"jmap"}, "email_address": {"lena@example.test"}, "jmap_session_url": {sessionURL}}
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/"+account.ID+"/test-changes", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetPathValue("id", account.ID)
		if asOwner {
			req = ownerRequest(req)
		} else {
			req = attackerRequest(req)
		}
		rec := httptest.NewRecorder()
		h.handleTestAccountChanges(rec, req)
		return rec
	}
	if rec := edit(true); rec.Header().Get(accountConnectionTestResultHeader) != "ok" {
		t.Fatalf("edit test with saved password = %q %s", rec.Header().Get(accountConnectionTestResultHeader), rec.Body.String())
	}
	if rec := edit(false); rec.Code != http.StatusNotFound {
		t.Fatalf("another person's account edit test = %d, want 404", rec.Code)
	}
}

func TestJMAPConnectionTestsNeedTheExperimentFlag(t *testing.T) {
	h, _ := newAccountOwnershipTestHandler(t)
	t.Setenv("GOFER_JMAP_EXPERIMENT", "")
	rec := postNewAccountTest(h, jmapAccountTestForm("mail.example.test", "secret"))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "GOFER_JMAP_EXPERIMENT") {
		t.Fatalf("new account test with the flag off = %d %s", rec.Code, rec.Body.String())
	}
}

func jmapCreateRequest(form url.Values) (*http.Request, models.CreateAccountRequest) {
	req := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = req.ParseForm()
	return req, accountRequestFromForm(req)
}

// Creating a JMAP account keeps the session URL the wizard's test found, and
// discovers it when the test was skipped.
func TestPrepareJMAPAccountForCreation(t *testing.T) {
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	h, db := newAccountOwnershipTestHandler(t)
	var hits atomic.Int32
	server, host, port := jmapSessionTestServer(t, "secret", &hits)
	if err := db.AddPlaintextTransportException(t.Context(), "http", host, port, "administrator"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPrivateTargetException(t.Context(), "http", host, port, "administrator"); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"mail_service": {"jmap"}, "email_address": {"lena@example.test"}, "password": {"secret"}, "jmap_server": {net.JoinHostPort(host, strconv.Itoa(port))}}

	r, req := jmapCreateRequest(form)
	if req.Provider != "jmap" {
		t.Fatalf("mail_service jmap gave provider %q", req.Provider)
	}
	if err := h.prepareJMAPAccount(r, &req); err != nil {
		t.Fatalf("prepareJMAPAccount (discovering): %v", err)
	}
	if req.Username != "lena@example.test" || req.JMAPSessionURL != server.URL+"/.well-known/jmap" {
		t.Fatalf("discovered request = user %q url %q", req.Username, req.JMAPSessionURL)
	}

	hits.Store(0)
	form.Set("jmap_session_url", "http://other.example.test/.well-known/jmap")
	r, req = jmapCreateRequest(form)
	if err := h.prepareJMAPAccount(r, &req); err != nil || req.JMAPSessionURL != "http://other.example.test/.well-known/jmap" || hits.Load() != 0 {
		t.Fatalf("with a tested URL: %v, url %q, %d requests", err, req.JMAPSessionURL, hits.Load())
	}

	form.Del("password")
	r, req = jmapCreateRequest(form)
	if err := h.prepareJMAPAccount(r, &req); err == nil {
		t.Fatal("accepted a JMAP account without a password")
	}

	t.Setenv("GOFER_JMAP_EXPERIMENT", "")
	form.Set("password", "secret")
	r, req = jmapCreateRequest(form)
	if err := h.prepareJMAPAccount(r, &req); !errors.Is(err, providers.ErrJMAPDisabled) {
		t.Fatalf("with the flag off: %v", err)
	}
}

// Saving an edited JMAP account keeps a session URL as entered, and finds it
// again from a host or the email, using the saved password when the
// password field was left blank.
func TestResolveJMAPAccountChanges(t *testing.T) {
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	h, db := newAccountOwnershipTestHandler(t)
	var hits atomic.Int32
	server, host, port := jmapSessionTestServer(t, "saved-secret", &hits)
	if err := db.AddPlaintextTransportException(t.Context(), "http", host, port, "administrator"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPrivateTargetException(t.Context(), "http", host, port, "administrator"); err != nil {
		t.Fatal(err)
	}
	account, err := h.accountStore.CreateAccount(t.Context(), "owner", &models.CreateAccountRequest{
		Provider: "jmap", EmailAddress: "lena@example.test", JMAPSessionURL: "https://old.example.test/.well-known/jmap",
		Username: "lena@example.test", Password: "saved-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(form url.Values) (models.CreateAccountRequest, error) {
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/"+account.ID+"/edit", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetPathValue("id", account.ID)
		req = ownerRequest(req)
		_ = req.ParseForm()
		changes := accountRequestFromForm(req)
		return changes, h.resolveJMAPAccountChanges(req, account.ID, &changes)
	}

	changes, err := resolve(url.Values{"provider": {"jmap"}, "email_address": {"lena@example.test"}, "jmap_session_url": {"https://new.example.test/jmap/session"}})
	if err != nil || changes.JMAPSessionURL != "https://new.example.test/jmap/session" || hits.Load() != 0 {
		t.Fatalf("entered URL: %v, url %q, %d requests", err, changes.JMAPSessionURL, hits.Load())
	}

	changes, err = resolve(url.Values{"provider": {"jmap"}, "email_address": {"lena@example.test"}, "jmap_session_url": {net.JoinHostPort(host, strconv.Itoa(port))}})
	if err != nil || changes.JMAPSessionURL != server.URL+"/.well-known/jmap" || changes.Username != "lena@example.test" {
		t.Fatalf("host with the saved password: %v, url %q user %q", err, changes.JMAPSessionURL, changes.Username)
	}

	_, err = resolve(url.Values{"provider": {"jmap"}, "email_address": {"lena@example.test"}, "jmap_session_url": {net.JoinHostPort(host, strconv.Itoa(port))}, "password": {"typed-but-wrong"}})
	if !errors.Is(err, mail.ErrJMAPLoginRejected) {
		t.Fatalf("typed wrong password: %v", err)
	}
}
