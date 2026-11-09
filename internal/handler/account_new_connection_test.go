package handler

import (
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newAccountTestForm(host string, port int, tls string) url.Values {
	return url.Values{
		"provider": {"imap"}, "auth_method": {"plain"},
		"email_address": {"ana@example.test"}, "display_name": {"Ana"},
		"username": {"ana@example.test"}, "password": {"not-saved"},
		"imap_host": {host}, "imap_port": {strconv.Itoa(port)}, "imap_tls_mode": {tls},
		"smtp_host": {host}, "smtp_port": {strconv.Itoa(port)}, "smtp_tls_mode": {tls},
	}
}

func postNewAccountTest(h *Handler, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/test", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// The sign-in middleware names the person adding the account.
	req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "owner", Username: "owner"}))
	rec := httptest.NewRecorder()
	h.handleTestNewAccount(rec, req)
	return rec
}

// The add-account wizard tests the typed settings before the account exists.
// The test must apply the same rules as a saved account and save nothing.
func TestNewAccountConnectionTestSavesNothingAndKeepsTheRules(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	countAccounts := func() int {
		var n int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countAccounts()

	incomplete := newAccountTestForm("mail.example.test", 993, "tls")
	incomplete.Del("password")
	if rec := postNewAccountTest(h, incomplete); rec.Code != http.StatusBadRequest {
		t.Fatalf("incomplete form = %d", rec.Code)
	}
	oauth := newAccountTestForm("mail.example.test", 993, "tls")
	oauth.Set("auth_method", "oauth2")
	if rec := postNewAccountTest(h, oauth); rec.Code != http.StatusBadRequest {
		t.Fatalf("oauth form = %d", rec.Code)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	contacted := make(chan struct{}, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			contacted <- struct{}{}
			_ = conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port

	rec := postNewAccountTest(h, newAccountTestForm("127.0.0.1", port, "tls"))
	if rec.Code != http.StatusOK || rec.Header().Get(accountConnectionTestResultHeader) != "failed" {
		t.Fatalf("private server test = %d %q", rec.Code, rec.Header().Get(accountConnectionTestResultHeader))
	}
	if body := rec.Body.String(); strings.Count(body, "is on a private network") != 2 {
		t.Fatalf("results don't explain both refusals: %q", body)
	}
	select {
	case <-contacted:
		t.Fatal("an unapproved private server was contacted")
	case <-time.After(50 * time.Millisecond):
	}

	rec = postNewAccountTest(h, newAccountTestForm("mail.example.test", 143, "plaintext"))
	if rec.Header().Get(accountConnectionTestResultHeader) != "failed" || !strings.Contains(rec.Body.String(), "plaintext connections require") {
		t.Fatalf("unapproved plaintext test = %q %q", rec.Header().Get(accountConnectionTestResultHeader), rec.Body.String())
	}

	if after := countAccounts(); after != before {
		t.Fatalf("testing created accounts: %d before, %d after", before, after)
	}
}
