package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const accountOAuthFlowLifetime = 10 * time.Minute

var (
	ErrAccountOAuthFlowNotFound         = errors.New("account OAuth flow not found")
	ErrAccountOAuthFlowExpired          = errors.New("account OAuth flow expired")
	ErrAccountOAuthFlowUserMismatch     = errors.New("account OAuth flow user mismatch")
	ErrAccountOAuthFlowSessionMismatch  = errors.New("account OAuth flow session mismatch")
	ErrAccountOAuthFlowProviderMismatch = errors.New("account OAuth flow provider mismatch")
)

type AccountOAuthFlow struct {
	UserID    string
	Provider  string
	FormData  map[string]string
	ExpiresAt time.Time
}

func accountOAuthFlowSecretHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (m *Manager) CreateAccountOAuthFlow(ctx context.Context, userID, sessionToken, provider string, formData map[string]string) (string, error) {
	userID = strings.TrimSpace(userID)
	provider = strings.ToLower(strings.TrimSpace(provider))
	if userID == "" || provider == "" {
		return "", fmt.Errorf("account OAuth flow requires a user and provider")
	}
	if m.config.Enabled && sessionToken == "" {
		return "", fmt.Errorf("account OAuth flow requires an authenticated session")
	}
	encodedForm, err := json.Marshal(formData)
	if err != nil {
		return "", fmt.Errorf("encode account OAuth form: %w", err)
	}
	state := m.GenerateState()
	now := time.Now().UTC()
	expiresAt := now.Add(accountOAuthFlowLifetime)
	tx, err := m.db.Write().BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin account OAuth flow: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_account_flows WHERE expires_at <= ?`, now); err != nil {
		return "", fmt.Errorf("clean expired account OAuth flows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO oauth_account_flows (
			state_hash, user_id, session_token_hash, provider, form_data, expires_at, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		accountOAuthFlowSecretHash(state), userID, accountOAuthFlowSecretHash(sessionToken), provider, string(encodedForm), expiresAt, now); err != nil {
		return "", fmt.Errorf("store account OAuth flow: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit account OAuth flow: %w", err)
	}
	return state, nil
}

func (m *Manager) ConsumeAccountOAuthFlow(ctx context.Context, state, userID, sessionToken, provider string) (*AccountOAuthFlow, error) {
	state = strings.TrimSpace(state)
	userID = strings.TrimSpace(userID)
	provider = strings.ToLower(strings.TrimSpace(provider))
	if state == "" {
		return nil, ErrAccountOAuthFlowNotFound
	}
	if m.config.Enabled && sessionToken == "" {
		return nil, ErrAccountOAuthFlowSessionMismatch
	}
	tx, err := m.db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin account OAuth flow consume: %w", err)
	}
	defer tx.Rollback()

	var flow AccountOAuthFlow
	var storedSessionHash string
	var encodedForm string
	err = tx.QueryRowContext(ctx, `
		SELECT user_id, session_token_hash, provider, form_data, expires_at
		FROM oauth_account_flows
		WHERE state_hash = ?`, accountOAuthFlowSecretHash(state)).Scan(
		&flow.UserID, &storedSessionHash, &flow.Provider, &encodedForm, &flow.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, ErrAccountOAuthFlowNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load account OAuth flow: %w", err)
	}
	if !flow.ExpiresAt.After(time.Now().UTC()) {
		if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM oauth_account_flows WHERE state_hash = ?`, accountOAuthFlowSecretHash(state)); deleteErr != nil {
			return nil, fmt.Errorf("delete expired account OAuth flow: %w", deleteErr)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit expired account OAuth flow: %w", err)
		}
		return nil, ErrAccountOAuthFlowExpired
	}
	if flow.UserID != userID {
		return nil, ErrAccountOAuthFlowUserMismatch
	}
	if flow.Provider != provider {
		return nil, ErrAccountOAuthFlowProviderMismatch
	}
	actualSessionHash := accountOAuthFlowSecretHash(sessionToken)
	if subtle.ConstantTimeCompare([]byte(storedSessionHash), []byte(actualSessionHash)) != 1 {
		return nil, ErrAccountOAuthFlowSessionMismatch
	}
	if err := json.Unmarshal([]byte(encodedForm), &flow.FormData); err != nil {
		return nil, fmt.Errorf("decode account OAuth form: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_account_flows WHERE state_hash = ?`, accountOAuthFlowSecretHash(state)); err != nil {
		return nil, fmt.Errorf("consume account OAuth flow: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit account OAuth flow consume: %w", err)
	}
	return &flow, nil
}

func (m *Manager) CleanupExpiredAccountOAuthFlows(ctx context.Context) error {
	_, err := m.db.Write().ExecContext(ctx, `DELETE FROM oauth_account_flows WHERE expires_at <= ?`, time.Now().UTC())
	return err
}
