package mailauth

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/retry"
	"golang.org/x/oauth2"
)

const (
	GoogleCalendarReadOnlyScope        = "https://www.googleapis.com/auth/calendar.readonly"
	microsoftGraphContactsScope        = "https://graph.microsoft.com/Contacts.ReadWrite"
	microsoftGraphCalendarScope        = "https://graph.microsoft.com/Calendars.Read"
	microsoftGraphMailScope            = "https://graph.microsoft.com/Mail.ReadWrite"
	microsoftGraphMailSendScope        = "https://graph.microsoft.com/Mail.Send"
	microsoftGraphMailboxSettingsScope = "https://graph.microsoft.com/MailboxSettings.ReadWrite"
)

// OAuthTokenError keeps the non-secret parts of a token endpoint failure so
// callers can distinguish a permanent authorization problem from a temporary
// provider outage and can honor Retry-After.
type OAuthTokenError struct {
	Status      int
	Code        string
	Description string
	RetryAt     time.Time
}

func (e *OAuthTokenError) Error() string {
	if e == nil {
		return "oauth token endpoint failed"
	}
	if strings.TrimSpace(e.Code) != "" {
		return fmt.Sprintf("oauth token endpoint returned %d (%s)", e.Status, strings.TrimSpace(e.Code))
	}
	return fmt.Sprintf("oauth token endpoint returned %d", e.Status)
}

func (e *OAuthTokenError) RetryAfter() (time.Time, bool) {
	if e == nil || e.RetryAt.IsZero() {
		return time.Time{}, false
	}
	return e.RetryAt, true
}

func (e *OAuthTokenError) OAuthErrorCode() string {
	if e == nil {
		return ""
	}
	return e.Code
}

func (e *OAuthTokenError) OAuthErrorDescription() string {
	if e == nil {
		return ""
	}
	return e.Description
}

func (e *OAuthTokenError) OAuthErrorStatus() int {
	if e == nil {
		return 0
	}
	return e.Status
}

func (m *Manager) GetOAuthTokenForAccount(ctx context.Context, accountID string) (string, error) {
	var accountProvider string
	if err := m.db.Read().QueryRowContext(ctx, `SELECT provider FROM accounts WHERE id = ?`, accountID).Scan(&accountProvider); err != nil {
		return "", fmt.Errorf("query account oauth identity: %w", err)
	}
	oauthProvider, err := oauthProviderForAccountProvider(accountProvider)
	if err != nil {
		return "", err
	}
	if accountProvider == providers.ProviderOutlook {
		return m.GetMicrosoftGraphMailTokenForAccount(ctx, accountID)
	}
	return m.getOAuthTokenForAccount(ctx, accountID, oauthProvider)
}

// GetGoogleCalendarTokenForAccount returns a Google mailbox token only when
// the account was authorized for the read-only Calendar API scope. Existing
// mailbox grants must be reconnected once before Calendar discovery can use
// them.
func (m *Manager) GetGoogleCalendarTokenForAccount(ctx context.Context, accountID string) (string, error) {
	record, err := m.oauthTokenForAccount(ctx, accountID, providers.OAuthGoogle)
	if err != nil {
		return "", err
	}
	if !recordHasScopes(record.Scopes, GoogleCalendarReadOnlyScope) {
		return "", fmt.Errorf("Google Calendar access is not authorized for account %s; reconnect Google to grant Calendar access", accountID)
	}
	if record.AccessToken != "" && record.ExpiresAt.Valid && record.ExpiresAt.Time.After(time.Now().Add(5*time.Minute)) {
		return record.AccessToken, nil
	}
	if record.RefreshToken == "" {
		return "", fmt.Errorf("no refresh token available for account %s", accountID)
	}
	return m.refreshToken(ctx, providers.OAuthGoogle, record.ID, record.RefreshToken)
}

