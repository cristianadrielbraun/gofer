package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

func newAccountOAuthFlowTestHandler(t *testing.T) (*Handler, *auth.Manager, *storage.DB) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := auth.NewManager(&auth.Config{
		Enabled: true,
		BaseURL: "https://gofer.example",
		GoogleClient: &oauth2.Config{
			ClientID: "client-id",
			Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example/authorize"},
		},
	}, db)
	return &Handler{db: db, auth: manager}, manager, db
}

func accountOAuthUserRequest(req *http.Request, userID, sessionToken string) *http.Request {
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: sessionToken})
	return req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: userID, Email: userID + "@example.com"}))
}

func TestHandleAccountOAuthAuthorizeStoresServerSideFlow(t *testing.T) {
	h, manager, db := newAccountOAuthFlowTestHandler(t)
	if _, err := db.Write().Exec(`INSERT INTO users (id, email, name) VALUES ('user', 'user@example.com', 'User')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	form := url.Values{
		"provider":      {providers.ProviderGmail},
		"email_address": {"user@gmail.com"},
		"display_name":  {"User Gmail"},
		"flow_action":   {"add"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/oauth2/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = accountOAuthUserRequest(req, "user", "session-token")
	rec := httptest.NewRecorder()

	h.handleAccountOAuthAuthorize(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body = %q, want redirect", rec.Code, rec.Body.String())
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	state := location.Query().Get("state")
	if state == "" {
		t.Fatal("OAuth redirect is missing state")
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == "oauth_account_state" || cookie.Name == "oauth_account_form" {
			t.Fatalf("legacy OAuth account cookie %q was set", cookie.Name)
		}
	}
	flow, err := manager.ConsumeAccountOAuthFlow(req.Context(), state, "user", "session-token", providers.ProviderGmail)
	if err != nil {
		t.Fatalf("ConsumeAccountOAuthFlow() error = %v", err)
	}
	if flow.FormData["email_address"] != "user@gmail.com" || flow.FormData["display_name"] != "User Gmail" || flow.FormData["flow_action"] != "add" {
		t.Fatalf("stored form data = %#v", flow.FormData)
	}
}

func TestAccountOAuthSuccessRedirectMatchesFlowAction(t *testing.T) {
	if got := accountOAuthSuccessRedirect(map[string]string{"flow_action": "add"}); got != "/settings/accounts?account_added=1" {
		t.Fatalf("add redirect = %q", got)
	}
	if got := accountOAuthSuccessRedirect(map[string]string{"flow_action": "reconnect"}); got != "/settings/accounts?account_reconnected=1" {
		t.Fatalf("reconnect redirect = %q", got)
	}
	if got := accountOAuthSuccessRedirect(nil); got != "/settings/accounts?account_added=1" {
		t.Fatalf("default redirect = %q", got)
	}
}

func TestReadAccountOAuthCallbackUsesBoundFlowUser(t *testing.T) {
	h, manager, db := newAccountOAuthFlowTestHandler(t)
	if _, err := db.Write().Exec(`INSERT INTO users (id, email, name) VALUES ('user', 'user@example.com', 'User')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	state, err := manager.CreateAccountOAuthFlow(t.Context(), "user", "session-token", providers.ProviderGmail, map[string]string{"email_address": "user@gmail.com"})
	if err != nil {
		t.Fatalf("CreateAccountOAuthFlow() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/auth/google/account/callback?state="+url.QueryEscape(state)+"&code=auth-code", nil)
	req = accountOAuthUserRequest(req, "user", "session-token")
	rec := httptest.NewRecorder()

	flow, code, ok := h.readAccountOAuthCallback(rec, req, "test callback", providers.ProviderGmail)

	if !ok || code != "auth-code" {
		t.Fatalf("readAccountOAuthCallback() ok = %v code = %q response = %q", ok, code, rec.Body.String())
	}
	if flow.UserID != "user" || flow.FormData["email_address"] != "user@gmail.com" {
		t.Fatalf("callback flow = %#v", flow)
	}
}
