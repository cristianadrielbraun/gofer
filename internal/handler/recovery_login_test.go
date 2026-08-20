package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/httpguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var renderedRecoveryCodePattern = regexp.MustCompile(`<code[^>]*>([0-9A-HJKMNP-TV-Z]{4}(?:-[0-9A-HJKMNP-TV-Z]{4}){5})</code>`)

func completedSetupWithRecoveryCode(t *testing.T) (*auth.Manager, *storage.DB, http.Handler, string) {
	t.Helper()
	manager, db, stack, setupCookie := prepareHandlerVerifiedMFA(t)
	generated := postSetupRecovery(stack, setupCookie, url.Values{"action": {"generate"}})
	if generated.Code != http.StatusOK {
		t.Fatalf("setup recovery generation = %d %q", generated.Code, generated.Body.String())
	}
	matches := renderedRecoveryCodePattern.FindAllStringSubmatch(generated.Body.String(), -1)
	if len(matches) != 10 {
		t.Fatalf("rendered setup recovery codes = %d, want 10", len(matches))
	}
	state, err := manager.GetSetupOwnerState(t.Context(), setupCookie.Value, "https://gofer.example")
	if err != nil || state == nil || state.Draft == nil {
		t.Fatalf("setup recovery state = %#v, %v", state, err)
	}
	acknowledged := postSetupRecovery(stack, setupCookie, url.Values{
		"action": {"acknowledge"}, "batch_id": {state.Draft.RecoveryBatchID}, "saved": {"yes"},
	})
	if acknowledged.Code != http.StatusSeeOther {
		t.Fatalf("setup recovery acknowledgement = %d %q", acknowledged.Code, acknowledged.Body.String())
	}
	completed := postSetupReview(stack, setupCookie, url.Values{"action": {"complete"}})
	if completed.Code != http.StatusSeeOther {
		t.Fatalf("setup completion = %d %q", completed.Code, completed.Body.String())
	}
	return manager, db, stack, matches[0][1]
}

func startOwnerPasswordMFA(t *testing.T, stack http.Handler, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{
		"identifier": {"owner"},
		"password":   {"correct horse battery staple for owner"},
	}
	request := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	request.Header.Set("User-Agent", "Recovery Flow Browser/1.0")
	request.RemoteAddr = "198.51.100.91:51230"
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	return recorder
}

func postRecoveryRoute(stack http.Handler, path string, cookie *http.Cookie, values url.Values, otherCookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	request.Header.Set("User-Agent", "Recovery Flow Browser/1.0")
	request.RemoteAddr = "198.51.100.91:51230"
	if cookie != nil {
		request.AddCookie(cookie)
	}
	for _, other := range otherCookies {
		request.AddCookie(other)
	}
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	return recorder
}