func (m *Manager) RefreshOAuthTokenForAccount(ctx context.Context, accountID string) (string, error) {
	var accountProvider string
	if err := m.db.Read().QueryRowContext(ctx, `SELECT provider FROM accounts WHERE id = ?`, accountID).Scan(&accountProvider); err != nil {
		return "", fmt.Errorf("query account oauth identity: %w", err)
	}
	oauthProvider, err := oauthProviderForAccountProvider(accountProvider)
	if err != nil {
		return "", err
	}
	record, err := m.oauthTokenForAccount(ctx, accountID, oauthProvider)
	if err != nil {
		return "", err
	}
	if record.RefreshToken == "" {
		return "", fmt.Errorf("no refresh token available for account %s", accountID)
	}
	return m.refreshToken(ctx, oauthProvider, record.ID, record.RefreshToken)
}

func (m *Manager) GetMicrosoftGraphContactsTokenForAccount(ctx context.Context, accountID string) (string, error) {
	return m.getMicrosoftGraphTokenForAccount(ctx, accountID, "contacts", microsoftGraphContactsScope)
}

func (m *Manager) GetMicrosoftGraphCalendarTokenForAccount(ctx context.Context, accountID string) (string, error) {
	return m.getMicrosoftGraphTokenForAccount(ctx, accountID, "calendar", microsoftGraphCalendarScope)
}

func (m *Manager) GetMicrosoftGraphMailTokenForAccount(ctx context.Context, accountID string) (string, error) {
	return m.getMicrosoftGraphTokenForAccount(ctx, accountID, "mail", microsoftGraphMailScopes()...)
}

