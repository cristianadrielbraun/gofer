package auth

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
	"golang.org/x/oauth2"
)

const (
	microsoftGraphContactsScope        = "https://graph.microsoft.com/Contacts.ReadWrite"
	microsoftGraphMailScope            = "https://graph.microsoft.com/Mail.ReadWrite"
	microsoftGraphMailSendScope        = "https://graph.microsoft.com/Mail.Send"
	microsoftGraphMailboxSettingsScope = "https://graph.microsoft.com/MailboxSettings.ReadWrite"
)

func (m *Manager) GetOAuthTokenForAccount(ctx context.Context, accountID string) (string, error) {
	var accountProvider, providerAccountID string
	if err := m.db.Read().QueryRowContext(ctx, `SELECT provider, provider_account_id FROM accounts WHERE id = ?`, accountID).Scan(&accountProvider, &providerAccountID); err != nil {
		return "", fmt.Errorf("query account oauth identity: %w", err)
	}
	oauthProvider, err := oauthProviderForAccountProvider(accountProvider)
	if err != nil {
		return "", err
	}
	if accountProvider == providers.ProviderOutlook {
		return m.GetMicrosoftGraphMailTokenForAccount(ctx, accountID)
	}
	if providerAccountID != "" {
		return m.getOAuthTokenForAccount(ctx, accountID, oauthProvider, true)
	}
	return m.getOAuthTokenForAccount(ctx, accountID, oauthProvider, false)
}

func (m *Manager) RefreshOAuthTokenForAccount(ctx context.Context, accountID string) (string, error) {
	var accountProvider, providerAccountID string
	if err := m.db.Read().QueryRowContext(ctx, `SELECT provider, provider_account_id FROM accounts WHERE id = ?`, accountID).Scan(&accountProvider, &providerAccountID); err != nil {
		return "", fmt.Errorf("query account oauth identity: %w", err)
	}
	oauthProvider, err := oauthProviderForAccountProvider(accountProvider)
	if err != nil {
		return "", err
	}
	record, err := m.oauthTokenForAccount(ctx, accountID, oauthProvider, providerAccountID != "")
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

func (m *Manager) GetMicrosoftGraphMailTokenForAccount(ctx context.Context, accountID string) (string, error) {
	return m.getMicrosoftGraphTokenForAccount(ctx, accountID, "mail", microsoftGraphMailScopes()...)
}

func (m *Manager) getMicrosoftGraphTokenForAccount(ctx context.Context, accountID, label string, scopes ...string) (string, error) {
	var accountProvider, providerAccountID string
	if err := m.db.Read().QueryRowContext(ctx, `SELECT provider, provider_account_id FROM accounts WHERE id = ?`, accountID).Scan(&accountProvider, &providerAccountID); err != nil {
		return "", fmt.Errorf("query account oauth identity: %w", err)
	}
	if accountProvider != providers.ProviderOutlook {
		return "", fmt.Errorf("account %s is not an Outlook account", accountID)
	}
	record, err := m.oauthTokenForAccount(ctx, accountID, providers.OAuthMicrosoft, providerAccountID != "")
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

func (m *Manager) getOAuthTokenForAccount(ctx context.Context, accountID, oauthProvider string, useProviderIdentity bool) (string, error) {
	record, err := m.oauthTokenForAccount(ctx, accountID, oauthProvider, useProviderIdentity)
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
	ID           string
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresAt    sql.NullTime
	Scopes       string
}

func (m *Manager) oauthTokenForAccount(ctx context.Context, accountID, oauthProvider string, useProviderIdentity bool) (oauthTokenRecord, error) {
	var record oauthTokenRecord
	var err error
	if useProviderIdentity {
		err = m.db.Read().QueryRowContext(ctx,
			`SELECT oa.id, oa.access_token, oa.refresh_token, oa.token_type, oa.expires_at, oa.scopes
			 FROM accounts a
			 JOIN oauth_accounts oa ON oa.user_id = a.user_id
			  AND oa.provider = ?
			  AND oa.provider_account_id = a.provider_account_id
			 WHERE a.id = ? AND a.provider_account_id != ''`,
			oauthProvider, accountID,
		).Scan(&record.ID, &record.AccessToken, &record.RefreshToken, &record.TokenType, &record.ExpiresAt, &record.Scopes)
	} else {
		err = m.db.Read().QueryRowContext(ctx,
			`SELECT id, access_token, refresh_token, token_type, expires_at, scopes
			 FROM oauth_accounts WHERE user_id = (SELECT user_id FROM accounts WHERE id = ?) AND provider = ?`,
			accountID, oauthProvider,
		).Scan(&record.ID, &record.AccessToken, &record.RefreshToken, &record.TokenType, &record.ExpiresAt, &record.Scopes)
	}
	if err == sql.ErrNoRows {
		return record, fmt.Errorf("no oauth token found for account %s", accountID)
	}
	if err != nil {
		return record, fmt.Errorf("query oauth token: %w", err)
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
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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
	_, err := m.db.Write().ExecContext(ctx,
		`UPDATE oauth_accounts SET refresh_token = ?, updated_at = ? WHERE id = ?`,
		refreshToken, time.Now(), oauthAccountID,
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
	_, err := m.db.Write().ExecContext(ctx,
		`UPDATE oauth_accounts
		    SET access_token = ?,
		        refresh_token = COALESCE(NULLIF(?, ''), refresh_token),
		        token_type = ?,
		        expires_at = ?,
		        scopes = ?,
		        updated_at = ?
		  WHERE id = ?`,
		token.AccessToken, token.RefreshToken, tokenType, expiresAt, scopes, time.Now(), oauthAccountID,
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
