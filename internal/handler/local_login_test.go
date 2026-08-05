package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

const localLoginPassword = "a safe local login passphrase"

func newLocalLoginHandler(t *testing.T, status auth.UserStatus, isAdmin, mfaRequired, insertUser, showGoogle bool) (*Handler, *auth.Manager, *storage.DB) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	config := &auth.Config{Enabled: true, BaseURL: "https://gofer.example", SecureCookies: true}
	if showGoogle {
		config.GoogleClient = &oauth2.Config{ClientID: "client-id"}
	}
	manager := auth.NewManager(config, db, auth.Dependencies{
		BucketHashKey: []byte("local-login-handler-key-32-bytes!"),
	})
	if insertUser {
		hash, err := auth.HashPassword(localLoginPassword)
		if err != nil {
			t.Fatalf("HashPassword() error = %v", err)
		}
		now := time.Now().UTC().Add(-time.Hour)
		adminValue, mfaValue := 0, 0
		if isAdmin {
			adminValue = 1
		}
		if mfaRequired {
			mfaValue = 1
		}
		if _, err := db.Write().ExecContext(t.Context(), `
			INSERT INTO users (
				id, email, email_normalized, username, username_normalized, name,
				status, auth_version, mfa_required, is_admin, created_at, updated_at
			) VALUES ('person', 'Person@Example.com', 'person@example.com',
			          'Person', 'person', 'Person', ?, 1, ?, ?, ?, ?)`,
			status, mfaValue, adminValue, now, now,
		); err != nil {
			t.Fatalf("insert login user: %v", err)
		}
		if _, err := db.Write().ExecContext(t.Context(), `
			INSERT INTO password_credentials (user_id, password_hash, created_at, changed_at)
			VALUES ('person', ?, ?, ?)`, hash, now, now,
		); err != nil {
			t.Fatalf("insert password credential: %v", err)
		}
	}
	return &Handler{db: db, auth: manager}, manager, db
}

func postLocalLogin(t *testing.T, handler *Handler, identifier, password string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"identifier": {identifier}, "password": {password}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", "Gofer Login Test/1.0")
	request.RemoteAddr = "198.51.100.70:43120"
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.handleLoginSubmit(recorder, request)
	return recorder
}

func responseCookie(recorder *httptest.ResponseRecorder, name string, positiveMaxAge bool) *http.Cookie {
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == name && (!positiveMaxAge || cookie.MaxAge > 0) {
			return cookie
		}
	}
	return nil
}

func TestLocalLoginSuccessSetsSessionAndUsesSafeReturnTarget(t *testing.T) {
	handler, manager, _ := newLocalLoginHandler(t, auth.UserStatusActive, false, false, true, false)
	returnCookieRecorder := httptest.NewRecorder()
	auth.SetReturnToCookie(returnCookieRecorder, "/settings/advanced?from=login", true)
	returnCookie := responseCookie(returnCookieRecorder, "gofer_auth_return_to", true)
	if returnCookie == nil {
		t.Fatal("return-to setup omitted cookie")
	}

	recorder := postLocalLogin(t, handler, " PERSON@example.COM ", localLoginPassword, returnCookie)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/settings/advanced?from=login" {
		t.Fatalf("successful login = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	sessionCookie := responseCookie(recorder, "gofer_session", true)
	if sessionCookie == nil || !sessionCookie.HttpOnly || !sessionCookie.Secure || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie = %#v", sessionCookie)
	}
	session, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || session == nil {
		t.Fatalf("GetSessionByToken() = %#v, %v", session, err)
	}
	if session.AuthenticationMethod != auth.AuthenticationMethodPassword || session.AssuranceLevel != auth.AssuranceLevelSingleFactor || session.UserAgent != "Gofer Login Test/1.0" {
		t.Fatalf("password session = %#v", session)
	}
	clearedReturnTo := responseCookie(recorder, "gofer_auth_return_to", false)
	if clearedReturnTo == nil || clearedReturnTo.MaxAge != -1 {
		t.Fatalf("cleared return-to cookie = %#v", clearedReturnTo)
	}
}

func TestLocalLoginFailuresUseOneGenericResponse(t *testing.T) {
	tests := []struct {
		name       string
		insertUser bool
		status     auth.UserStatus
		password   string
	}{
		{name: "wrong password", insertUser: true, status: auth.UserStatusActive, password: "incorrect passphrase"},
		{name: "unknown identifier", password: "incorrect passphrase"},
		{name: "disabled user", insertUser: true, status: auth.UserStatusDisabled, password: localLoginPassword},
		{name: "pending user", insertUser: true, status: auth.UserStatusPending, password: localLoginPassword},
	}
	var referenceBody string
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, _, _ := newLocalLoginHandler(t, test.status, false, false, test.insertUser, false)
			recorder := postLocalLogin(t, handler, "person@example.com", test.password)
			body := recorder.Body.String()
			if recorder.Code != http.StatusUnauthorized || !strings.Contains(body, loginFailureMessage) || strings.Contains(strings.ToLower(body), test.name) {
				t.Fatalf("generic login failure = status:%d body:%q", recorder.Code, body)
			}
			if referenceBody == "" {
				referenceBody = body
			} else if body != referenceBody {
				t.Fatal("login failure body differs by account state")
			}
			if responseCookie(recorder, "gofer_session", true) != nil {
				t.Fatal("failed login issued session cookie")
			}
		})
	}
}

