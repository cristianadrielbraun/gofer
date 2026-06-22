package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

func TestMicrosoftGraphContactsTokenUsesGraphScopeAndDoesNotReplaceMailToken(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().ExecContext(ctx, `INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address)
		VALUES ('acc', 'default', 'outlook', 'subject-id', 'person@outlook.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	var gotScope string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		gotScope = r.FormValue("scope")
		if got := r.FormValue("grant_type"); got != "refresh_token" {
			t.Fatalf("grant_type = %q, want refresh_token", got)
		}
		if got := r.FormValue("refresh_token"); got != "refresh-token" {
			t.Fatalf("refresh_token = %q, want refresh-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"graph.header.payload","refresh_token":"rotated-refresh-token","token_type":"Bearer","expires_in":3600}`))
	}))
	defer server.Close()

	manager := NewManager(&Config{
		MicrosoftClient: &oauth2.Config{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			Endpoint:     oauth2.Endpoint{TokenURL: server.URL},
		},
	}, db)
	expiresAt := time.Now().Add(time.Hour)
	if err := manager.UpsertOAuthAccount(ctx, "default", providers.OAuthMicrosoft, "subject-id", "outlook-opaque-token", "refresh-token", "Bearer", &expiresAt, "https://outlook.office.com/IMAP.AccessAsUser.All"); err != nil {
		t.Fatalf("UpsertOAuthAccount() error = %v", err)
	}

	token, err := manager.GetMicrosoftGraphContactsTokenForAccount(ctx, "acc")
	if err != nil {
		t.Fatalf("GetMicrosoftGraphContactsTokenForAccount() error = %v", err)
	}
	if token != "graph.header.payload" {
		t.Fatalf("token = %q, want graph token", token)
	}
	if gotScope != microsoftGraphContactsScope {
		t.Fatalf("scope = %q, want %q", gotScope, microsoftGraphContactsScope)
	}

	var storedAccessToken, storedRefreshToken string
	if err := db.Read().QueryRowContext(ctx, `SELECT access_token, refresh_token FROM oauth_accounts WHERE provider = ? AND provider_account_id = ?`, providers.OAuthMicrosoft, "subject-id").Scan(&storedAccessToken, &storedRefreshToken); err != nil {
		t.Fatalf("query stored token: %v", err)
	}
	if storedAccessToken != "outlook-opaque-token" {
		t.Fatalf("stored access token = %q, want Outlook mail token preserved", storedAccessToken)
	}
	if storedRefreshToken != "rotated-refresh-token" {
		t.Fatalf("stored refresh token = %q, want rotated refresh token", storedRefreshToken)
	}
}

func TestMicrosoftGraphMailTokenUsesGraphMailAndMailboxSettingsScopesAndDoesNotReplaceMailToken(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().ExecContext(ctx, `INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address)
		VALUES ('acc', 'default', 'outlook', 'subject-id', 'person@outlook.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	var gotScope string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		gotScope = r.FormValue("scope")
		if got := r.FormValue("grant_type"); got != "refresh_token" {
			t.Fatalf("grant_type = %q, want refresh_token", got)
		}
		if got := r.FormValue("refresh_token"); got != "refresh-token" {
			t.Fatalf("refresh_token = %q, want refresh-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"graph-mail-token","refresh_token":"rotated-refresh-token","token_type":"Bearer","expires_in":3600}`))
	}))
	defer server.Close()

	manager := NewManager(&Config{
		MicrosoftClient: &oauth2.Config{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			Endpoint:     oauth2.Endpoint{TokenURL: server.URL},
		},
	}, db)
	expiresAt := time.Now().Add(time.Hour)
	if err := manager.UpsertOAuthAccount(ctx, "default", providers.OAuthMicrosoft, "subject-id", "outlook-opaque-token", "refresh-token", "Bearer", &expiresAt, strings.Join(microsoftOutlookMailScopes(), " ")); err != nil {
		t.Fatalf("UpsertOAuthAccount() error = %v", err)
	}

	token, err := manager.GetMicrosoftGraphMailTokenForAccount(ctx, "acc")
	if err != nil {
		t.Fatalf("GetMicrosoftGraphMailTokenForAccount() error = %v", err)
	}
	if token != "graph-mail-token" {
		t.Fatalf("token = %q, want graph mail token", token)
	}
	if gotScope != strings.Join(microsoftGraphMailScopes(), " ") {
		t.Fatalf("scope = %q, want Graph mail scopes", gotScope)
	}
	if !strings.Contains(gotScope, microsoftGraphMailScope) {
		t.Fatalf("scope = %q, want Graph mail scope", gotScope)
	}
	if !strings.Contains(gotScope, microsoftGraphMailboxSettingsScope) {
		t.Fatalf("scope = %q, want Graph mailbox settings scope", gotScope)
	}

	var storedAccessToken, storedRefreshToken string
	if err := db.Read().QueryRowContext(ctx, `SELECT access_token, refresh_token FROM oauth_accounts WHERE provider = ? AND provider_account_id = ?`, providers.OAuthMicrosoft, "subject-id").Scan(&storedAccessToken, &storedRefreshToken); err != nil {
		t.Fatalf("query stored token: %v", err)
	}
	if storedAccessToken != "outlook-opaque-token" {
		t.Fatalf("stored access token = %q, want Outlook mail token preserved", storedAccessToken)
	}
	if storedRefreshToken != "rotated-refresh-token" {
		t.Fatalf("stored refresh token = %q, want rotated refresh token", storedRefreshToken)
	}
}

