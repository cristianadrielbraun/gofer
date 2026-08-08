package handler

import (
	"crypto/sha256"
	"fmt"
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
)

func setupEntryStack(t *testing.T) (*auth.Manager, *storage.DB, http.Handler, string) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := auth.NewManager(&auth.Config{
		Enabled: true, BaseURL: "https://gofer.example", SecureCookies: true,
	}, db, auth.Dependencies{BucketHashKey: []byte("setup-handler-test-key-32-bytes!")})
	provision, err := manager.EnsureSetupToken(t.Context(), "")
	if err != nil || provision == nil || provision.Token == "" {
		t.Fatalf("EnsureSetupToken() = %#v, %v", provision, err)
	}
	handler := &Handler{db: db, auth: manager}
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	return manager, db, manager.Middleware(mux), provision.Token
}

func postSetup(stack http.Handler, token string) *httptest.ResponseRecorder {
	form := url.Values{"token": {token}}
	request := httptest.NewRequest(http.MethodPost, setupPath, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	request.Header.Set("User-Agent", "Setup Handler Test/1.0")
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	return recorder
}

func TestSetupEntryIsPublicLocalNoStoreAndSecretFree(t *testing.T) {
	_, _, stack, setupToken := setupEntryStack(t)
	request := httptest.NewRequest(http.MethodGet, setupPath, nil)
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("setup entry = %d %q", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, required := range []string{
		`action="/setup"`, `name="token"`, `type="password"`,
		`autocomplete="one-time-code"`, "never included in the page URL", "browser storage",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("setup entry missing %q", required)
		}
	}
	if strings.Contains(body, setupToken) || strings.Contains(body, "fonts.googleapis.com") || strings.Contains(body, "fonts.gstatic.com") {
		t.Fatal("setup entry exposed a token or requested a remote font")
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Referrer-Policy") != "no-referrer" || recorder.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("setup entry security headers = cache:%q referrer:%q robots:%q", recorder.Header().Get("Cache-Control"), recorder.Header().Get("Referrer-Policy"), recorder.Header().Get("X-Robots-Tag"))
	}
}

func TestSetupTokenVerificationCreatesProtectedContinuationWithoutCompletingSetup(t *testing.T) {
	manager, db, stack, setupToken := setupEntryStack(t)
	recorder := postSetup(stack, setupToken)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != setupOwnerPath {
		t.Fatalf("successful setup verification = %d location:%q body:%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	setupCookie := responseCookie(recorder, "gofer_pre_auth", true)
	if setupCookie == nil || setupCookie.Value == "" || setupCookie.Value == setupToken || !setupCookie.HttpOnly || !setupCookie.Secure || setupCookie.SameSite != http.SameSiteLaxMode || setupCookie.MaxAge != 600 || setupCookie.Path != "/" {
		t.Fatalf("setup access cookie = %#v", setupCookie)
	}
	for _, cookieName := range []string{"gofer_session", "gofer_auth_return_to"} {
		cleared := responseCookie(recorder, cookieName, false)
		if cleared == nil || cleared.MaxAge != -1 {
			t.Fatalf("setup verification did not clear %s: %#v", cookieName, cleared)
		}
	}
	if strings.Contains(recorder.Body.String(), setupToken) || strings.Contains(recorder.Body.String(), setupCookie.Value) {
		t.Fatal("setup verification response exposed a bearer secret")
	}

	var initialized int
	var setupHash, challengeHash string
	var attempts int64
	if err := db.Read().QueryRow(`
		SELECT initialized, setup_token_hash, setup_attempts
		FROM auth_system_state WHERE id = 1`,
	).Scan(&initialized, &setupHash, &attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`
		SELECT challenge_hash FROM auth_challenges
		WHERE purpose = 'enrollment' AND user_id IS NULL AND consumed_at IS NULL`,
	).Scan(&challengeHash); err != nil {
		t.Fatal(err)
	}
	wantSetupHash := sha256.Sum256([]byte(setupToken))
	wantChallengeHash := sha256.Sum256([]byte(setupCookie.Value))
	if initialized != 0 || setupHash != fmt.Sprintf("%x", wantSetupHash) || attempts != 0 ||
		challengeHash != fmt.Sprintf("%x", wantChallengeHash) || challengeHash == setupCookie.Value {
		t.Fatalf("setup verification persistence = initialized:%d setupHash:%q attempts:%d challengeHash:%q", initialized, setupHash, attempts, challengeHash)
	}
	active, err := manager.GetActiveSetupAccess(t.Context(), setupCookie.Value, "https://gofer.example")
	if err != nil || active == nil {
		t.Fatalf("GetActiveSetupAccess() = %#v, %v", active, err)
	}
	request := httptest.NewRequest(http.MethodGet, setupPath, nil)
	request.AddCookie(setupCookie)
	alreadyVerified := httptest.NewRecorder()
	stack.ServeHTTP(alreadyVerified, request)
	if alreadyVerified.Code != http.StatusSeeOther || alreadyVerified.Header().Get("Location") != setupOwnerPath {
		t.Fatalf("verified setup entry = %d location:%q", alreadyVerified.Code, alreadyVerified.Header().Get("Location"))
	}

	request = httptest.NewRequest(http.MethodGet, setupOwnerPath, nil)
	request.AddCookie(setupCookie)
	ownerRecorder := httptest.NewRecorder()
	stack.ServeHTTP(ownerRecorder, request)
	if ownerRecorder.Code != http.StatusOK || !strings.Contains(ownerRecorder.Body.String(), "Setup access verified") || !strings.Contains(ownerRecorder.Body.String(), "has not been consumed") || strings.Contains(ownerRecorder.Body.String(), setupToken) || strings.Contains(ownerRecorder.Body.String(), setupCookie.Value) {
		t.Fatalf("protected setup owner page = %d %q", ownerRecorder.Code, ownerRecorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, setupOwnerPath, nil)
	withoutCookie := httptest.NewRecorder()
	stack.ServeHTTP(withoutCookie, request)
	if withoutCookie.Code != http.StatusSeeOther || withoutCookie.Header().Get("Location") != setupPath {
		t.Fatalf("unverified setup owner request = %d location:%q", withoutCookie.Code, withoutCookie.Header().Get("Location"))
	}
	cleared := responseCookie(withoutCookie, "gofer_pre_auth", false)
	if cleared == nil || cleared.MaxAge != -1 {
		t.Fatalf("unverified setup owner request did not clear stale cookie: %#v", cleared)
	}
}

func TestSetupTokenFailuresAreIndistinguishableAndDoNotEchoSecrets(t *testing.T) {
	type testCase struct {
		name  string
		alter func(*storage.DB)
		token func(string) string
	}
	tests := []testCase{
		{name: "unknown", token: func(string) string { return "unknown-private-setup-token" }},
		{name: "expired", alter: func(db *storage.DB) {
			_, _ = db.Write().Exec(`UPDATE auth_system_state SET setup_expires_at = ? WHERE id = 1`, time.Now().UTC().Add(-time.Minute))
		}},
		{name: "blocked", alter: func(db *storage.DB) {
			_, _ = db.Write().Exec(`UPDATE auth_system_state SET setup_attempts = 10 WHERE id = 1`)
		}},
	}
	var referenceBody string
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, db, stack, setupToken := setupEntryStack(t)
			if test.alter != nil {
				test.alter(db)
			}
			submitted := setupToken
			if test.token != nil {
				submitted = test.token(setupToken)
			}
			recorder := postSetup(stack, submitted)
			if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), setupTokenFailureMessage) {
				t.Fatalf("generic setup failure = %d %q", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), submitted) || strings.Contains(recorder.Body.String(), setupToken) {
				t.Fatal("setup failure echoed submitted or stored credentials")
			}
			if responseCookie(recorder, "gofer_pre_auth", true) != nil {
				t.Fatal("setup failure issued an access cookie")
			}
			if referenceBody == "" {
				referenceBody = recorder.Body.String()
			} else if recorder.Body.String() != referenceBody {
				t.Fatal("setup failure body differs by token state")
			}
			var challenges int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&challenges); err != nil || challenges != 0 {
				t.Fatalf("rejected setup challenge count = %d, %v", challenges, err)
			}
		})
	}
}

