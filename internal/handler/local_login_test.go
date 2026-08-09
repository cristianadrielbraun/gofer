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
	"github.com/cristianadrielbraun/gofer/internal/httpguard"
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
	if pageRecorder.Code != http.StatusOK || !strings.Contains(pageRecorder.Body.String(), "Enter your authenticator code") ||
		!strings.Contains(pageRecorder.Body.String(), `action="/login/mfa"`) || strings.Contains(pageRecorder.Body.String(), "https://") {
		t.Fatalf("MFA continuation page = %d %q", pageRecorder.Code, pageRecorder.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/login/mfa?code=must-not-survive", nil)
	request.AddCookie(challengeCookie)
	queryRecorder := httptest.NewRecorder()
	handler.handleLoginMFA(queryRecorder, request)
	if queryRecorder.Code != http.StatusSeeOther || queryRecorder.Header().Get("Location") != "/login/mfa" || strings.Contains(queryRecorder.Body.String(), "must-not-survive") {
		t.Fatalf("MFA query stripping = %d location:%q body:%q", queryRecorder.Code, queryRecorder.Header().Get("Location"), queryRecorder.Body.String())
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

func TestLocalLoginTOTPContinuationCreatesMultiFactorSession(t *testing.T) {
	manager, _, stack, setupCookie := prepareHandlerSetupReview(t)
	state, err := manager.GetSetupOwnerState(t.Context(), setupCookie.Value, "https://gofer.example")
	if err != nil || state == nil || state.Draft == nil || state.Draft.TOTPSecret == "" {
		t.Fatalf("prepared setup state = %#v, %v", state, err)
	}
	secret := state.Draft.TOTPSecret
	completed := postSetupReview(stack, setupCookie, url.Values{"action": {"complete"}})
	if completed.Code != http.StatusSeeOther {
		t.Fatalf("setup completion prerequisite = %d %q", completed.Code, completed.Body.String())
	}

	returnCookieRecorder := httptest.NewRecorder()
	auth.SetReturnToCookie(returnCookieRecorder, "/settings/advanced?from=mfa", true)
	returnCookie := responseCookie(returnCookieRecorder, "gofer_auth_return_to", true)
	loginForm := url.Values{
		"identifier": {"owner"},
		"password":   {"correct horse battery staple for owner"},
	}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(loginForm.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	request.Header.Set("User-Agent", "Owner MFA Browser/1.0")
	request.RemoteAddr = "198.51.100.88:41412"
	request.AddCookie(returnCookie)
	password := httptest.NewRecorder()
	stack.ServeHTTP(password, request)
	if password.Code != http.StatusSeeOther || password.Header().Get("Location") != "/login/mfa" || responseCookie(password, "gofer_session", true) != nil {
		t.Fatalf("password MFA start = %d location:%q cookies:%#v body:%q", password.Code, password.Header().Get("Location"), password.Result().Cookies(), password.Body.String())
	}
	challengeCookie := responseCookie(password, "gofer_pre_auth", true)
	if challengeCookie == nil {
		t.Fatal("password MFA start omitted challenge cookie")
	}

	request = httptest.NewRequest(http.MethodGet, "/login/mfa", nil)
	request.AddCookie(challengeCookie)
	page := httptest.NewRecorder()
	stack.ServeHTTP(page, request)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `action="/login/mfa"`) ||
		!strings.Contains(page.Body.String(), `autocomplete="one-time-code"`) || strings.Contains(page.Body.String(), secret) {
		t.Fatalf("TOTP continuation page = %d %q", page.Code, page.Body.String())
	}

	currentCode := setupHandlerTOTPCode(t, secret)
	invalidCode := "0" + currentCode[1:]
	if currentCode[0] == '0' {
		invalidCode = "1" + currentCode[1:]
	}
	invalidForm := url.Values{"code": {invalidCode}}
	request = httptest.NewRequest(http.MethodPost, "/login/mfa", strings.NewReader(invalidForm.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	request.RemoteAddr = "198.51.100.88:41412"
	request.AddCookie(challengeCookie)
	invalid := httptest.NewRecorder()
	stack.ServeHTTP(invalid, request)
	if invalid.Code != http.StatusUnauthorized || !strings.Contains(invalid.Body.String(), loginMFAFailureMessage) ||
		responseCookie(invalid, "gofer_session", true) != nil || strings.Contains(invalid.Body.String(), invalidCode) {
		t.Fatalf("invalid TOTP continuation = %d cookies:%#v body:%q", invalid.Code, invalid.Result().Cookies(), invalid.Body.String())
	}

	validCode := setupHandlerTOTPCodeAt(t, secret, time.Now().UTC().Add(30*time.Second))
	validForm := url.Values{"code": {validCode}}
	request = httptest.NewRequest(http.MethodPost, "/login/mfa", strings.NewReader(validForm.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	request.Header.Set("User-Agent", "Owner MFA Browser/1.0")
	request.RemoteAddr = "198.51.100.88:41412"
	request.AddCookie(challengeCookie)
	request.AddCookie(returnCookie)
	verified := httptest.NewRecorder()
	stack.ServeHTTP(verified, request)
	if verified.Code != http.StatusSeeOther || verified.Header().Get("Location") != "/settings/advanced?from=mfa" || strings.Contains(verified.Body.String(), validCode) {
		t.Fatalf("verified TOTP continuation = %d location:%q body:%q", verified.Code, verified.Header().Get("Location"), verified.Body.String())
	}
	sessionCookie := responseCookie(verified, "gofer_session", true)
	if sessionCookie == nil || !sessionCookie.HttpOnly || !sessionCookie.Secure || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("verified TOTP session cookie = %#v", sessionCookie)
	}
	if cleared := responseCookie(verified, "gofer_pre_auth", false); cleared == nil || cleared.MaxAge != -1 {
		t.Fatalf("verified TOTP pre-auth cleanup = %#v", cleared)
	}
	if cleared := responseCookie(verified, "gofer_auth_return_to", false); cleared == nil || cleared.MaxAge != -1 {
		t.Fatalf("verified TOTP return-target cleanup = %#v", cleared)
	}
	session, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || session == nil || session.AuthenticationMethod != auth.AuthenticationMethodPassword ||
		session.AssuranceLevel != auth.AssuranceLevelMultiFactor || session.StepUpAt == nil ||
		session.StepUpMethod != auth.AuthenticationMethodTOTP || session.UserAgent != "Owner MFA Browser/1.0" {
		t.Fatalf("verified TOTP session = %#v, %v", session, err)
	}
}

func TestLocalLoginMFAPostIsProtectedByCanonicalOriginGuard(t *testing.T) {
	handler, _, db := newLocalLoginHandler(t, auth.UserStatusActive, true, false, true, false)
	password := postLocalLogin(t, handler, "person", localLoginPassword)
	challengeCookie := responseCookie(password, "gofer_pre_auth", true)
	if challengeCookie == nil {
		t.Fatal("MFA origin test omitted challenge cookie")
	}
	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"code": {"123456"}}
	request := httptest.NewRequest(http.MethodPost, "/login/mfa", strings.NewReader(form.Encode()))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	request.AddCookie(challengeCookie)
	recorder := httptest.NewRecorder()
	guard.Middleware(http.HandlerFunc(handler.handleLoginMFASubmit)).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "cross-origin request blocked") || strings.Contains(recorder.Body.String(), "123456") {
		t.Fatalf("cross-origin TOTP submission = %d %q", recorder.Code, recorder.Body.String())
	}
	var attempts, sessions int
	if err := db.Read().QueryRow(`SELECT attempts FROM auth_challenges`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || sessions != 0 {
		t.Fatalf("cross-origin TOTP mutation = attempts:%d sessions:%d", attempts, sessions)
	}
}

func TestLocalLoginMFARejectsMissingCookieAndOversizedForm(t *testing.T) {
	handler, _, db := newLocalLoginHandler(t, auth.UserStatusActive, true, false, true, false)
	request := httptest.NewRequest(http.MethodPost, "/login/mfa", strings.NewReader(url.Values{"code": {"123456"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	handler.handleLoginMFASubmit(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login?error=mfa" || responseCookie(recorder, "gofer_session", true) != nil {
		t.Fatalf("missing MFA cookie = %d location:%q cookies:%#v", recorder.Code, recorder.Header().Get("Location"), recorder.Result().Cookies())
	}

	password := postLocalLogin(t, handler, "person", localLoginPassword)
	challengeCookie := responseCookie(password, "gofer_pre_auth", true)
	request = httptest.NewRequest(http.MethodPost, "/login/mfa", io.LimitReader(strings.NewReader(strings.Repeat("x", loginMFAFormMaximumBytes+1)), loginMFAFormMaximumBytes+1))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(challengeCookie)
	recorder = httptest.NewRecorder()
	handler.handleLoginMFASubmit(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), loginMFAFailureMessage) || strings.Contains(recorder.Body.String(), strings.Repeat("x", 32)) {
		t.Fatalf("oversized MFA form = %d %q", recorder.Code, recorder.Body.String())
	}
	var attempts, sessions int
	if err := db.Read().QueryRow(`SELECT attempts FROM auth_challenges`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || sessions != 0 {
		t.Fatalf("oversized MFA mutation = attempts:%d sessions:%d", attempts, sessions)
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
