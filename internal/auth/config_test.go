package auth

import (
	"slices"
	"testing"
)

func TestLoadConfigSeparatesGoogleLoginFromMailboxOAuth(t *testing.T) {
	t.Setenv("GOFER_AUTH_ENABLED", "true")
	t.Setenv("GOOGLE_OAUTH_CLIENT_ID", "mailbox-client")
	t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "mailbox-secret")
	t.Setenv("GOFER_GOOGLE_LOGIN_CLIENT_ID", "login-client")
	t.Setenv("GOFER_GOOGLE_LOGIN_CLIENT_SECRET", "login-secret")

	cfg := LoadConfig("https://gofer.example")
	if !cfg.Enabled || cfg.GoogleLoginClient == nil {
		t.Fatalf("authentication config = %#v", cfg)
	}
	if cfg.GoogleLoginClient.ClientID != "login-client" || cfg.GoogleLoginClient.ClientSecret != "login-secret" {
		t.Fatalf("Google login client = %#v", cfg.GoogleLoginClient)
	}
	if cfg.GoogleLoginClient.RedirectURL != "https://gofer.example/auth/google/callback" {
		t.Fatalf("Google login redirect = %q", cfg.GoogleLoginClient.RedirectURL)
	}
	wantScopes := []string{"openid", "email", "profile"}
	if !slices.Equal(cfg.GoogleLoginClient.Scopes, wantScopes) {
		t.Fatalf("Google login scopes = %#v, want %#v", cfg.GoogleLoginClient.Scopes, wantScopes)
	}
	for _, scope := range cfg.GoogleLoginClient.Scopes {
		if scope == "https://mail.google.com/" || scope == "https://www.googleapis.com/auth/contacts" {
			t.Fatalf("Google application login requested mailbox scope %q", scope)
		}
	}
}

func TestLoadConfigKeepsLocalAuthenticationEnabledWithoutGoogleLogin(t *testing.T) {
	t.Setenv("GOFER_AUTH_ENABLED", "true")
	t.Setenv("GOFER_GOOGLE_LOGIN_CLIENT_ID", "")
	t.Setenv("GOFER_GOOGLE_LOGIN_CLIENT_SECRET", "")

	cfg := LoadConfig("https://gofer.example")
	if !cfg.Enabled {
		t.Fatal("local authentication was disabled because Google login was not configured")
	}
	if cfg.GoogleLoginClient != nil {
		t.Fatalf("Google login client = %#v, want nil", cfg.GoogleLoginClient)
	}
}