func TestSetupRoutesDisappearAfterInitialization(t *testing.T) {
	_, db, stack, setupToken := setupEntryStack(t)
	if _, err := db.Write().Exec(`
		UPDATE auth_system_state
		SET initialized = 1, setup_token_hash = NULL, setup_expires_at = NULL
		WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: setupPath},
		{method: http.MethodPost, path: setupPath},
		{method: http.MethodGet, path: setupOwnerPath},
	} {
		var request *http.Request
		if test.method == http.MethodPost {
			form := url.Values{"token": {setupToken}}
			request = httptest.NewRequest(test.method, test.path, strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			request = httptest.NewRequest(test.method, test.path, nil)
		}
		recorder := httptest.NewRecorder()
		stack.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound || strings.Contains(recorder.Body.String(), setupToken) || recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("initialized %s %s = %d %q", test.method, test.path, recorder.Code, recorder.Body.String())
		}
	}
	var challenges, events int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&challenges); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE event_type LIKE 'setup_token_verification%' OR event_type = 'setup_token_verified'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if challenges != 0 || events != 0 {
		t.Fatalf("initialized setup routes mutated state = challenges:%d events:%d", challenges, events)
	}
}

func TestSetupPostIsProtectedByCanonicalOriginGuard(t *testing.T) {
	_, db, stack, setupToken := setupEntryStack(t)
	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"token": {setupToken}}
	request := httptest.NewRequest(http.MethodPost, setupPath, strings.NewReader(form.Encode()))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	recorder := httptest.NewRecorder()
	guard.Middleware(stack).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "cross-origin request blocked") {
		t.Fatalf("cross-origin setup = %d %q", recorder.Code, recorder.Body.String())
	}
	var attempts, challenges int
	if err := db.Read().QueryRow(`SELECT setup_attempts FROM auth_system_state WHERE id = 1`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&challenges); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || challenges != 0 {
		t.Fatalf("cross-origin setup mutation = attempts:%d challenges:%d", attempts, challenges)
	}
}

func TestSetupAcceptsPrivacyBrowserNullOriginWithSameOriginMetadata(t *testing.T) {
	_, _, stack, setupToken := setupEntryStack(t)
	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"token": {setupToken}}
	request := httptest.NewRequest(http.MethodPost, setupPath, strings.NewReader(form.Encode()))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "null")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	recorder := httptest.NewRecorder()
	guard.Middleware(stack).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != setupOwnerPath || responseCookie(recorder, "gofer_pre_auth", true) == nil {
		t.Fatalf("null-origin same-origin setup = %d location:%q body:%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
}

func TestSetupRejectsOversizedFormWithoutAdvancingAttempts(t *testing.T) {
	_, db, stack, setupToken := setupEntryStack(t)
	request := httptest.NewRequest(http.MethodPost, setupPath, strings.NewReader("token="+strings.Repeat("x", setupFormMaximumBytes+1)))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), setupTokenFailureMessage) || strings.Contains(recorder.Body.String(), setupToken) {
		t.Fatalf("oversized setup form = %d %q", recorder.Code, recorder.Body.String())
	}
	var attempts, challenges int
	if err := db.Read().QueryRow(`SELECT setup_attempts FROM auth_system_state WHERE id = 1`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&challenges); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || challenges != 0 {
		t.Fatalf("oversized setup mutation = attempts:%d challenges:%d", attempts, challenges)
	}
}

func TestSetupTokenIsIgnoredInQueryString(t *testing.T) {
	_, db, stack, setupToken := setupEntryStack(t)
	request := httptest.NewRequest(http.MethodPost, setupPath+"?token="+url.QueryEscape(setupToken), strings.NewReader(""))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != setupPath || strings.Contains(recorder.Body.String(), setupToken) || responseCookie(recorder, "gofer_pre_auth", true) != nil || recorder.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("query-string setup token = %d %q", recorder.Code, recorder.Body.String())
	}
	var attempts, challenges int
	if err := db.Read().QueryRow(`SELECT setup_attempts FROM auth_system_state WHERE id = 1`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&challenges); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || challenges != 0 {
		t.Fatalf("query-string setup handling = attempts:%d challenges:%d", attempts, challenges)
	}

	request = httptest.NewRequest(http.MethodGet, setupPath+"?token="+url.QueryEscape(setupToken), nil)
	recorder = httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != setupPath || strings.Contains(recorder.Body.String(), setupToken) || recorder.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("query-string setup GET = %d %q", recorder.Code, recorder.Body.String())
	}
}
