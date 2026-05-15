package auth

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func (m *Manager) CreateOrUpdateUser(ctx context.Context, email, name, avatarURL string) (*User, error) {
	existing := &User{}
	var isAdmin int
	err := m.db.Read().QueryRowContext(ctx,
		`SELECT id, email, name, avatar_url, is_admin FROM users WHERE email = ?`, email,
	).Scan(&existing.ID, &existing.Email, &existing.Name, &existing.AvatarURL, &isAdmin)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("lookup user: %w", err)
	}

	if existing.ID != "" {
		existing.IsAdmin = isAdmin == 1
		if name != "" {
			_, err = m.db.Write().ExecContext(ctx,
				`UPDATE users SET name = ?, avatar_url = ?, updated_at = ? WHERE id = ?`,
				name, avatarURL, time.Now(), existing.ID,
			)
			if err != nil {
				return nil, fmt.Errorf("update user: %w", err)
			}
			existing.Name = name
			existing.AvatarURL = avatarURL
		}
		return existing, nil
	}

	var userCount int
	m.db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&userCount)

	isAdminVal := 0
	if userCount == 0 {
		isAdminVal = 1
	}

	id := uuid.New().String()
	now := time.Now()
	_, err = m.db.Write().ExecContext(ctx,
		`INSERT INTO users (id, email, name, avatar_url, is_admin, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, email, name, avatarURL, isAdminVal, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}

	return &User{
		ID:        id,
		Email:     email,
		Name:      name,
		AvatarURL: avatarURL,
		IsAdmin:   isAdminVal == 1,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func (m *Manager) UpsertOAuthAccount(ctx context.Context, userID, provider, providerAccountID, accessToken, refreshToken, tokenType string, expiresAt *time.Time, scopes string) error {
	now := time.Now()

	var existingID string
	err := m.db.Read().QueryRowContext(ctx,
		`SELECT id FROM oauth_accounts WHERE provider = ? AND provider_account_id = ?`,
		provider, providerAccountID,
	).Scan(&existingID)

	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("lookup oauth account: %w", err)
	}

	if existingID != "" {
		_, err = m.db.Write().ExecContext(ctx,
			`UPDATE oauth_accounts SET user_id = ?, access_token = ?, refresh_token = COALESCE(NULLIF(?, ''), refresh_token), token_type = ?, expires_at = ?, scopes = ?, updated_at = ? WHERE id = ?`,
			userID, accessToken, refreshToken, tokenType, expiresAt, scopes, now, existingID,
		)
		return err
	}

	id := uuid.New().String()
	_, err = m.db.Write().ExecContext(ctx,
		`INSERT INTO oauth_accounts (id, user_id, provider, provider_account_id, access_token, refresh_token, token_type, expires_at, scopes, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, userID, provider, providerAccountID, accessToken, refreshToken, tokenType, expiresAt, scopes, now, now,
	)
	return err
}

func (m *Manager) CreateSession(ctx context.Context, userID, userAgent string) (*Session, error) {
	id := uuid.New().String()
	token := generateToken()
	expiresAt := time.Now().Add(30 * 24 * time.Hour)
	now := time.Now()

	_, err := m.db.Write().ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, token, user_agent, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, userID, token, userAgent, expiresAt, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}

	return &Session{
		ID:        id,
		UserID:    userID,
		Token:     token,
		UserAgent: userAgent,
		ExpiresAt: expiresAt,
		CreatedAt: now,
	}, nil
}

func (m *Manager) GetSessionByToken(ctx context.Context, token string) (*Session, error) {
	s := &Session{}
	err := m.db.Read().QueryRowContext(ctx,
		`SELECT id, user_id, token, user_agent, expires_at, created_at FROM sessions WHERE token = ? AND expires_at > ?`,
		token, time.Now(),
	).Scan(&s.ID, &s.UserID, &s.Token, &s.UserAgent, &s.ExpiresAt, &s.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (m *Manager) DeleteSession(ctx context.Context, token string) error {
	_, err := m.db.Write().ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, token)
	return err
}

func (m *Manager) CleanupExpiredSessions(ctx context.Context) error {
	_, err := m.db.Write().ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, time.Now())
	return err
}
