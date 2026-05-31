package auth

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"golang.org/x/oauth2"
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
	if providerAccountID != "" {
		return m.getOAuthTokenForAccount(ctx, accountID, oauthProvider, true)
	}
	return m.getOAuthTokenForAccount(ctx, accountID, oauthProvider, false)
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
	var oauthAccountID string
	var accessToken, refreshToken, tokenType string
	var expiresAt sql.NullTime
	var scopes string
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
		).Scan(&oauthAccountID, &accessToken, &refreshToken, &tokenType, &expiresAt, &scopes)
	} else {
		err = m.db.Read().QueryRowContext(ctx,
			`SELECT id, access_token, refresh_token, token_type, expires_at, scopes
			 FROM oauth_accounts WHERE user_id = (SELECT user_id FROM accounts WHERE id = ?) AND provider = ?`,
			accountID, oauthProvider,
		).Scan(&oauthAccountID, &accessToken, &refreshToken, &tokenType, &expiresAt, &scopes)
	}
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no oauth token found for account %s", accountID)
	}
	if err != nil {
		return "", fmt.Errorf("query oauth token: %w", err)
	}

	if accessToken != "" && expiresAt.Valid && expiresAt.Time.After(time.Now().Add(5*time.Minute)) {
		return accessToken, nil
	}

	if refreshToken == "" {
		return "", fmt.Errorf("no refresh token available for account %s", accountID)
	}

	return m.refreshToken(ctx, oauthProvider, oauthAccountID, refreshToken)
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

	var expiresAt *time.Time
	if !token.Expiry.IsZero() {
		t := token.Expiry
		expiresAt = &t
	}
	if oauthAccountID != "" {
		if _, err := m.db.Write().ExecContext(ctx,
			`UPDATE oauth_accounts SET access_token = ?, token_type = ?, expires_at = ?, updated_at = ? WHERE id = ?`,
			token.AccessToken, token.TokenType, expiresAt, time.Now(), oauthAccountID,
		); err != nil {
			return "", fmt.Errorf("store refreshed token: %w", err)
		}
	}

	return token.AccessToken, nil
}