func TestRecoveryCodeLoginRequiresFactorRepairBeforeIssuingSession(t *testing.T) {
	manager, db, stack, recoveryCode := completedSetupWithRecoveryCode(t)
	returnRecorder := httptest.NewRecorder()
	auth.SetReturnToCookie(returnRecorder, "/admin/account/security?from=recovery", true)
	returnCookie := responseCookie(returnRecorder, "gofer_auth_return_to", true)
	password := startOwnerPasswordMFA(t, stack, returnCookie)
	if password.Code != http.StatusSeeOther || password.Header().Get("Location") != "/login/mfa" {
		t.Fatalf("password MFA start = %d location:%q", password.Code, password.Header().Get("Location"))
	}
	mfaCookie := responseCookie(password, "gofer_pre_auth", true)
	if mfaCookie == nil {
		t.Fatal("password MFA start omitted pre-authentication cookie")
	}

	request := httptest.NewRequest(http.MethodGet, "/login/mfa/recovery", nil)
	request.AddCookie(mfaCookie)
	page := httptest.NewRecorder()
	stack.ServeHTTP(page, request)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Use a recovery code") ||
		!strings.Contains(page.Body.String(), `action="/login/mfa/recovery"`) || strings.Contains(page.Body.String(), recoveryCode) ||
		page.Header().Get("Cache-Control") != "no-store" || page.Header().Get("Referrer-Policy") != "no-referrer" ||
		page.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("recovery-code page = %d %q", page.Code, page.Body.String())
	}

	invalid := postRecoveryRoute(stack, "/login/mfa/recovery", mfaCookie, url.Values{"code": {"AAAA-BBBB-CCCC-DDDD-EEEE-FFFF"}})
	if invalid.Code != http.StatusUnauthorized || !strings.Contains(invalid.Body.String(), recoveryCodeFailureMessage) ||
		responseCookie(invalid, "gofer_session", true) != nil {
		t.Fatalf("invalid recovery code = %d cookies:%#v body:%q", invalid.Code, invalid.Result().Cookies(), invalid.Body.String())
	}
	valid := postRecoveryRoute(stack, "/login/mfa/recovery", mfaCookie, url.Values{"code": {strings.ToLower(recoveryCode)}})
	if valid.Code != http.StatusSeeOther || valid.Header().Get("Location") != "/login/recovery/mfa" ||
		responseCookie(valid, "gofer_session", true) != nil {
		t.Fatalf("valid recovery code = %d location:%q cookies:%#v", valid.Code, valid.Header().Get("Location"), valid.Result().Cookies())
	}
	repairCookie := responseCookie(valid, "gofer_pre_auth", true)
	if repairCookie == nil || repairCookie.Value == mfaCookie.Value || repairCookie.MaxAge != 900 {
		t.Fatalf("rotated repair cookie = %#v, previous %#v", repairCookie, mfaCookie)
	}
	var sessionsBeforeRepair int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessionsBeforeRepair); err != nil {
		t.Fatal(err)
	}
	if sessionsBeforeRepair != 1 {
		t.Fatalf("recovery code created a premature session; total = %d", sessionsBeforeRepair)
	}

	request = httptest.NewRequest(http.MethodGet, "/login/recovery/mfa", nil)
	request.AddCookie(repairCookie)
	mfaPage := httptest.NewRecorder()
	stack.ServeHTTP(mfaPage, request)
	if mfaPage.Code != http.StatusOK || !strings.Contains(mfaPage.Body.String(), "Replace your authenticator") ||
		!strings.Contains(mfaPage.Body.String(), "data:image/png;base64,") || strings.Contains(mfaPage.Body.String(), "https://") {
		t.Fatalf("repair MFA page = %d %q", mfaPage.Code, mfaPage.Body.String())
	}
	repairState, err := manager.GetRecoveryRepairState(t.Context(), repairCookie.Value, "https://gofer.example")
	if err != nil || repairState == nil || repairState.Enrollment == nil {
		t.Fatalf("repair state = %#v, %v", repairState, err)
	}
	newSecret := strings.ReplaceAll(repairState.Enrollment.ManualKey, " ", "")
	confirmed := postRecoveryRoute(stack, "/login/recovery/mfa", repairCookie, url.Values{
		"action": {"confirm"}, "code": {setupHandlerTOTPCode(t, newSecret)},
	})
	if confirmed.Code != http.StatusSeeOther || confirmed.Header().Get("Location") != "/login/recovery/codes" ||
		responseCookie(confirmed, "gofer_session", true) != nil {
		t.Fatalf("replacement TOTP confirmation = %d location:%q cookies:%#v body:%q", confirmed.Code, confirmed.Header().Get("Location"), confirmed.Result().Cookies(), confirmed.Body.String())
	}

	generated := postRecoveryRoute(stack, "/login/recovery/codes", repairCookie, url.Values{"action": {"generate"}})
	matches := renderedRecoveryCodePattern.FindAllStringSubmatch(generated.Body.String(), -1)
	if generated.Code != http.StatusOK || len(matches) != 10 ||
		!strings.Contains(generated.Body.String(), "shown only in this response") || responseCookie(generated, "gofer_session", true) != nil {
		t.Fatalf("fresh recovery-code generation = %d codes:%d cookies:%#v body:%q", generated.Code, len(matches), generated.Result().Cookies(), generated.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/login/recovery/codes", nil)
	request.AddCookie(repairCookie)
	refreshed := httptest.NewRecorder()
	stack.ServeHTTP(refreshed, request)
	if refreshed.Code != http.StatusOK || renderedRecoveryCodePattern.MatchString(refreshed.Body.String()) ||
		!strings.Contains(refreshed.Body.String(), "already shown") {
		t.Fatalf("refreshed recovery-code page = %d %q", refreshed.Code, refreshed.Body.String())
	}
	repairState, err = manager.GetRecoveryRepairState(t.Context(), repairCookie.Value, "https://gofer.example")
	if err != nil || !repairState.RecoveryGenerated || repairState.RecoveryBatchID == "" {
		t.Fatalf("generated repair state = %#v, %v", repairState, err)
	}
	completed := postRecoveryRoute(stack, "/login/recovery/codes", repairCookie, url.Values{
		"action": {"complete"}, "batch_id": {repairState.RecoveryBatchID}, "saved": {"yes"},
	}, returnCookie)
	if completed.Code != http.StatusSeeOther || completed.Header().Get("Location") != "/admin/account/security?from=recovery" {
		t.Fatalf("recovery completion = %d location:%q body:%q", completed.Code, completed.Header().Get("Location"), completed.Body.String())
	}
	sessionCookie := responseCookie(completed, "gofer_session", true)
	if sessionCookie == nil || !sessionCookie.HttpOnly || !sessionCookie.Secure {
		t.Fatalf("recovery session cookie = %#v", sessionCookie)
	}
	if cleared := responseCookie(completed, "gofer_pre_auth", false); cleared == nil || cleared.MaxAge != -1 {
		t.Fatalf("recovery pre-auth cleanup = %#v", cleared)
	}
	session, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || session == nil || session.AssuranceLevel != auth.AssuranceLevelMultiFactor ||
		session.StepUpMethod != auth.AuthenticationMethodRecoveryCode || session.UserAgent != "Recovery Flow Browser/1.0" {
		t.Fatalf("recovery session = %#v, %v", session, err)
	}
}

func TestRecoveryCodeSubmissionIsOriginProtectedAndBounded(t *testing.T) {
	manager, db, stack, recoveryCode := completedSetupWithRecoveryCode(t)
	password := startOwnerPasswordMFA(t, stack)
	mfaCookie := responseCookie(password, "gofer_pre_auth", true)
	if mfaCookie == nil {
		t.Fatal("recovery origin test omitted MFA cookie")
	}
	handler := &Handler{db: db, auth: manager}
	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"code": {recoveryCode}}
	request := httptest.NewRequest(http.MethodPost, "/login/mfa/recovery", strings.NewReader(form.Encode()))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	request.AddCookie(mfaCookie)
	recorder := httptest.NewRecorder()
	guard.Middleware(http.HandlerFunc(handler.handleRecoveryCodeLoginSubmit)).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "cross-origin request blocked") ||
		strings.Contains(recorder.Body.String(), recoveryCode) {
		t.Fatalf("cross-origin recovery submission = %d %q", recorder.Code, recorder.Body.String())
	}
	var attempts, used int
	if err := db.Read().QueryRow(`SELECT attempts FROM auth_challenges WHERE purpose = 'mfa' AND consumed_at IS NULL`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM recovery_codes WHERE used_at IS NOT NULL`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || used != 0 {
		t.Fatalf("cross-origin recovery mutation = attempts:%d used:%d", attempts, used)
	}

	request = httptest.NewRequest(http.MethodPost, "/login/mfa/recovery", io.LimitReader(
		strings.NewReader(strings.Repeat("x", recoveryLoginFormMaximumBytes+1)), recoveryLoginFormMaximumBytes+1,
	))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(mfaCookie)
	recorder = httptest.NewRecorder()
	handler.handleRecoveryCodeLoginSubmit(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), recoveryCodeFailureMessage) ||
		strings.Contains(recorder.Body.String(), strings.Repeat("x", 32)) {
		t.Fatalf("oversized recovery form = %d %q", recorder.Code, recorder.Body.String())
	}
	if err := db.Read().QueryRow(`SELECT attempts FROM auth_challenges WHERE purpose = 'mfa' AND consumed_at IS NULL`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("oversized recovery form advanced attempts to %d", attempts)
	}
}

func TestRecoveryRoutesStripQueriesAndRejectMissingState(t *testing.T) {
	handler, _, _ := newLocalLoginHandler(t, auth.UserStatusActive, false, false, false, false)
	tests := []struct {
		path     string
		location string
		handler  http.HandlerFunc
	}{
		{path: "/login/mfa/recovery?code=secret", location: "/login/mfa/recovery", handler: handler.handleRecoveryCodeLogin},
		{path: "/login/recovery/mfa?code=secret", location: "/login/recovery/mfa", handler: handler.handleRecoveryRepairMFA},
		{path: "/login/recovery/codes?code=secret", location: "/login/recovery/codes", handler: handler.handleRecoveryRepairCodes},
	}
	for _, test := range tests {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		recorder := httptest.NewRecorder()
		test.handler(recorder, request)
		if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != test.location || strings.Contains(recorder.Body.String(), "secret") {
			t.Fatalf("query stripping for %s = %d location:%q body:%q", test.path, recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/login/mfa/recovery", nil)
	recorder := httptest.NewRecorder()
	handler.handleRecoveryCodeLogin(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login" {
		t.Fatalf("missing recovery MFA state = %d location:%q", recorder.Code, recorder.Header().Get("Location"))
	}
	request = httptest.NewRequest(http.MethodGet, "/login/recovery/mfa", nil)
	recorder = httptest.NewRecorder()
	handler.handleRecoveryRepairMFA(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login?error=recovery" {
		t.Fatalf("missing repair state = %d location:%q", recorder.Code, recorder.Header().Get("Location"))
	}
}
