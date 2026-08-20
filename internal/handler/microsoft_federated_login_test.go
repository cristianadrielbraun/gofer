package handler

import (
	"context"
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

const handlerMicrosoftTenantID = "11111111-2222-3333-4444-555555555555"

type handlerMicrosoftIDTokenVerifier struct {
	claims *auth.MicrosoftIDTokenClaims
}

func (verifier *handlerMicrosoftIDTokenVerifier) Verify(context.Context, string) (*auth.MicrosoftIDTokenClaims, error) {
	claims := *verifier.claims
	return &claims, nil
}

func microsoftLoginHandlerStack(
	t *testing.T,
	tokenURL string,
) (*auth.Manager, *storage.DB, http.Handler, *handlerMicrosoftIDTokenVerifier) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	issuer := "https://login.microsoftonline.com/" + handlerMicrosoftTenantID + "/v2.0"
	verifier := &handlerMicrosoftIDTokenVerifier{claims: &auth.MicrosoftIDTokenClaims{
		Issuer: issuer, Subject: "microsoft-subject", TenantID: handlerMicrosoftTenantID,
		Email: "person@microsoft.example", Name: "Person",
	}}
	manager := auth.NewManager(&auth.Config{
		Enabled: true, BaseURL: "https://gofer.example", SecureCookies: true,
		MicrosoftLoginTenant: "common",
		MicrosoftLoginClient: &oauth2.Config{
			ClientID: "microsoft-login-client", ClientSecret: "microsoft-login-secret",
			RedirectURL: "https://gofer.example/auth/microsoft/callback",
			Endpoint: oauth2.Endpoint{
				AuthURL: "https://login.example/authorize", TokenURL: tokenURL,
			},
		},
	}, db, auth.Dependencies{
		BucketHashKey: []byte("0123456789abcdef0123456789abcdef"), MicrosoftIDTokenVerifier: verifier,
	})
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, username, username_normalized, name, status, auth_version, created_at, updated_at
		) VALUES ('person', 'person', 'person', 'Gofer Person', 'active', 1, ?, ?);`,
		now, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_identities (
			id, user_id, provider, issuer, subject, email, email_verified, created_at, linked_at
		) VALUES ('microsoft-identity', 'person', 'microsoft', ?, 'microsoft-subject', 'old@example.com', 0, ?, ?)`,
		issuer, now, now,
	); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, auth: manager}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return manager, db, manager.Middleware(mux), verifier
}

func beginMicrosoftHandlerLogin(t *testing.T, stack http.Handler) (*http.Cookie, string, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/microsoft", nil)
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusTemporaryRedirect {
		t.Fatalf("Microsoft login start = %d %q", recorder.Code, recorder.Body.String())
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state, nonce := location.Query().Get("state"), location.Query().Get("nonce")
	if state == "" || nonce == "" || location.Query().Get("scope") != "openid profile email" ||
		location.Query().Get("code_challenge_method") != "S256" || location.Query().Get("code_challenge") == "" {
		t.Fatalf("Microsoft authorization query = %q", location.RawQuery)
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == "gofer_pre_auth" && cookie.MaxAge > 0 {
			return cookie, state, nonce
		}
	}
	t.Fatal("Microsoft login start omitted pre-authentication cookie")
	return nil, "", ""
}

func TestMicrosoftApplicationLoginHandlerCreatesOnlyGoferSession(t *testing.T) {
	exchanges := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges++
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("code") != "authorization-code" || r.Form.Get("code_verifier") == "" {
			t.Fatalf("Microsoft token request = %q", r.Form.Encode())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"identity-only-access","refresh_token":"must-not-persist","token_type":"Bearer","id_token":"signed-id-token"}`)
	}))
	defer provider.Close()
	_, db, stack, verifier := microsoftLoginHandlerStack(t, provider.URL)
	preAuthCookie, state, nonce := beginMicrosoftHandlerLogin(t, stack)
	verifier.claims.Nonce = nonce

	callback := httptest.NewRequest(
		http.MethodGet, "/auth/microsoft/callback?state="+url.QueryEscape(state)+"&code=authorization-code", nil,
	)
	callback.Host = "gofer.example"
	callback.AddCookie(preAuthCookie)
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, callback)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/" || exchanges != 1 {
		t.Fatalf("Microsoft callback = %d %q exchanges:%d body:%q", recorder.Code, recorder.Header().Get("Location"), exchanges, recorder.Body.String())
	}
	foundSessionCookie := false
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == "gofer_session" && cookie.Value != "" && cookie.MaxAge > 0 {
			foundSessionCookie = true
		}
	}
	if !foundSessionCookie {
		t.Fatal("Microsoft callback omitted the Gofer session cookie")
	}
	for table, want := range map[string]int{"accounts": 0, "oauth_accounts": 0, "sessions": 1, "auth_identities": 1} {
		var count int
		if err := db.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s rows = %d, %v; want %d", table, count, err, want)
		}
	}
	var method auth.AuthenticationMethod
	if err := db.Read().QueryRow(`SELECT authentication_method FROM sessions`).Scan(&method); err != nil || method != auth.AuthenticationMethodFederatedMicrosoft {
		t.Fatalf("Microsoft session method = %q, %v", method, err)
	}
}

func TestMicrosoftCallbackRejectsNonCanonicalHostBeforeCodeExchange(t *testing.T) {
	exchanges := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges++
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer provider.Close()
	_, _, stack, _ := microsoftLoginHandlerStack(t, provider.URL)
	preAuthCookie, state, _ := beginMicrosoftHandlerLogin(t, stack)
	callback := httptest.NewRequest(
		http.MethodGet, "/auth/microsoft/callback?state="+url.QueryEscape(state)+"&code=authorization-code", nil,
	)
	callback.Host = "localhost"
	callback.AddCookie(preAuthCookie)
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, callback)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login?error=auth_failed" || exchanges != 0 {
		t.Fatalf("non-canonical Microsoft callback = %d %q exchanges:%d", recorder.Code, recorder.Header().Get("Location"), exchanges)
	}
	assertPreAuthCookieCleared(t, recorder)
}

func TestMicrosoftCallbackDoesNotReflectProviderError(t *testing.T) {
	_, _, stack, _ := microsoftLoginHandlerStack(t, "https://login.example/token")
	preAuthCookie, state, _ := beginMicrosoftHandlerLogin(t, stack)
	callback := httptest.NewRequest(
		http.MethodGet,
		"/auth/microsoft/callback?state="+url.QueryEscape(state)+"&error="+url.QueryEscape("private provider detail"), nil,
	)
	callback.Host = "gofer.example"
	callback.AddCookie(preAuthCookie)
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, callback)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login?error=auth_failed" {
		t.Fatalf("Microsoft provider denial = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if strings.Contains(recorder.Body.String(), "private provider detail") || strings.Contains(recorder.Header().Get("Location"), "private") {
		t.Fatal("Microsoft provider detail was reflected to the browser")
	}
}

func TestMicrosoftLoginRoutesAreUnavailableWithoutApplicationLoginClient(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := auth.NewManager(&auth.Config{Enabled: true, BaseURL: "https://gofer.example"}, db)
	h := &Handler{db: db, auth: manager}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	stack := manager.Middleware(mux)
	for _, target := range []string{"/auth/microsoft", "/auth/microsoft/callback?state=unused&code=unused"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Host = "gofer.example"
		recorder := httptest.NewRecorder()
		stack.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", target, recorder.Code)
		}
	}
	var challenges int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&challenges); err != nil || challenges != 0 {
		t.Fatalf("Microsoft-disabled challenges = %d, %v", challenges, err)
	}
}