func TestGetOAuthTokenForOutlookUsesOutlookScopesAndStoresMailToken(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().ExecContext(ctx, `INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address)
		VALUES ('acc', 'default', 'outlook', 'subject-id', 'person@outlook.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	var gotScope string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		gotScope = r.FormValue("scope")
		if got := r.FormValue("refresh_token"); got != "refresh-token" {
			t.Fatalf("refresh_token = %q, want refresh-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"outlook-mail-token","refresh_token":"rotated-mail-refresh-token","token_type":"Bearer","expires_in":3600,"scope":"https://outlook.office.com/IMAP.AccessAsUser.All https://outlook.office.com/SMTP.Send"}`))
	}))
	defer server.Close()

	manager := NewManager(&Config{
		MicrosoftClient: &oauth2.Config{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			Endpoint:     oauth2.Endpoint{TokenURL: server.URL},
		},
	}, db)
	expiresAt := time.Now().Add(time.Hour)
	if err := manager.UpsertOAuthAccount(ctx, "default", providers.OAuthMicrosoft, "subject-id", "graph-token", "refresh-token", "Bearer", &expiresAt, microsoftGraphContactsScope); err != nil {
		t.Fatalf("UpsertOAuthAccount() error = %v", err)
	}

	token, err := manager.GetOAuthTokenForAccount(ctx, "acc")
	if err != nil {
		t.Fatalf("GetOAuthTokenForAccount() error = %v", err)
	}
	if token != "outlook-mail-token" {
		t.Fatalf("token = %q, want Outlook mail token", token)
	}
	if gotScope != strings.Join(microsoftOutlookMailScopes(), " ") {
		t.Fatalf("scope = %q, want Outlook mail scopes", gotScope)
	}

	var storedAccessToken, storedRefreshToken, storedScopes string
	if err := db.Read().QueryRowContext(ctx, `SELECT access_token, refresh_token, scopes FROM oauth_accounts WHERE provider = ? AND provider_account_id = ?`, providers.OAuthMicrosoft, "subject-id").Scan(&storedAccessToken, &storedRefreshToken, &storedScopes); err != nil {
		t.Fatalf("query stored token: %v", err)
	}
	if storedAccessToken != "outlook-mail-token" {
		t.Fatalf("stored access token = %q, want Outlook mail token", storedAccessToken)
	}
	if storedRefreshToken != "rotated-mail-refresh-token" {
		t.Fatalf("stored refresh token = %q, want rotated refresh token", storedRefreshToken)
	}
	if storedScopes != strings.Join(microsoftOutlookMailScopes(), " ") {
		t.Fatalf("stored scopes = %q, want Outlook mail scopes", storedScopes)
	}
}

func TestMicrosoftGraphContactsTokenRejectsNonOutlookAccount(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().ExecContext(ctx, `INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address)
		VALUES ('gmail_acc', 'default', 'gmail', 'subject-id', 'person@gmail.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	manager := NewManager(&Config{}, db)
	_, err = manager.GetMicrosoftGraphContactsTokenForAccount(ctx, "gmail_acc")
	if err == nil || !strings.Contains(err.Error(), "not an Outlook account") {
		t.Fatalf("error = %v, want non-Outlook account rejection", err)
	}
}
