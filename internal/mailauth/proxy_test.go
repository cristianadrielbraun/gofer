package mailauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"github.com/cristianadrielbraun/gofer/internal/netguard/netguardtest"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

// The token refresh goes through the account's proxy, and so does every API
// request that carries the token it returns, whatever its context. An account
// whose proxy can't be read gets no token at all.
func TestAccountTokensGoThroughTheAccountProxy(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users (id, username, username_normalized, name) VALUES ('owner', 'owner', 'owner', 'Owner');
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address)
		VALUES ('gmail-account', 'owner', 'gmail', 'google-subject', 'owner@example.test');
		INSERT INTO oauth_accounts (id, account_id, provider, provider_account_id, access_token, refresh_token, scopes)
		VALUES ('google', 'gmail-account', 'google', 'google-subject', '', 'refresh-token', 'https://mail.google.com/');`); err != nil {
		t.Fatal(err)
	}
	var refreshes atomic.Int32
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"proxied-access-token","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokens.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer api.Close()

	proxy := netguardtest.StartHTTPProxy(t)
	manager := New(&Config{GoogleClient: &oauth2.Config{ClientID: "client-id", ClientSecret: "client-secret",
		Endpoint: oauth2.Endpoint{TokenURL: tokens.URL}}}, db, testMailboxCredentialKey)
	if err := manager.SecureOAuthCredentials(ctx); err != nil {
		t.Fatal(err)
	}
	manager.SetAccountProxyResolver(func(_ context.Context, accountID string) (*netguard.Proxy, error) {
		if accountID != "gmail-account" {
			t.Fatalf("resolved the proxy of %q", accountID)
		}
		return proxy.Config, nil
	})

	token, err := manager.GetOAuthTokenForAccount(ctx, "gmail-account")
	if err != nil || token != "proxied-access-token" {
		t.Fatalf("GetOAuthTokenForAccount() = %q, %v", token, err)
	}
	if got := proxy.Targets(); len(got) != 1 || got[0] != strings.TrimPrefix(tokens.URL, "http://") {
		t.Fatalf("token refresh went through %v, want the token endpoint through the proxy", got)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, api.URL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := netguard.APIClient(0).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := proxy.Targets(); len(got) != 2 || got[1] != strings.TrimPrefix(api.URL, "http://") {
		t.Fatalf("API request went through %v, want the API through the proxy", got)
	}

	manager.SetAccountProxyResolver(func(context.Context, string) (*netguard.Proxy, error) {
		return nil, errors.New("proxy password can't be read")
	})
	before := refreshes.Load()
	if _, err := manager.RefreshOAuthTokenForAccount(ctx, "gmail-account"); err == nil {
		t.Fatal("an account whose proxy can't be read got a token")
	}
	if refreshes.Load() != before {
		t.Fatal("the token endpoint was contacted without the account's proxy")
	}
}

// Accounts in their owners' own databases: the refresh goes through the
// account's proxy, and another owner's account stays direct.
func TestUserCredentialTokensGoThroughTheAccountProxy(t *testing.T) {
	var refreshes atomic.Int32
	f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-` + strconv.Itoa(int(refreshes.Load())) + `","token_type":"Bearer","expires_in":3600}`))
	}))
	f.service.SetAccountProxyResolver(f.accounts.AccountProxyForUser)
	proxy := netguardtest.StartHTTPProxy(t)
	central, err := config.NewAccountStore(f.system, testMailboxCredentialKey)
	if err != nil {
		t.Fatal(err)
	}
	id, err := central.SaveOutboundProxy(t.Context(), config.OutboundProxyInput{Name: "Office", Type: "http", Host: proxy.Config.Host, Port: proxy.Config.Port}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.accounts.WithAccountForUser(t.Context(), "alice", f.ids["alice"], func(local *config.AccountStore, _ *storage.DB) error {
		return local.SetAccountProxy(t.Context(), "alice", f.ids["alice"], id)
	}); err != nil {
		t.Fatal(err)
	}
	f.grant(t, "alice", "old-alice", "refresh-alice", true, "https://mail.google.com/")
	f.grant(t, "bob", "old-bob", "refresh-bob", true, "https://mail.google.com/")

	if _, err := f.service.GetOAuthTokenForUser(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	if got := proxy.Targets(); len(got) != 1 {
		t.Fatalf("alice's refresh went through the proxy %d times, want once", len(got))
	}
	if _, err := f.service.GetOAuthTokenForUser(t.Context(), "bob", f.ids["bob"]); err != nil {
		t.Fatal(err)
	}
	if got := proxy.Targets(); len(got) != 1 {
		t.Fatalf("bob's refresh went through alice's proxy: %v", got)
	}
	if refreshes.Load() != 2 {
		t.Fatalf("token endpoint saw %d refreshes, want 2", refreshes.Load())
	}
}

// Signing in an account being added redeems the code through its proxy and
// binds the token to it, so the profile lookup that follows goes there too.
func TestAccountCodeExchangeGoesThroughTheChosenProxy(t *testing.T) {
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-account-token","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokens.Close()
	proxy := netguardtest.StartHTTPProxy(t)
	manager := New(&Config{BaseURL: "https://gofer.example.test", GoogleClient: &oauth2.Config{ClientID: "client", ClientSecret: "secret",
		Endpoint: oauth2.Endpoint{TokenURL: tokens.URL}}}, nil, testMailboxCredentialKey)
	token, err := manager.ExchangeAccountCode(netguard.WithProxy(t.Context(), proxy.Config), "code")
	if err != nil || token.AccessToken != "new-account-token" {
		t.Fatalf("ExchangeAccountCode() = %v, %v", token, err)
	}
	if got := proxy.Targets(); len(got) != 1 || got[0] != strings.TrimPrefix(tokens.URL, "http://") {
		t.Fatalf("code exchange went through %v", got)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "{}") }))
	defer api.Close()
	req, _ := http.NewRequest(http.MethodGet, api.URL, nil)
	req.Header.Set("Authorization", "Bearer new-account-token")
	resp, err := netguard.APIClient(0).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := proxy.Targets(); len(got) != 2 {
		t.Fatalf("a request with the new token went through %v, want the proxy", got)
	}
}
