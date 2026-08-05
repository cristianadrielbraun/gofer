package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

func newPreAuthHandler(t *testing.T, tokenURL string) (*Handler, *storage.DB) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := auth.NewManager(&auth.Config{
		Enabled:       true,
		BaseURL:       "https://gofer.example",
		SecureCookies: true,
		GoogleClient: &oauth2.Config{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			RedirectURL:  "https://gofer.example/auth/google/callback",
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://accounts.example/authorize",
				TokenURL: tokenURL,
			},
		},
	}, db)
	return &Handler{db: db, auth: manager}, db
}

func beginGooglePreAuth(t *testing.T, handler *Handler) (*http.Cookie, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/auth/google", nil)
	recorder := httptest.NewRecorder()
	handler.handleGoogleRedirect(recorder, request)
	if recorder.Code != http.StatusTemporaryRedirect {
		t.Fatalf("Google redirect status = %d", recorder.Code)
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization redirect: %v", err)
	}
	state := location.Query().Get("state")
	if state == "" {
		t.Fatal("authorization redirect omitted state")
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == "gofer_pre_auth" && cookie.MaxAge > 0 {
			continue
		}
		if cookie.Name == "oauth_state" && cookie.MaxAge != -1 {
			t.Fatal("Google redirect did not clear the legacy OAuth state cookie")
		}
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == "gofer_pre_auth" && cookie.MaxAge > 0 {
			return cookie, state
		}
	}
	t.Fatal("Google redirect omitted pre-authentication cookie")
	return nil, ""
}

func TestGoogleRedirectTerminatesPreviousPreAuthChallenge(t *testing.T) {
	handler, db := newPreAuthHandler(t, "https://accounts.example/token")
	firstCookie, firstState := beginGooglePreAuth(t, handler)
	request := httptest.NewRequest(http.MethodGet, "/auth/google", nil)
	request.AddCookie(firstCookie)
	recorder := httptest.NewRecorder()
	handler.handleGoogleRedirect(recorder, request)
	if recorder.Code != http.StatusTemporaryRedirect {
		t.Fatalf("replacement Google redirect status = %d", recorder.Code)
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse replacement authorization redirect: %v", err)
	}
	secondState := location.Query().Get("state")
	if secondState == "" || secondState == firstState {
		t.Fatalf("replacement states = first:%q second:%q", firstState, secondState)
	}
	var total, active, consumed int
	if err := db.Read().QueryRow(`
		SELECT COUNT(*),
		       SUM(CASE WHEN consumed_at IS NULL THEN 1 ELSE 0 END),
		       SUM(CASE WHEN consumed_at IS NOT NULL THEN 1 ELSE 0 END)
		FROM auth_challenges`).Scan(&total, &active, &consumed); err != nil {
		t.Fatalf("query replacement challenges: %v", err)
	}
	if total != 2 || active != 1 || consumed != 1 {
		t.Fatalf("replacement challenges = total:%d active:%d consumed:%d", total, active, consumed)
	}
}

