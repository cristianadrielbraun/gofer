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

const userSelect = `SELECT id, username, username_normalized, name, avatar_url,
	status, auth_version, mfa_required, last_login_at, disabled_at,
	COALESCE(disabled_by, ''), user_type, is_admin, created_at, updated_at
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
		&user.ID, &user.Username, &user.UsernameNormalized,
		&user.Name, &user.AvatarURL, &user.Status, &user.AuthVersion, &mfaRequired,
		&lastLoginAt, &disabledAt, &user.DisabledBy, &user.UserType, &isAdmin, &user.CreatedAt, &user.UpdatedAt,
	); err != nil {
		return nil, err
	}
	user.IsAdmin = isAdmin == 1
	user.MFARequired = mfaRequired == 1
	if !user.UserType.Valid() {
		return nil, fmt.Errorf("user %q has invalid type %q", user.ID, user.UserType)
	}
	if lastLoginAt.Valid {
		user.LastLoginAt = &lastLoginAt.Time
	}
	if disabledAt.Valid {
		user.DisabledAt = &disabledAt.Time
	}
	return user, nil
}

func (m *Manager) CreateOrUpdateUser(ctx context.Context, username, name, avatarURL string) (*User, error) {
	username, usernameNormalized, err := PrepareUsername(username)
	if err != nil {
		return nil, err
	}
	existing, err := scanUser(m.db.Read().QueryRowContext(ctx,
		userSelect+` WHERE username_normalized = ?`, usernameNormalized,
	))
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("lookup user: %w", err)
	}

	if err == nil {
		now := m.clock.Now()
		if name != "" {
			_, err = m.db.Write().ExecContext(ctx,
				`UPDATE users SET username = ?, name = ?, avatar_url = ?, updated_at = ? WHERE id = ?`,
				username, name, avatarURL, now, existing.ID,
			)
			if err != nil {
				return nil, fmt.Errorf("update user: %w", err)
			}
			existing.Name = name
			existing.AvatarURL = avatarURL
		} else if _, err := m.db.Write().ExecContext(ctx,
			`UPDATE users SET username = ?, updated_at = ? WHERE id = ?`,
			username, now, existing.ID,
		); err != nil {
			return nil, fmt.Errorf("update user spelling: %w", err)
		}
		existing.Username = username
		return existing, nil
	}

	var id string
	var now time.Time
	isAdminVal := 0
	userType := UserTypeWebmail
	err = m.runSecurityTransition(ctx, SecurityTransitionLoginCompletion, func(tx *sql.Tx) error {
		policy, err := readInstanceSecurityPolicy(ctx, tx)
		if err != nil {
			return err
		}
		if policy.MFA.RequiresAllUsers() {
			return ErrInstanceMFAEnrollmentNeeded
		}

		var userCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&userCount); err != nil {
			return fmt.Errorf("count users before creation: %w", err)
		}
		if userCount == 0 {
			isAdminVal = 1
			userType = UserTypeManagement
		}
		id, err = m.tokens.ID()
		if err != nil {
			return fmt.Errorf("generate user ID: %w", err)
		}
		now = m.clock.Now()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO users (id, username, username_normalized, name, avatar_url, status, user_type, is_admin, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, username, usernameNormalized, name, avatarURL, UserStatusActive, userType, isAdminVal, now, now,
		); err != nil {
			return fmt.Errorf("insert user: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &User{
		ID:                 id,
		Username:           username,
		UsernameNormalized: usernameNormalized,
		Name:               name,
		AvatarURL:          avatarURL,
		Status:             UserStatusActive,
		AuthVersion:        1,
		UserType:           userType,
		IsAdmin:            isAdminVal == 1,
		CreatedAt:          now,
		UpdatedAt:          now,
	}, nil
}

func (m *Manager) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	normalized := normalizeLoginIdentifier(username)
	if normalized == "" {
		return nil, nil
	}
	user, err := scanUser(m.db.Read().QueryRowContext(ctx, userSelect+`
		WHERE username_normalized = ?`, normalized))
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
		_, err := m.setUserStatusTx(ctx, tx, userID, status, disabledBy, m.clock.Now().UTC())
		return err
	})
}

type userStatusTransitionResult struct {
	From            UserStatus
	To              UserStatus
	Changed         bool
	RevokedSessions int64
}

func (m *Manager) setUserStatusTx(
	ctx context.Context, tx *sql.Tx, userID string, status UserStatus, changedBy string, now time.Time,
) (userStatusTransitionResult, error) {
	result := userStatusTransitionResult{To: status}
	var isAdmin, mfaRequired int
	var userType UserType
	var authVersion int64
	if err := tx.QueryRowContext(ctx, `
		SELECT status, user_type, is_admin, mfa_required, auth_version
		FROM users WHERE id = ?`, userID,
	).Scan(&result.From, &userType, &isAdmin, &mfaRequired, &authVersion); err != nil {
		return result, err
	}
	if !userType.Valid() || (isAdmin != 0 && isAdmin != 1) || (mfaRequired != 0 && mfaRequired != 1) {
		return result, fmt.Errorf("user %q has invalid authentication state", userID)
	}
	if result.From == status {
		return result, nil
	}
	if result.From == UserStatusActive && status != UserStatusActive && isAdmin == 1 {
		var otherActiveAdmins int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM users
			WHERE id != ? AND user_type = 'management' AND is_admin = 1 AND status = 'active'`,
			userID,
		).Scan(&otherActiveAdmins); err != nil {
			return result, fmt.Errorf("count other active administrators: %w", err)
		}
		if otherActiveAdmins == 0 {
			return result, ErrLastActiveAdmin
		}
	}
	if status == UserStatusActive {
		instancePolicy, err := readInstanceSecurityPolicy(ctx, tx)
		if err != nil {
			return result, err
		}
		policy := resolveAuthenticationPolicy(
			authVersion, mfaRequired == 1, isAdmin == 1, instancePolicy.MFA.RequiresAllUsers(),
		)
		if err := m.requireUserReadyForAuthenticationPolicy(ctx, tx, userID, policy); err != nil {
			return result, err
		}
	}

	var actor any
	if actorID := strings.TrimSpace(changedBy); actorID != "" {
		actor = actorID
	}
	updated, err := tx.ExecContext(ctx, `
		UPDATE users
		SET status = ?,
			disabled_at = CASE WHEN ? = 'disabled' THEN ? ELSE NULL END,
			disabled_by = CASE WHEN ? = 'disabled' THEN ? ELSE NULL END,
			auth_version = auth_version + CASE WHEN ? IN ('disabled', 'pending') THEN 1 ELSE 0 END,
			updated_at = ?
		WHERE id = ? AND status = ?`,
		status, status, now, status, actor, status, now, userID, result.From,
	)
	if err != nil {
		return result, fmt.Errorf("update user status: %w", err)
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return result, fmt.Errorf("count updated user status rows: %w", err)
	}
	if changed != 1 {
		return result, fmt.Errorf("user status changed concurrently")
	}
	if status == UserStatusDisabled || status == UserStatusPending {
		reason := SessionRevocationUserStatusChanged
		if status == UserStatusDisabled {
			reason = SessionRevocationUserDisabled
		}
		revoked, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
			WHERE user_id = ? AND revoked_at IS NULL`,
			now, actor, reason, userID,
		)
		if err != nil {
			return result, fmt.Errorf("revoke sessions after user status change: %w", err)
		}
		result.RevokedSessions, err = revoked.RowsAffected()
		if err != nil {
			return result, fmt.Errorf("count revoked sessions after user status change: %w", err)
		}
	}
	result.Changed = true
	return result, nil
}
