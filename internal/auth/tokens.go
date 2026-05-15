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
	var providerAccountID string
	_ = m.db.Read().QueryRowContext(ctx, `SELECT provider_account_id FROM accounts WHERE id = ?`, accountID).Scan(&providerAccountID)
	if providerAccountID != "" {
		return m.getOAuthTokenForAccount(ctx, accountID, true)
	}
	return m.getOAuthTokenForAccount(ctx, accountID, false)
}

func (m *Manager) getOAuthTokenForAccount(ctx context.Context, accountID string, useProviderIdentity bool) (string, error) {
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
			providers.OAuthGoogle, accountID,
		).Scan(&oauthAccountID, &accessToken, &refreshToken, &tokenType, &expiresAt, &scopes)
	} else {
		err = m.db.Read().QueryRowContext(ctx,
			`SELECT id, access_token, refresh_token, token_type, expires_at, scopes
			 FROM oauth_accounts WHERE user_id = (SELECT user_id FROM accounts WHERE id = ?) AND provider = ?`,
			accountID, providers.OAuthGoogle,
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

	return m.refreshToken(ctx, oauthAccountID, refreshToken)
}

func (m *Manager) refreshToken(ctx context.Context, oauthAccountID, refreshToken string) (string, error) {
	if m.config.GoogleClient == nil {
		return "", fmt.Errorf("google oauth not configured")
	}

	ts := m.config.GoogleClient.TokenSource(ctx, &oauth2.Token{
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
