package mailauth

import (
	"slices"
	"strings"
	"testing"
)

func TestLoadConfigUsesMailboxCredentialsAndScopes(t *testing.T) {
	t.Setenv("GOFER_GOOGLE_LOGIN_CLIENT_ID", "login-client")
	t.Setenv("GOFER_GOOGLE_LOGIN_CLIENT_SECRET", "login-secret")
	t.Setenv("GOOGLE_OAUTH_CLIENT_ID", "google-mailbox-client")
	t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "google-mailbox-secret")
	t.Setenv("MICROSOFT_OAUTH_CLIENT_ID", "microsoft-mailbox-client")
	t.Setenv("MICROSOFT_OAUTH_CLIENT_SECRET", "microsoft-mailbox-secret")
	t.Setenv("MICROSOFT_OAUTH_TENANT", "organizations")

	cfg := LoadConfig("https://gofer.example/", true)
	if !cfg.Enabled || cfg.GoogleClient == nil || cfg.MicrosoftClient == nil {
		t.Fatalf("mailbox OAuth config = %#v", cfg)
	}
	if cfg.GoogleClient.ClientID != "google-mailbox-client" || cfg.GoogleClient.RedirectURL != "https://gofer.example/auth/google/account/callback" {
		t.Fatalf("Google mailbox client = %#v", cfg.GoogleClient)
	}
	if !slices.Equal(cfg.GoogleClient.Scopes, googleAccountScopes()) {
		t.Fatalf("Google mailbox scopes = %#v", cfg.GoogleClient.Scopes)
	}
	if cfg.MicrosoftClient.ClientID != "microsoft-mailbox-client" || cfg.MicrosoftClient.RedirectURL != "https://gofer.example/auth/microsoft/account/callback" {
		t.Fatalf("Microsoft mailbox client = %#v", cfg.MicrosoftClient)
	}
	if !slices.Equal(cfg.MicrosoftClient.Scopes, microsoftAccountTokenScopes()) {
		t.Fatalf("Microsoft mailbox scopes = %#v", cfg.MicrosoftClient.Scopes)
	}
	if !strings.Contains(cfg.MicrosoftClient.Endpoint.AuthURL, "/organizations/") {
		t.Fatalf("Microsoft tenant endpoint = %q", cfg.MicrosoftClient.Endpoint.AuthURL)
	}
}

func TestLoadConfigDoesNotReuseApplicationLoginCredentialsForMailboxAccess(t *testing.T) {
	t.Setenv("GOFER_GOOGLE_LOGIN_CLIENT_ID", "login-client")
	t.Setenv("GOFER_GOOGLE_LOGIN_CLIENT_SECRET", "login-secret")
	t.Setenv("GOOGLE_OAUTH_CLIENT_ID", "")
	t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "")
	t.Setenv("MICROSOFT_OAUTH_CLIENT_ID", "")
	t.Setenv("MICROSOFT_OAUTH_CLIENT_SECRET", "")

	cfg := LoadConfig("https://gofer.example", true)
	if cfg.GoogleClient != nil || cfg.MicrosoftClient != nil {
		t.Fatalf("mailbox OAuth reused application-login credentials: %#v", cfg)
	}
}
