package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/httpguard"
)

func passkeyLoginStack(t *testing.T) (*Handler, *auth.Manager, http.Handler) {
	t.Helper()
	handler, manager, _ := newLocalLoginHandler(t, auth.UserStatusActive, false, false, true, false)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	return handler, manager, manager.Middleware(mux)
}

func postPasskeyStart(t *testing.T, stack http.Handler, identifier string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"identifier": {identifier}}
	request := httptest.NewRequest(http.MethodPost, loginPasskeyStartPath, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	request.RemoteAddr = "198.51.100.71:45120"
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	return recorder
}

func TestPasskeyLoginPageAndPublicHandlersSupportDiscoverableFallback(t *testing.T) {
	_, manager, stack := passkeyLoginStack(t)
	pageRequest := httptest.NewRequest(http.MethodGet, "/login", nil)
	page := httptest.NewRecorder()
	stack.ServeHTTP(page, pageRequest)
	if page.Code != http.StatusOK {
		t.Fatalf("passkey login page = %d %q", page.Code, page.Body.String())
	}
	for _, want := range []string{
		`data-passkey-authentication`, `data-start-path="/login/passkey/start"`,
		`data-finish-path="/login/passkey/finish"`, `data-identifier-source="#login-identifier"`,
		`src="/assets/js/passkey-authentication.js"`, "Sign in with a passkey", "use your password",
	} {
		if !strings.Contains(strings.ToLower(page.Body.String()), strings.ToLower(want)) {
			t.Fatalf("passkey login page missing %q: %q", want, page.Body.String())
		}
	}

	started := postPasskeyStart(t, stack, "")
	if started.Code != http.StatusOK || started.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(started.Body.String(), `"userVerification":"required"`) {
		t.Fatalf("discoverable passkey start = %d headers:%v body:%q", started.Code, started.Header(), started.Body.String())
	}
	challengeCookie := responseCookie(started, "gofer_passkey_login", true)
	if challengeCookie == nil || !challengeCookie.HttpOnly || !challengeCookie.Secure ||
		challengeCookie.Path != "/login" || challengeCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("passkey login challenge cookie = %#v", challengeCookie)
	}

	request := httptest.NewRequest(http.MethodPost, loginPasskeyFinishPath, strings.NewReader(`{"invalid":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://gofer.example")
	request.Header.Set("User-Agent", "Passkey Handler/1.0")
	request.RemoteAddr = "198.51.100.71:45120"
	request.AddCookie(challengeCookie)
	rejected := httptest.NewRecorder()
	stack.ServeHTTP(rejected, request)
	if rejected.Code != http.StatusUnauthorized || !strings.Contains(rejected.Body.String(), passkeyLoginFailureMessage) ||
		responseCookie(rejected, "gofer_session", true) != nil {
		t.Fatalf("invalid passkey finish = %d cookies:%#v body:%q", rejected.Code, rejected.Result().Cookies(), rejected.Body.String())
	}
	if cleared := responseCookie(rejected, "gofer_passkey_login", false); cleared == nil || cleared.MaxAge != -1 {
		t.Fatalf("invalid passkey challenge cleanup = %#v", cleared)
	}
	var activeChallenges, sessions int
	if err := manager.DB().Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_challenges WHERE purpose = ? AND consumed_at IS NULL`, auth.ChallengePurposeLogin,
	).Scan(&activeChallenges); err != nil {
		t.Fatal(err)
	}
	if err := manager.DB().Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if activeChallenges != 0 || sessions != 0 {
		t.Fatalf("invalid passkey state = challenges:%d sessions:%d", activeChallenges, sessions)
	}
}

func TestPasskeyLoginUnknownIdentifierUsesGenericBoundedFailure(t *testing.T) {
	_, _, stack := passkeyLoginStack(t)
	unknown := postPasskeyStart(t, stack, "unknown@example.com")
	if unknown.Code != http.StatusUnprocessableEntity || !strings.Contains(unknown.Body.String(), passkeyLoginFailureMessage) ||
		strings.Contains(strings.ToLower(unknown.Body.String()), "unknown") {
		t.Fatalf("unknown passkey identifier = %d %q", unknown.Code, unknown.Body.String())
	}
	request := httptest.NewRequest(
		http.MethodPost, loginPasskeyStartPath,
		io.LimitReader(strings.NewReader(strings.Repeat("x", passkeyLoginStartMaximumBytes+1)), passkeyLoginStartMaximumBytes+1),
	)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	oversized := httptest.NewRecorder()
	stack.ServeHTTP(oversized, request)
	if oversized.Code != http.StatusBadRequest || !strings.Contains(oversized.Body.String(), passkeyLoginFailureMessage) {
		t.Fatalf("oversized passkey start = %d %q", oversized.Code, oversized.Body.String())
	}
}

func TestPasskeyEndpointsRemainProtectedByCanonicalOriginAndSessionCSRF(t *testing.T) {
	handler, _, db := newLocalLoginHandler(t, auth.UserStatusActive, false, false, true, false)
	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"identifier": {"person@example.com"}}
	request := httptest.NewRequest(http.MethodPost, loginPasskeyStartPath, strings.NewReader(form.Encode()))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	blocked := httptest.NewRecorder()
	guard.Middleware(http.HandlerFunc(handler.handleLoginPasskeyStart)).ServeHTTP(blocked, request)
	if blocked.Code != http.StatusForbidden || !strings.Contains(blocked.Body.String(), "cross-origin request blocked") {
		t.Fatalf("cross-origin passkey start = %d %q", blocked.Code, blocked.Body.String())
	}
	var challenges int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_challenges`).Scan(&challenges); err != nil || challenges != 0 {
		t.Fatalf("cross-origin passkey challenge count = %d, %v", challenges, err)
	}

	manager, _, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	withoutCSRF := postSecuritySettings(t, stack, securityPasskeyStepUpStartPath, url.Values{}, sessionCookie)
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("passkey step-up without CSRF = %d %q", withoutCSRF.Code, withoutCSRF.Body.String())
	}
	proof := csrfProofForSession(t, manager, sessionCookie.Value, securityPasskeyStepUpStartPath)
	withCSRF := postSecuritySettings(t, stack, securityPasskeyStepUpStartPath, url.Values{
		auth.CSRFFormFieldName: {proof},
	}, sessionCookie)
	if withCSRF.Code != http.StatusConflict || !strings.Contains(withCSRF.Body.String(), "No usable passkey") {
		t.Fatalf("passkey step-up without credential = %d %q", withCSRF.Code, withCSRF.Body.String())
	}
}
