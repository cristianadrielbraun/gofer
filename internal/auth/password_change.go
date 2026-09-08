package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrCurrentPasswordInvalid = errors.New("current password is incorrect")
	ErrPasswordUnchanged      = errors.New("new password must be different from the current password")
)

type PasswordChangeOptions struct {
	SessionToken    string
	CurrentPassword string
	NewPassword     string
	UserAgent       string
}

type PasswordChangeResult struct {
	Session         *Session
	RevokedSessions int64
}

type passwordChangeCandidate struct {
	session      *Session
	passwordHash string
	username     string
}

// ChangePassword verifies the current local credential and atomically replaces
// it, revokes every other session, rotates the current session, and emits a
// credential event. Single-factor accounts receive fresh password step-up
// metadata; MFA-required accounts must present and preserve a recent strong
// step-up.
func (m *Manager) ChangePassword(ctx context.Context, options PasswordChangeOptions) (*PasswordChangeResult, error) {
	candidate, err := m.passwordChangeCandidate(ctx, options.SessionToken)
	if err != nil {
		return nil, err
	}
	if candidate == nil {
		return nil, ErrCurrentPasswordInvalid
	}

	matches, _, err := VerifyPassword(candidate.passwordHash, options.CurrentPassword)
	if err != nil || !matches {
		if eventErr := m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
			current, err := currentSecuritySession(ctx, tx, options.SessionToken, m.clock.Now().UTC(), false)
			if err != nil {
				return err
			}
			if current.ID != candidate.session.ID || current.UserID != candidate.session.UserID {
				return ErrSessionNotActive
			}
			return m.appendTransitionEvent(ctx, tx, current.UserID, current.UserID, current.ID, AuthEventCredentialChanged, false, AuthEventReasonInvalidCredentials, transitionEventMetadata{Method: AuthenticationMethodPassword})
		}); eventErr != nil {
			return nil, eventErr
		}
		return nil, ErrCurrentPasswordInvalid
	}
	preparedPassword, err := PrepareNewPassword(options.NewPassword, PasswordPolicyContext{
		Username: candidate.username,
	})
	if err != nil {
		return nil, err
	}
	preparedCurrent, err := normalizePasswordForHashing(options.CurrentPassword)
	if err != nil {
		return nil, ErrCurrentPasswordInvalid
	}
	if preparedPassword == preparedCurrent {
		return nil, ErrPasswordUnchanged
	}
	newPasswordHash, err := HashPassword(preparedPassword)
	if err != nil {
		return nil, fmt.Errorf("hash changed password: %w", err)
	}

	sessionID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate changed-password session ID: %w", err)
	}
	sessionToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate changed-password session token: %w", err)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate password-change event ID: %w", err)
	}

	now := m.clock.Now().UTC()
	userAgent := boundedUserAgent(options.UserAgent)
	var changedSession *Session
	var revokedSessions int64
	err = m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		current, err := scanSession(tx.QueryRowContext(ctx, sessionSelect+`
			WHERE token_hash = ? AND revoked_at IS NULL
			  AND idle_expires_at > ? AND absolute_expires_at > ?
			  AND EXISTS (
				SELECT 1 FROM users u
				WHERE u.id = sessions.user_id AND u.status = 'active'
				  AND u.auth_version = sessions.auth_version
			  )`, hashToken(options.SessionToken), now, now))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSessionNotActive
		}
		if err != nil {
			return fmt.Errorf("recheck password-change session: %w", err)
		}
		if current.ID != candidate.session.ID || current.UserID != candidate.session.UserID {
			return ErrSessionNotActive
		}
		policy, err := m.requireAuthenticationAssurance(
			ctx, tx, current.UserID, current.AuthVersion, current.AssuranceLevel,
		)
		if err != nil {
			if errors.Is(err, ErrAuthenticationPolicyNotSatisfied) || errors.Is(err, ErrUserNotActive) {
				return ErrSessionNotActive
			}
			return err
		}
		if policy.RequiresMFA && !hasRecentSecurityStepUp(current, policy, now) {
			return ErrRecentStepUpRequired
		}

		var currentHash, username string
		err = tx.QueryRowContext(ctx, `
			SELECT p.password_hash, u.username
			FROM users u
			JOIN password_credentials p ON p.user_id = u.id
			WHERE u.id = ? AND u.status = 'active' AND u.auth_version = ?`,
			current.UserID, current.AuthVersion,
		).Scan(&currentHash, &username)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCurrentPasswordInvalid
		}
		if err != nil {
			return fmt.Errorf("recheck password credential: %w", err)
		}
		if currentHash != candidate.passwordHash || username != candidate.username {
			return ErrCurrentPasswordInvalid
		}

		result, err := tx.ExecContext(ctx, `
			UPDATE password_credentials
			SET password_hash = ?, must_change = 0, changed_at = ?
			WHERE user_id = ? AND password_hash = ?`,
			newPasswordHash, now, current.UserID, currentHash,
		)
		if err != nil {
			return fmt.Errorf("replace password credential: %w", err)
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count replaced password credentials: %w", err)
		}
		if rowsAffected != 1 {
			return ErrCurrentPasswordInvalid
		}

		result, err = tx.ExecContext(ctx, `
			UPDATE sessions
			SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
			WHERE user_id = ? AND id != ? AND revoked_at IS NULL`,
			now, current.UserID, SessionRevocationCredentialReset,
			current.UserID, current.ID,
		)
		if err != nil {
			return fmt.Errorf("revoke other sessions after password change: %w", err)
		}
		revokedSessions, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count sessions revoked after password change: %w", err)
		}

		result, err = tx.ExecContext(ctx, `
			UPDATE sessions
			SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
			WHERE id = ? AND revoked_at IS NULL`,
			now, current.UserID, SessionRevocationRotation, current.ID,
		)
		if err != nil {
			return fmt.Errorf("revoke current session after password change: %w", err)
		}
		rowsAffected, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count current password-change session: %w", err)
		}
		if rowsAffected != 1 {
			return ErrSessionNotActive
		}

		idleExpiresAt := now.Add(sessionIdleLifetime)
		if idleExpiresAt.After(current.AbsoluteExpiresAt) {
			idleExpiresAt = current.AbsoluteExpiresAt
		}
		stepUpAt := now
		stepUpMethod := AuthenticationMethodPassword
		if policy.RequiresMFA {
			stepUpAt = current.StepUpAt.UTC()
			stepUpMethod = current.StepUpMethod
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (
				id, user_id, token_hash, auth_version, authentication_method,
				assurance_level, user_agent, authenticated_at, last_used_at,
				idle_expires_at, absolute_expires_at, step_up_at, step_up_method,
				created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, current.UserID, hashToken(sessionToken), current.AuthVersion,
			current.AuthenticationMethod, current.AssuranceLevel, userAgent,
			current.AuthenticatedAt, now, idleExpiresAt, current.AbsoluteExpiresAt,
			stepUpAt, stepUpMethod, now,
		); err != nil {
			return fmt.Errorf("insert changed-password session: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, '{}')`,
			eventID, now, current.UserID, current.UserID, sessionID,
			AuthEventCredentialChanged, AuthEventReasonChallengeVerified, userAgent,
		); err != nil {
			return fmt.Errorf("record password-change event: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `UPDATE users SET updated_at = ? WHERE id = ?`, now, current.UserID); err != nil {
			return fmt.Errorf("update password-change user timestamp: %w", err)
		}

		changedSession = &Session{
			ID:                   sessionID,
			UserID:               current.UserID,
			Token:                sessionToken,
			AuthVersion:          current.AuthVersion,
			AuthenticationMethod: current.AuthenticationMethod,
			AssuranceLevel:       current.AssuranceLevel,
			UserAgent:            userAgent,
			AuthenticatedAt:      current.AuthenticatedAt,
			LastUsedAt:           now,
			IdleExpiresAt:        idleExpiresAt,
			AbsoluteExpiresAt:    current.AbsoluteExpiresAt,
			StepUpAt:             &stepUpAt,
			StepUpMethod:         stepUpMethod,
			CreatedAt:            now,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &PasswordChangeResult{Session: changedSession, RevokedSessions: revokedSessions}, nil
}

func (m *Manager) HasPasswordCredential(ctx context.Context, userID string) (bool, error) {
	if strings.TrimSpace(userID) == "" {
		return false, nil
	}
	var exists int
	if err := m.db.Read().QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM password_credentials p
			JOIN users u ON u.id = p.user_id
			WHERE p.user_id = ? AND u.status = 'active'
		)`, userID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check password credential: %w", err)
	}
	return exists == 1, nil
}

func (m *Manager) passwordChangeCandidate(ctx context.Context, sessionToken string) (*passwordChangeCandidate, error) {
	if strings.TrimSpace(sessionToken) == "" {
		return nil, nil
	}
	session, err := m.GetSessionByToken(ctx, sessionToken)
	if err != nil {
		return nil, fmt.Errorf("load password-change session: %w", err)
	}
	if session == nil {
		return nil, nil
	}
	candidate := &passwordChangeCandidate{session: session}
	err = m.db.Read().QueryRowContext(ctx, `
		SELECT p.password_hash, u.username
		FROM users u
		JOIN password_credentials p ON p.user_id = u.id
		WHERE u.id = ? AND u.status = 'active' AND u.auth_version = ?`,
		session.UserID, session.AuthVersion,
	).Scan(&candidate.passwordHash, &candidate.username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load password credential: %w", err)
	}
	return candidate, nil
}
