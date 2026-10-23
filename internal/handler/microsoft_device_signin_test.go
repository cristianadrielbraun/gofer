package handler

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
)

func newMicrosoftDeviceFixture(t *testing.T) *userOAuthFixture {
	t.Helper()
	var f *userOAuthFixture
	f = newUserOAuthFixtureWithConfig(t, func(w http.ResponseWriter, r *http.Request) bool {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/devicecode":
			_, _ = w.Write([]byte(`{"device_code":"device-secret","user_code":"WXYZ-9876","verification_uri":"https://microsoft.com/devicelogin","expires_in":900,"interval":5}`))
			return true
		case r.URL.Path == "/token" && r.FormValue("device_code") != "":
			f.calls.Add(1)
			claims, _ := json.Marshal(map[string]any{"sub": "same-subject", "email": "shared@mail.test", "name": "Device Person", "aud": "client", "exp": time.Now().Add(time.Hour).Unix()})
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "device-access", "refresh_token": "device-refresh", "token_type": "Bearer", "expires_in": 3600, "scope": "https://graph.microsoft.com/Mail.ReadWrite https://graph.microsoft.com/Mail.Send https://graph.microsoft.com/MailboxSettings.ReadWrite", "id_token": "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"})
			return true
		}
		return false
	}, func(cfg *mailauth.Config) {
		cfg.MicrosoftDeviceCode = true
		cfg.MicrosoftClient.Endpoint.DeviceAuthURL = strings.TrimSuffix(cfg.MicrosoftClient.Endpoint.TokenURL, "/token") + "/devicecode"
	})
	return f
}

func (f *userOAuthFixture) devicePage(owner, state string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.h.auth.Middleware(http.HandlerFunc(f.h.handleUserMicrosoftDeviceSignInPage)).ServeHTTP(rec, f.request(owner, "GET", microsoftDeviceSignInLocation(state), ""))
	return rec
}

func (f *userOAuthFixture) devicePoll(t *testing.T, owner, state string) map[string]string {
	t.Helper()
	form := url.Values{"state": {state}, auth.CSRFFormFieldName: {csrfProofForSession(t, f.h.auth, f.sessions[owner].Token, microsoftDeviceSignInPath)}}
	rec := httptest.NewRecorder()
	f.h.auth.Middleware(http.HandlerFunc(f.h.handleUserMicrosoftDeviceSignInPoll)).ServeHTTP(rec, f.request(owner, "POST", microsoftDeviceSignInPath, form.Encode()))
	if rec.Code != http.StatusOK {
		t.Fatalf("poll: %d %s", rec.Code, rec.Body.String())
	}
	var result map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("poll response %q: %v", rec.Body.String(), err)
	}
	return result
}

func TestUserMicrosoftDeviceSignInConnectsMailbox(t *testing.T) {
	f := newMicrosoftDeviceFixture(t)
	state := f.start(t, "alice", "outlook", "add")

	page := f.devicePage("alice", state)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "WXYZ-9876") || !strings.Contains(page.Body.String(), "https://microsoft.com/devicelogin") {
		t.Fatalf("device page: %d %s", page.Code, page.Body.String())
	}
	if strings.Contains(page.Body.String(), "device-secret") {
		t.Fatal("device page exposed the device code")
	}
	if rec := f.devicePage("bob", state); !strings.Contains(rec.Header().Get("Location"), "oauth_session_mismatch") {
		t.Fatalf("other user's device page: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if result := f.devicePoll(t, "bob", state); result["status"] != "done" || !strings.Contains(result["redirect"], "oauth_session_mismatch") {
		t.Fatalf("other user's poll: %v", result)
	}
	if f.calls.Load() != 0 {
		t.Fatal("other user's poll reached Microsoft")
	}

	result := f.devicePoll(t, "alice", state)
	if result["status"] != "done" || result["redirect"] != "/settings/accounts?account_added=1" {
		t.Fatalf("poll: %v", result)
	}
	accounts := f.accounts(t, "alice")
	if len(accounts) != 1 || accounts[0].Email != "shared@mail.test" || accounts[0].Provider != "outlook" {
		t.Fatalf("accounts: %+v", accounts)
	}
	token, err := f.h.userCredentials.GetOAuthTokenForUser(t.Context(), "alice", accounts[0].ID)
	if err != nil || token != "device-access" {
		t.Fatalf("stored token: %q %v", token, err)
	}
	if f.starts.Load() != 1 {
		t.Fatalf("mailbox worker starts = %d", f.starts.Load())
	}
	if replay := f.devicePoll(t, "alice", state); !strings.Contains(replay["redirect"], "oauth_invalid_state") || f.calls.Load() != 1 {
		t.Fatalf("replayed poll: %v after %d grants", replay, f.calls.Load())
	}
}

func TestUserMicrosoftDeviceSignInReconnectKeepsAccount(t *testing.T) {
	f := newMicrosoftDeviceFixture(t)
	if result := f.devicePoll(t, "alice", f.start(t, "alice", "outlook", "add")); result["status"] != "done" {
		t.Fatalf("add: %v", result)
	}
	id := f.accounts(t, "alice")[0].ID
	result := f.devicePoll(t, "alice", f.start(t, "alice", "outlook", "reconnect"))
	if result["redirect"] != "/settings/accounts?account_reconnected=1" {
		t.Fatalf("reconnect: %v", result)
	}
	if accounts := f.accounts(t, "alice"); len(accounts) != 1 || accounts[0].ID != id {
		t.Fatalf("reconnect replaced account: %+v", accounts)
	}
}

func TestUserMicrosoftAuthorizeRedirectsToDevicePage(t *testing.T) {
	f := newMicrosoftDeviceFixture(t)
	path := "/api/accounts/oauth2/authorize"
	form := url.Values{"provider": {"outlook"}, "flow_action": {"add"}, auth.CSRFFormFieldName: {csrfProofForSession(t, f.h.auth, f.sessions["alice"].Token, path)}}
	rec := httptest.NewRecorder()
	f.h.auth.Middleware(http.HandlerFunc(f.h.handleUserAccountOAuthAuthorize)).ServeHTTP(rec, f.request("alice", "POST", path, form.Encode()))
	location, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusSeeOther || location == nil || location.Path != microsoftDeviceSignInPath || location.Query().Get("state") == "" {
		t.Fatalf("authorize: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}