func TestLocalLoginThrottleReturnsRetryAfter(t *testing.T) {
	handler, manager, _ := newLocalLoginHandler(t, auth.UserStatusActive, false, false, true, false)
	for attempt := 1; attempt < 5; attempt++ {
		if _, err := manager.RecordLoginFailure(t.Context(), "person@example.com", "seed-source-"+string(rune('0'+attempt))); err != nil {
			t.Fatalf("seed login failure %d: %v", attempt, err)
		}
	}
	recorder := postLocalLogin(t, handler, "person@example.com", "incorrect passphrase")
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "1" || !strings.Contains(recorder.Body.String(), loginFailureMessage) {
		t.Fatalf("throttled login = status:%d retry:%q body:%q", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
}

func TestLocalLoginMFAContinuationNeverCreatesSession(t *testing.T) {
	handler, _, db := newLocalLoginHandler(t, auth.UserStatusActive, true, false, true, false)
	recorder := postLocalLogin(t, handler, "person", localLoginPassword)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login/mfa" {
		t.Fatalf("MFA login = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	challengeCookie := responseCookie(recorder, "gofer_pre_auth", true)
	if challengeCookie == nil || !challengeCookie.HttpOnly || !challengeCookie.Secure || challengeCookie.MaxAge != 600 {
		t.Fatalf("MFA challenge cookie = %#v", challengeCookie)
	}
	var sessionCount int
	var purpose, challengeHash string
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&sessionCount); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT purpose, challenge_hash FROM auth_challenges`).Scan(&purpose, &challengeHash); err != nil {
		t.Fatal(err)
	}
	if sessionCount != 0 || purpose != string(auth.ChallengePurposeMFA) || challengeHash == challengeCookie.Value {
		t.Fatalf("MFA state = sessions:%d purpose:%q hash:%q", sessionCount, purpose, challengeHash)
	}

	request := httptest.NewRequest(http.MethodGet, "/login/mfa", nil)
	request.AddCookie(challengeCookie)
	pageRecorder := httptest.NewRecorder()
	handler.handleLoginMFA(pageRecorder, request)
	if pageRecorder.Code != http.StatusOK || !strings.Contains(pageRecorder.Body.String(), "Additional verification required") || strings.Contains(pageRecorder.Body.String(), "https://") {
		t.Fatalf("MFA continuation page = %d %q", pageRecorder.Code, pageRecorder.Body.String())
	}
}

func TestLocalLoginMFAContinuationRequiresCookie(t *testing.T) {
	handler, _, _ := newLocalLoginHandler(t, auth.UserStatusActive, false, false, false, false)
	request := httptest.NewRequest(http.MethodGet, "/login/mfa", nil)
	recorder := httptest.NewRecorder()
	handler.handleLoginMFA(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login" {
		t.Fatalf("missing MFA cookie response = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	cleared := responseCookie(recorder, "gofer_pre_auth", false)
	if cleared == nil || cleared.MaxAge != -1 {
		t.Fatalf("invalid MFA cookie cleanup = %#v", cleared)
	}

	request = httptest.NewRequest(http.MethodGet, "/login/mfa", nil)
	request.AddCookie(&http.Cookie{Name: "gofer_pre_auth", Value: "forged-token"})
	recorder = httptest.NewRecorder()
	handler.handleLoginMFA(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login" {
		t.Fatalf("forged MFA cookie response = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestDirectLoginSourceUsesOnlyTransportPeer(t *testing.T) {
	tests := map[string]string{
		"198.51.100.8:42100":  "198.51.100.8",
		"[2001:db8::5]:443":   "2001:db8::5",
		"  local-transport  ": "local-transport",
		"":                    "",
	}
	for input, want := range tests {
		if got := directLoginSource(input); got != want {
			t.Fatalf("directLoginSource(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLocalLoginRejectsOversizedForm(t *testing.T) {
	handler, _, _ := newLocalLoginHandler(t, auth.UserStatusActive, false, false, false, false)
	request := httptest.NewRequest(http.MethodPost, "/login", io.LimitReader(strings.NewReader(strings.Repeat("x", loginFormMaximumBytes+1)), loginFormMaximumBytes+1))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	handler.handleLoginSubmit(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), loginFailureMessage) {
		t.Fatalf("oversized login form = %d %q", recorder.Code, recorder.Body.String())
	}
}