func TestGoogleRedirectFailsClosedWhenPreviousChallengeCannotBeTerminated(t *testing.T) {
	handler, db := newPreAuthHandler(t, "https://accounts.example/token")
	firstCookie, _ := beginGooglePreAuth(t, handler)
	if _, err := db.Write().Exec(`
		CREATE TRIGGER reject_pre_auth_termination
		BEFORE UPDATE OF consumed_at ON auth_challenges
		BEGIN
			SELECT RAISE(ABORT, 'forced challenge termination failure');
		END`); err != nil {
		t.Fatalf("create termination failure trigger: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/auth/google", nil)
	request.AddCookie(firstCookie)
	recorder := httptest.NewRecorder()
	handler.handleGoogleRedirect(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("replacement Google redirect status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	assertPreAuthCookieCleared(t, recorder)
	var count int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("challenge count after failed termination = %d, %v; want 1", count, err)
	}
}

func TestGoogleRedirectCreatesHashOnlyPreAuthChallenge(t *testing.T) {
	handler, db := newPreAuthHandler(t, "https://accounts.example/token")
	cookie, state := beginGooglePreAuth(t, handler)
	if cookie.Value != state || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 600 {
		t.Fatalf("pre-authentication cookie = %#v state=%q", cookie, state)
	}
	hash := sha256.Sum256([]byte(state))
	wantHash := hex.EncodeToString(hash[:])
	var storedHash, purpose, origin string
	var active bool
	if err := db.Read().QueryRow(`
		SELECT challenge_hash, purpose, origin, consumed_at IS NULL
		FROM auth_challenges`).Scan(&storedHash, &purpose, &origin, &active); err != nil {
		t.Fatalf("query pre-authentication challenge: %v", err)
	}
	if storedHash != wantHash || storedHash == state || purpose != string(auth.ChallengePurposeFederatedLogin) || origin != "https://gofer.example" || !active {
		t.Fatalf("stored challenge = hash:%q purpose:%q origin:%q active:%t", storedHash, purpose, origin, active)
	}
}

func TestGoogleCallbackTerminatesMismatchedStateAndClearsCookie(t *testing.T) {
	handler, db := newPreAuthHandler(t, "https://accounts.example/token")
	cookie, _ := beginGooglePreAuth(t, handler)
	request := httptest.NewRequest(http.MethodGet, "/auth/google/callback?state=wrong-state&code=unused", nil)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.handleGoogleCallback(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login?error=invalid_state" {
		t.Fatalf("mismatched callback = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	assertPreAuthCookieCleared(t, recorder)
	var attempts int
	var consumed bool
	if err := db.Read().QueryRow(`SELECT attempts, consumed_at IS NOT NULL FROM auth_challenges`).Scan(&attempts, &consumed); err != nil {
		t.Fatalf("query terminated challenge: %v", err)
	}
	if attempts != 1 || !consumed {
		t.Fatalf("terminated challenge = attempts:%d consumed:%t", attempts, consumed)
	}
}

func TestGoogleCallbackConsumesStateOnceBeforeCodeExchange(t *testing.T) {
	exchanges := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges++
		http.Error(w, "rejected", http.StatusBadRequest)
	}))
	defer provider.Close()
	handler, db := newPreAuthHandler(t, provider.URL)
	cookie, state := beginGooglePreAuth(t, handler)

	request := httptest.NewRequest(http.MethodGet, "/auth/google/callback?state="+url.QueryEscape(state)+"&code=provider-code", nil)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.handleGoogleCallback(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login?error=auth_failed" || exchanges == 0 {
		t.Fatalf("first callback = %d %q exchanges:%d", recorder.Code, recorder.Header().Get("Location"), exchanges)
	}
	assertPreAuthCookieCleared(t, recorder)
	firstExchanges := exchanges

	request = httptest.NewRequest(http.MethodGet, "/auth/google/callback?state="+url.QueryEscape(state)+"&code=provider-code", nil)
	request.AddCookie(cookie)
	recorder = httptest.NewRecorder()
	handler.handleGoogleCallback(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login?error=invalid_state" || exchanges != firstExchanges {
		t.Fatalf("replayed callback = %d %q exchanges:%d", recorder.Code, recorder.Header().Get("Location"), exchanges)
	}
	var attempts int
	var consumed bool
	if err := db.Read().QueryRow(`SELECT attempts, consumed_at IS NOT NULL FROM auth_challenges`).Scan(&attempts, &consumed); err != nil {
		t.Fatalf("query consumed challenge: %v", err)
	}
	if attempts != 1 || !consumed {
		t.Fatalf("consumed challenge = attempts:%d consumed:%t", attempts, consumed)
	}
}

func TestGoogleCallbackRejectsExpiredChallengeBeforeCodeExchange(t *testing.T) {
	exchanges := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges++
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer provider.Close()
	handler, db := newPreAuthHandler(t, provider.URL)
	cookie, state := beginGooglePreAuth(t, handler)
	if _, err := db.Write().Exec(`UPDATE auth_challenges SET expires_at = datetime('now', '-1 second')`); err != nil {
		t.Fatalf("expire challenge: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/auth/google/callback?state="+url.QueryEscape(state)+"&code=provider-code", nil)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.handleGoogleCallback(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login?error=invalid_state" || exchanges != 0 {
		t.Fatalf("expired callback = %d %q exchanges:%d", recorder.Code, recorder.Header().Get("Location"), exchanges)
	}
	assertPreAuthCookieCleared(t, recorder)
}

func assertPreAuthCookieCleared(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == "gofer_pre_auth" {
			if cookie.MaxAge != -1 {
				t.Fatalf("pre-authentication cookie was not cleared: %#v", cookie)
			}
			return
		}
	}
	t.Fatal("response omitted pre-authentication cookie cleanup")
}
