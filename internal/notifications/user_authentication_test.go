package notifications

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserAuthenticationLoginMailboxAndLogout(t *testing.T) {
	f := newUserStorageFixture(t)
	password := "a synthetic local login passphrase"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.system.Write().ExecContext(t.Context(), `INSERT INTO password_credentials(user_id,password_hash) VALUES ('alice',?)`, hash); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/login", "/admin/login"} {
		page := f.request("", "GET", path, "")
		if page.Code != 200 || !strings.Contains(page.Body.String(), `name="password"`) || page.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("login page %s: %d %s", path, page.Code, page.Body.String())
		}
	}
	form := url.Values{"identifier": {"alice"}, "password": {password}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://localhost:8090")
	response := httptest.NewRecorder()
	f.http.ServeHTTP(response, req)
	if response.Code != 303 || response.Header().Get("Location") != "/" {
		t.Fatal("password login", response.Code, response.Body.String(), response.Header())
	}
	var loginCookie *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "gofer_session" && cookie.MaxAge > 0 {
			loginCookie = cookie
		}
	}
	if loginCookie == nil || !loginCookie.HttpOnly {
		t.Fatal("login omitted session cookie")
	}
	loginSession, err := f.auth.GetSessionByToken(t.Context(), loginCookie.Value)
	if err != nil || loginSession == nil || loginSession.UserID != "alice" {
		t.Fatal("central login session", loginSession, err)
	}
	// Repository reads deliberately never return the raw bearer token.
	loginSession.Token = loginCookie.Value
	f.sessions["alice"] = loginSession
	if page := f.request("alice", "GET", "/contacts", ""); page.Code != 200 || !strings.Contains(page.Body.String(), "alice private contact") || strings.Contains(page.Body.String(), "bob private contact") {
		t.Fatal("new login did not select owned contacts", page.Code)
	}
	if page := f.request("alice", "GET", "/settings/accounts", ""); page.Code != 200 || !strings.Contains(page.Body.String(), "alice@example.com") || strings.Contains(page.Body.String(), "bob@example.com") {
		t.Fatal("new login did not select owned mailbox", page.Code)
	}
	var proof string
	req = httptest.NewRequest("GET", "/settings/security", nil)
	req.AddCookie(loginCookie)
	f.auth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		proof = auth.CSRFToken(r.Context(), "POST", "/auth/logout")
	})).ServeHTTP(httptest.NewRecorder(), req)
	if proof == "" {
		t.Fatal("logout proof missing")
	}
	if response := f.request("alice", "POST", "/auth/logout", ""); response.Code != 403 {
		t.Fatal("logout accepted without CSRF", response.Code)
	}
	response = f.request("alice", "POST", "/auth/logout", url.Values{"_csrf": {proof}}.Encode())
	if response.Code != 303 || response.Header().Get("Location") != "/login" {
		t.Fatal("logout route", response.Code, response.Body.String())
	}
	if session, err := f.auth.GetSessionByToken(t.Context(), loginCookie.Value); err != nil || session != nil {
		t.Fatal("central session survived logout", err)
	}
	if response := f.request("alice", "GET", "/contacts", ""); response.Code != 303 || response.Header().Get("Location") != "/login" {
		t.Fatal("revoked login retained mailbox access", response.Code, response.Header())
	}
	if response := f.request("bob", "GET", "/api/settings/ui", ""); response.Code != 200 {
		t.Fatal("logout affected another owner", response.Code)
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			var count int
			if err := db.Read().QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM sessions)+(SELECT COUNT(*) FROM password_credentials)+(SELECT COUNT(*) FROM auth_events)`).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Fatal("application authentication leaked into owned store", owner, count)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUserAuthenticationAssetsAndUnknownRoutes(t *testing.T) {
	// Disk assets use the application's launch directory; embedded builds use
	// the same fixture without depending on that directory.
	t.Chdir("../..")
	t.Setenv("GO_ENV", "production")
	f := newUserStorageFixture(t)
	for _, path := range []string{"/assets/js/passkey-authentication.js", "/sw.js"} {
		response := f.request("", "GET", path, "")
		if response.Code != 200 || response.Body.Len() == 0 || !strings.Contains(response.Header().Get("Cache-Control"), "public") {
			t.Fatal("public asset", path, response.Code, response.Header())
		}
	}
	for _, path := range []string{"/account/enroll?token=invalid", "/account/redeem?token=invalid", "/account/recover", "/setup"} {
		response := f.request("", "GET", path, "")
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("public auth route", path, response.Code, response.Header())
		}
	}
	if response := f.request("alice", "GET", "/not-a-gofer-route", ""); response.Code != 404 {
		t.Fatal("unknown route reached mailbox home", response.Code)
	}
	// Unsupported administrator data paths must not fall through to the private
	// mailbox router before owned administration is integrated.
	if response := f.request("alice", "GET", "/api/admin/contacts/status", ""); response.Code != 403 {
		t.Fatal("webmail user reached administration", response.Code)
	}
}

func TestUserAuthenticationRequiredPasswordChange(t *testing.T) {
	f := newUserStorageFixture(t)
	password := "the first synthetic local passphrase"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.system.Write().ExecContext(t.Context(), `INSERT INTO password_credentials(user_id,password_hash,must_change) VALUES ('alice',?,1)`, hash); err != nil {
		t.Fatal(err)
	}
	freshUserSecuritySession(t, f, "alice")
	oldToken := f.sessions["alice"].Token
	if response := f.request("alice", "GET", "/contacts", ""); response.Code != 303 || response.Header().Get("Location") != auth.RequiredPasswordChangePath {
		t.Fatal("required change did not block owned mailbox", response.Code, response.Header())
	}
	page := f.request("alice", "GET", auth.RequiredPasswordChangePath, "")
	if page.Code != 200 || page.Header().Get("Cache-Control") != "no-store" || !strings.Contains(page.Body.String(), `name="current_password"`) {
		t.Fatal("required change page missing", page.Code, page.Body.String())
	}
	var proof string
	req := httptest.NewRequest("GET", auth.RequiredPasswordChangePath, nil)
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: oldToken})
	f.auth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		proof = auth.CSRFToken(r.Context(), "POST", auth.RequiredPasswordChangePath)
	})).ServeHTTP(httptest.NewRecorder(), req)
	form := url.Values{"current_password": {password}, "new_password": {"the replacement synthetic passphrase"}, "confirm_password": {"the replacement synthetic passphrase"}, "_csrf": {proof}}
	response := f.request("alice", "POST", auth.RequiredPasswordChangePath, form.Encode())
	if response.Code != 303 || response.Header().Get("Location") != "/" {
		t.Fatal("required change completion", response.Code, response.Body.String())
	}
	if session, err := f.auth.GetSessionByToken(t.Context(), oldToken); err != nil || session != nil {
		t.Fatal("required change left old session active", err)
	}
	var newToken string
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "gofer_session" && cookie.MaxAge > 0 {
			newToken = cookie.Value
		}
	}
	session, err := f.auth.GetSessionByToken(t.Context(), newToken)
	if err != nil || session == nil || session.PasswordChangeRequired {
		t.Fatal("required change did not rotate central session", err)
	}
	session.Token = newToken
	f.sessions["alice"] = session
	if response := f.request("alice", "GET", "/contacts", ""); response.Code != 200 || !strings.Contains(response.Body.String(), "alice private contact") {
		t.Fatal("required change did not restore owned mailbox", response.Code)
	}
	if response := f.request("bob", "GET", "/api/settings/ui", ""); response.Code != 200 {
		t.Fatal("required change affected another owner", response.Code)
	}
}