func (m *Manager) getMicrosoftGraphTokenForAccount(ctx context.Context, accountID, label string, scopes ...string) (string, error) {
	var accountProvider string
	if err := m.db.Read().QueryRowContext(ctx, `SELECT provider FROM accounts WHERE id = ?`, accountID).Scan(&accountProvider); err != nil {
		return "", fmt.Errorf("query account oauth identity: %w", err)
	}
	if accountProvider != providers.ProviderOutlook {
		return "", fmt.Errorf("account %s is not an Outlook account", accountID)
	}
	record, err := m.oauthTokenForAccount(ctx, accountID, providers.OAuthMicrosoft)
	if err != nil {
		return "", err
	}
	if record.AccessToken != "" && recordHasScopes(record.Scopes, scopes...) && record.ExpiresAt.Valid && record.ExpiresAt.Time.After(time.Now().Add(5*time.Minute)) {
		return record.AccessToken, nil
	}
	if strings.TrimSpace(record.RefreshToken) == "" {
		return "", fmt.Errorf("no refresh token available for account %s", accountID)
	}
	cfg, err := m.oauthConfigForProvider(providers.OAuthMicrosoft)
	if err != nil {
		return "", err
	}
	token, err := refreshTokenForScopes(ctx, cfg, record.RefreshToken, scopes)
	if err != nil {
		return "", fmt.Errorf("refresh graph %s token: %w", label, err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return "", fmt.Errorf("empty graph %s access token", label)
	}
	if strings.TrimSpace(token.RefreshToken) != "" && token.RefreshToken != record.RefreshToken {
		if err := m.storeOAuthRefreshToken(ctx, record.ID, token.RefreshToken); err != nil {
			return "", fmt.Errorf("store graph %s refresh token: %w", label, err)
		}
	}
	return token.AccessToken, nil
}

func microsoftGraphMailScopes() []string {
	return []string{microsoftGraphMailScope, microsoftGraphMailSendScope, microsoftGraphMailboxSettingsScope}
}

func recordHasScopes(recordScopes string, expected ...string) bool {
	if len(expected) == 0 {
		return true
	}
	seen := map[string]bool{}
	for _, scope := range strings.Fields(recordScopes) {
		seen[strings.ToLower(scope)] = true
	}
	for _, scope := range expected {
		if !seen[strings.ToLower(scope)] {
			return false
		}
	}
	return true
}

func oauthProviderForAccountProvider(provider string) (string, error) {
	switch provider {
	case providers.ProviderGmail:
		return providers.OAuthGoogle, nil
	case providers.ProviderOutlook:
		return providers.OAuthMicrosoft, nil
	default:
		return "", fmt.Errorf("unsupported oauth account provider %q", provider)
	}
}

func (m *Manager) getOAuthTokenForAccount(ctx context.Context, accountID, oauthProvider string) (string, error) {
	record, err := m.oauthTokenForAccount(ctx, accountID, oauthProvider)
	if err != nil {
		return "", err
	}

	if record.AccessToken != "" && record.ExpiresAt.Valid && record.ExpiresAt.Time.After(time.Now().Add(5*time.Minute)) {
		return record.AccessToken, nil
	}

	if record.RefreshToken == "" {
		return "", fmt.Errorf("no refresh token available for account %s", accountID)
	}

	return m.refreshToken(ctx, oauthProvider, record.ID, record.RefreshToken)
}

type oauthTokenRecord struct {
	oauthCredentialContext
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresAt    sql.NullTime
	Scopes       string
}

func (m *Manager) oauthTokenForAccount(ctx context.Context, accountID, oauthProvider string) (oauthTokenRecord, error) {
	var record oauthTokenRecord
	var accessCiphertext, refreshCiphertext []byte
	var keyVersion sql.NullInt64
	err := m.db.Read().QueryRowContext(ctx, `
		SELECT oa.id, oa.account_id, oa.provider, oa.provider_account_id,
		       oa.access_token_ciphertext, oa.refresh_token_ciphertext, oa.key_version,
		       oa.token_type, oa.expires_at, oa.scopes
		FROM accounts account
		JOIN oauth_accounts oa ON oa.account_id = account.id
		WHERE account.id = ? AND oa.provider = ?`,
		accountID, oauthProvider,
	).Scan(
		&record.ID, &record.AccountID, &record.Provider, &record.ProviderAccountID,
		&accessCiphertext, &refreshCiphertext, &keyVersion,
		&record.TokenType, &record.ExpiresAt, &record.Scopes,
	)
	if err == sql.ErrNoRows {
		return record, fmt.Errorf("no oauth token found for account %s", accountID)
	}
	if err != nil {
		return record, fmt.Errorf("query oauth token: %w", err)
	}
	if !keyVersion.Valid {
		return record, fmt.Errorf("mailbox credential for account %s has not been encrypted", accountID)
	}
	record.AccessToken, err = m.decryptOAuthToken(record.oauthCredentialContext, "access", accessCiphertext, int(keyVersion.Int64))
	if err != nil {
		return oauthTokenRecord{}, fmt.Errorf("decrypt mailbox access token: %w", err)
	}
	record.RefreshToken, err = m.decryptOAuthToken(record.oauthCredentialContext, "refresh", refreshCiphertext, int(keyVersion.Int64))
	if err != nil {
		return oauthTokenRecord{}, fmt.Errorf("decrypt mailbox refresh token: %w", err)
	}
	return record, nil
}

func (m *Manager) oauthConfigForProvider(provider string) (*oauth2.Config, error) {
	switch provider {
	case providers.OAuthGoogle:
		if m.config.GoogleClient == nil {
			return nil, fmt.Errorf("google oauth not configured")
		}
		return m.config.GoogleClient, nil
	case providers.OAuthMicrosoft:
		if m.config.MicrosoftClient == nil {
			return nil, fmt.Errorf("microsoft oauth not configured")
		}
		return m.config.MicrosoftClient, nil
	default:
		return nil, fmt.Errorf("unsupported oauth provider %q", provider)
	}
}

func refreshTokenForScopes(ctx context.Context, cfg *oauth2.Config, refreshToken string, scopes []string) (*oauth2.Token, error) {
	if cfg == nil {
		return nil, fmt.Errorf("oauth config is not configured")
	}
	values := url.Values{}
	values.Set("grant_type", "refresh_token")
	values.Set("refresh_token", refreshToken)
	if len(scopes) > 0 {
		values.Set("scope", strings.Join(scopes, " "))
	}
	if cfg.ClientID != "" {
		values.Set("client_id", cfg.ClientID)
	}
	if cfg.ClientSecret != "" {
		values.Set("client_secret", cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint.TokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if readErr != nil {
			return nil, readErr
		}
		var tokenFailure struct {
			Code        string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &tokenFailure)
		retryAt, _ := retry.ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now().UTC())
		return nil, &OAuthTokenError{
			Status:      resp.StatusCode,
			Code:        strings.TrimSpace(tokenFailure.Code),
			Description: strings.TrimSpace(tokenFailure.Description),
			RetryAt:     retryAt,
		}
	}
	if readErr != nil {
		return nil, readErr
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	token := &oauth2.Token{
		AccessToken:  result.AccessToken,
		TokenType:    result.TokenType,
		RefreshToken: result.RefreshToken,
	}
	if token.TokenType == "" {
		token.TokenType = "Bearer"
	}
	if result.ExpiresIn > 0 {
		token.Expiry = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	}
	token = token.WithExtra(map[string]any{"scope": result.Scope})
	return token, nil
}

func (m *Manager) storeOAuthRefreshToken(ctx context.Context, oauthAccountID, refreshToken string) error {
	if oauthAccountID == "" || strings.TrimSpace(refreshToken) == "" {
		return nil
	}
	credential, err := m.loadOAuthCredentialContext(ctx, m.db.Read(), oauthAccountID)
	if err != nil {
		return err
	}
	ciphertext, err := m.encryptOAuthToken(credential, "refresh", refreshToken)
	if err != nil {
		return err
	}
	_, err = m.db.Write().ExecContext(ctx,
		`UPDATE oauth_accounts
		 SET refresh_token = '', refresh_token_ciphertext = ?, key_version = ?, updated_at = ?
		 WHERE id = ? AND account_id = ?`,
		ciphertext, mailboxCredentialKeyVersion, time.Now(), oauthAccountID, credential.AccountID,
	)
	if err != nil {
		return fmt.Errorf("store refreshed token: %w", err)
	}
	return nil
}

func (m *Manager) storeOAuthAccessToken(ctx context.Context, oauthAccountID string, token *oauth2.Token) error {
	if oauthAccountID == "" || token == nil {
		return nil
	}
	var expiresAt *time.Time
	if !token.Expiry.IsZero() {
		t := token.Expiry
		expiresAt = &t
	}
	tokenType := token.TokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	scopes, _ := token.Extra("scope").(string)
	credential, err := m.loadOAuthCredentialContext(ctx, m.db.Read(), oauthAccountID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(scopes) == "" {
		var existingScopes string
		err := m.db.Read().QueryRowContext(ctx,
			`SELECT scopes FROM oauth_accounts WHERE id = ? AND account_id = ?`, oauthAccountID, credential.AccountID,
		).Scan(&existingScopes)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("load mailbox OAuth scopes: %w", err)
		}
		if err == nil {
			scopes = existingScopes
		}
	}
	accessCiphertext, err := m.encryptOAuthToken(credential, "access", token.AccessToken)
	if err != nil {
		return err
	}
	var refreshCiphertext []byte
	if token.RefreshToken != "" {
		refreshCiphertext, err = m.encryptOAuthToken(credential, "refresh", token.RefreshToken)
		if err != nil {
			return err
		}
	}
	_, err = m.db.Write().ExecContext(ctx,
		`UPDATE oauth_accounts
		    SET access_token = '', refresh_token = '',
		        access_token_ciphertext = ?,
		        refresh_token_ciphertext = CASE WHEN ? IS NULL THEN refresh_token_ciphertext ELSE ? END,
		        key_version = ?,
		        token_type = ?,
		        expires_at = ?,
		        scopes = ?,
		        updated_at = ?
		  WHERE id = ? AND account_id = ?`,
		accessCiphertext, refreshCiphertext, refreshCiphertext, mailboxCredentialKeyVersion,
		tokenType, expiresAt, scopes, time.Now(), oauthAccountID, credential.AccountID,
	)
	if err != nil {
		return fmt.Errorf("store refreshed token: %w", err)
	}
	return nil
}

func (m *Manager) refreshToken(ctx context.Context, oauthProvider, oauthAccountID, refreshToken string) (string, error) {
	cfg, err := m.oauthConfigForProvider(oauthProvider)
	if err != nil {
		return "", err
	}

	ts := cfg.TokenSource(ctx, &oauth2.Token{
		RefreshToken: refreshToken,
		TokenType:    "Bearer",
	})

	token, err := ts.Token()
	if err != nil {
		return "", fmt.Errorf("refresh token: %w", err)
	}

	if err := m.storeOAuthAccessToken(ctx, oauthAccountID, token); err != nil {
		return "", err
	}

	return token.AccessToken, nil
}
