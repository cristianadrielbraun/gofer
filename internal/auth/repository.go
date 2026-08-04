package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrLastActiveAdmin = errors.New("cannot deactivate the last active administrator")
var ErrUserNotActive = errors.New("user is not active")

const userSelect = `SELECT id, email, COALESCE(email_normalized, ''),
	COALESCE(username, ''), COALESCE(username_normalized, ''), name, avatar_url,
	status, auth_version, mfa_required, last_login_at, disabled_at,
	COALESCE(disabled_by, ''), is_admin, created_at, updated_at
	FROM users`

type rowScanner interface {
	Scan(...any) error
}

func normalizeLoginIdentifier(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func scanUser(row rowScanner) (*User, error) {
	user := &User{}
	var isAdmin, mfaRequired int
	var lastLoginAt, disabledAt sql.NullTime
	if err := row.Scan(
		&user.ID, &user.Email, &user.EmailNormalized, &user.Username, &user.UsernameNormalized,
		&user.Name, &user.AvatarURL, &user.Status, &user.AuthVersion, &mfaRequired,
		&lastLoginAt, &disabledAt, &user.DisabledBy, &isAdmin, &user.CreatedAt, &user.UpdatedAt,
	); err != nil {
		return nil, err
	}
	user.IsAdmin = isAdmin == 1
	user.MFARequired = mfaRequired == 1
	if lastLoginAt.Valid {
		user.LastLoginAt = &lastLoginAt.Time
	}
	if disabledAt.Valid {
		user.DisabledAt = &disabledAt.Time
	}
	return user, nil
}

func (m *Manager) CreateOrUpdateUser(ctx context.Context, email, name, avatarURL string) (*User, error) {
	email = strings.TrimSpace(email)
	emailNormalized := normalizeLoginIdentifier(email)
	if emailNormalized == "" {
		return nil, errors.New("email is required")
	}
	existing, err := scanUser(m.db.Read().QueryRowContext(ctx,
		userSelect+` WHERE email_normalized = ? OR (email_normalized IS NULL AND lower(trim(email)) = ?)`,
		emailNormalized, emailNormalized,
	))
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("lookup user: %w", err)
	}

	if err == nil {
		now := m.clock.Now()
		if name != "" {
			_, err = m.db.Write().ExecContext(ctx,
				`UPDATE users SET email = ?, email_normalized = ?, name = ?, avatar_url = ?, updated_at = ? WHERE id = ?`,
				email, emailNormalized, name, avatarURL, now, existing.ID,
			)
			if err != nil {
				return nil, fmt.Errorf("update user: %w", err)
			}
			existing.Name = name
			existing.AvatarURL = avatarURL
		} else if _, err := m.db.Write().ExecContext(ctx,
			`UPDATE users SET email = ?, email_normalized = ?, updated_at = ? WHERE id = ?`,
			email, emailNormalized, now, existing.ID,
		); err != nil {
			return nil, fmt.Errorf("normalize user email: %w", err)
		}
		existing.Email = email
		existing.EmailNormalized = emailNormalized
		return existing, nil
	}

	var userCount int
	m.db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&userCount)

	isAdminVal := 0
	if userCount == 0 {
		isAdminVal = 1
	}

	id, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate user ID: %w", err)
	}
	now := m.clock.Now()
	_, err = m.db.Write().ExecContext(ctx,
		`INSERT INTO users (id, email, email_normalized, name, avatar_url, status, is_admin, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, email, emailNormalized, name, avatarURL, UserStatusActive, isAdminVal, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}

	return &User{
		ID:              id,
		Email:           email,
		EmailNormalized: emailNormalized,
		Name:            name,
		AvatarURL:       avatarURL,
		Status:          UserStatusActive,
		AuthVersion:     1,
		IsAdmin:         isAdminVal == 1,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

func (m *Manager) GetUserByLoginIdentifier(ctx context.Context, identifier string) (*User, error) {
	normalized := normalizeLoginIdentifier(identifier)
	if normalized == "" {
		return nil, nil
	}
	user, err := scanUser(m.db.Read().QueryRowContext(ctx, userSelect+`
		WHERE email_normalized = ? OR username_normalized = ?`, normalized, normalized))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return user, err
}

func (m *Manager) SetUserStatus(ctx context.Context, userID string, status UserStatus, disabledBy string) error {
	if status != UserStatusPending && status != UserStatusActive && status != UserStatusDisabled {
		return fmt.Errorf("invalid user status %q", status)
	}
	return m.runSecurityTransition(ctx, SecurityTransitionUserStatus, func(tx *sql.Tx) error {
		var isAdmin int
		var currentStatus UserStatus
		if err := tx.QueryRowContext(ctx, `SELECT is_admin, status FROM users WHERE id = ?`, userID).Scan(&isAdmin, &currentStatus); err != nil {
			return err
		}
		if currentStatus == status {
			return nil
		}
		if currentStatus == UserStatusActive && status != UserStatusActive && isAdmin == 1 {
			var otherActiveAdmins int
			if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM users
			WHERE id != ? AND is_admin = 1 AND status = 'active'`, userID).Scan(&otherActiveAdmins); err != nil {
				return err
			}
			if otherActiveAdmins == 0 {
				return ErrLastActiveAdmin
			}
		}

		now := m.clock.Now().UTC()
		if status == UserStatusDisabled {
			var actor any
			if actorID := strings.TrimSpace(disabledBy); actorID != "" {
				actor = actorID
			}
			if _, err := tx.ExecContext(ctx, `
			UPDATE users
			SET status = ?, disabled_at = ?, disabled_by = ?, auth_version = auth_version + 1, updated_at = ?
			WHERE id = ?`, status, now, actor, now, userID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE sessions
				SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
				WHERE user_id = ? AND revoked_at IS NULL`,
				now, actor, SessionRevocationUserDisabled, userID,
			); err != nil {
				return err
			}
		} else if status == UserStatusPending {
			if _, err := tx.ExecContext(ctx, `
			UPDATE users
			SET status = ?, disabled_at = NULL, disabled_by = NULL,
				auth_version = auth_version + 1, updated_at = ?
			WHERE id = ?`, status, now, userID); err != nil {
				return err
			}
			var actor any
			if actorID := strings.TrimSpace(disabledBy); actorID != "" {
				actor = actorID
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE sessions
				SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
				WHERE user_id = ? AND revoked_at IS NULL`,
				now, actor, SessionRevocationUserStatusChanged, userID,
			); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
			UPDATE users
			SET status = ?, disabled_at = NULL, disabled_by = NULL, updated_at = ?
			WHERE id = ?`, status, now, userID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *Manager) UpsertOAuthAccount(ctx context.Context, userID, provider, providerAccountID, accessToken, refreshToken, tokenType string, expiresAt *time.Time, scopes string) error {
	now := m.clock.Now()

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

	id, err := m.tokens.ID()
	if err != nil {
		return fmt.Errorf("generate OAuth account ID: %w", err)
	}
	_, err = m.db.Write().ExecContext(ctx,
		`INSERT INTO oauth_accounts (id, user_id, provider, provider_account_id, access_token, refresh_token, token_type, expires_at, scopes, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, userID, provider, providerAccountID, accessToken, refreshToken, tokenType, expiresAt, scopes, now, now,
	)
	return err
}
