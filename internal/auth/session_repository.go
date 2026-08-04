package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrSessionNotActive = errors.New("session is not active")

const (
	sessionAbsoluteLifetime      = 30 * 24 * time.Hour
	sessionIdleLifetime          = 30 * 24 * time.Hour
	sessionLastUsedWriteInterval = 5 * time.Minute
	sessionRevokedRetention      = 30 * 24 * time.Hour
	sessionCleanupBatchSize      = 500
)

const sessionSelect = `SELECT id, user_id, auth_version, authentication_method,
	assurance_level, user_agent, authenticated_at, last_used_at,
	idle_expires_at, absolute_expires_at, step_up_at, step_up_method,
	revoked_at, COALESCE(revoked_by, ''), revocation_reason, created_at
	FROM sessions`

func scanSession(row rowScanner) (*Session, error) {
	session := &Session{}
	var stepUpAt, revokedAt sql.NullTime
	if err := row.Scan(
		&session.ID, &session.UserID, &session.AuthVersion,
		&session.AuthenticationMethod, &session.AssuranceLevel, &session.UserAgent,
		&session.AuthenticatedAt, &session.LastUsedAt, &session.IdleExpiresAt,
		&session.AbsoluteExpiresAt, &stepUpAt, &session.StepUpMethod,
		&revokedAt, &session.RevokedBy, &session.RevocationReason, &session.CreatedAt,
	); err != nil {
		return nil, err
	}
	if stepUpAt.Valid {
		session.StepUpAt = &stepUpAt.Time
	}
	if revokedAt.Valid {
		session.RevokedAt = &revokedAt.Time
	}
	return session, nil
}

func (m *Manager) CreateSession(ctx context.Context, userID, userAgent string) (*Session, error) {
	return m.CreateAuthenticatedSession(ctx, userID, userAgent, AuthenticationMethodLegacy, AssuranceLevelLegacy)
}

func (m *Manager) CreateAuthenticatedSession(ctx context.Context, userID, userAgent string, method AuthenticationMethod, assurance AssuranceLevel) (*Session, error) {
	if !method.Valid() {
		return nil, fmt.Errorf("invalid authentication method %q", method)
	}
	if !assurance.Valid() {
		return nil, fmt.Errorf("invalid assurance level %q", assurance)
	}
	id, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate session ID: %w", err)
	}
	token, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate session token: %w", err)
	}
	now := m.clock.Now().UTC()
	idleExpiresAt := now.Add(sessionIdleLifetime)
	absoluteExpiresAt := now.Add(sessionAbsoluteLifetime)
	if idleExpiresAt.After(absoluteExpiresAt) {
		idleExpiresAt = absoluteExpiresAt
	}
	var authVersion int64
	err = m.db.Write().QueryRowContext(ctx,
		`INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method, assurance_level,
			user_agent, authenticated_at, last_used_at, idle_expires_at, absolute_expires_at, created_at
		)
		 SELECT ?, u.id, ?, u.auth_version, ?, ?, ?, ?, ?, ?, ?, ?
		 FROM users u
		 WHERE u.id = ? AND u.status = 'active'
		 RETURNING auth_version`,
		id, hashToken(token), method, assurance, userAgent,
		now, now, idleExpiresAt, absoluteExpiresAt, now, userID,
	).Scan(&authVersion)
	if err == sql.ErrNoRows {
		return nil, ErrUserNotActive
	}
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	return &Session{
		ID:                   id,
		UserID:               userID,
		Token:                token,
		AuthVersion:          authVersion,
		AuthenticationMethod: method,
		AssuranceLevel:       assurance,
		UserAgent:            userAgent,
		AuthenticatedAt:      now,
		LastUsedAt:           now,
		IdleExpiresAt:        idleExpiresAt,
		AbsoluteExpiresAt:    absoluteExpiresAt,
		CreatedAt:            now,
	}, nil
}

func (m *Manager) GetSessionByToken(ctx context.Context, token string) (*Session, error) {
	if strings.TrimSpace(token) == "" {
		return nil, nil
	}
	now := m.clock.Now().UTC()
	session, err := scanSession(m.db.Read().QueryRowContext(ctx, sessionSelect+`
		WHERE token_hash = ? AND revoked_at IS NULL
		  AND idle_expires_at > ? AND absolute_expires_at > ?
		  AND EXISTS (
			SELECT 1 FROM users u
			WHERE u.id = sessions.user_id AND u.status = 'active'
			  AND u.auth_version = sessions.auth_version
		  )`, hashToken(token), now, now))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := m.touchSession(ctx, session, now); err != nil {
		return nil, err
	}
	return session, nil
}

func (m *Manager) touchSession(ctx context.Context, session *Session, now time.Time) error {
	if session.LastUsedAt.After(now.Add(-sessionLastUsedWriteInterval)) {
		return nil
	}
	idleExpiresAt := now.Add(sessionIdleLifetime)
	if idleExpiresAt.After(session.AbsoluteExpiresAt) {
		idleExpiresAt = session.AbsoluteExpiresAt
	}
	result, err := m.db.Write().ExecContext(ctx, `
		UPDATE sessions
		SET last_used_at = ?, idle_expires_at = ?
		WHERE id = ? AND revoked_at IS NULL AND last_used_at = ?
		  AND idle_expires_at > ? AND absolute_expires_at > ?`,
		now, idleExpiresAt, session.ID, session.LastUsedAt, now, now,
	)
	if err != nil {
		return fmt.Errorf("update session activity: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect session activity update: %w", err)
	}
	if changed == 1 {
		session.LastUsedAt = now
		session.IdleExpiresAt = idleExpiresAt
	}
	return nil
}

func (m *Manager) ListSessions(ctx context.Context, userID string) ([]Session, error) {
	rows, err := m.db.Read().QueryContext(ctx, sessionSelect+`
		WHERE user_id = ? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, *session)
	}
	return sessions, rows.Err()
}

func (m *Manager) RevokeSessionByToken(ctx context.Context, token, actorID string, reason SessionRevocationReason) (bool, error) {
	if strings.TrimSpace(token) == "" {
		return false, nil
	}
	return m.revokeSessions(ctx, `token_hash = ?`, []any{hashToken(token)}, actorID, reason)
}

func (m *Manager) RevokeSession(ctx context.Context, userID, sessionID, actorID string, reason SessionRevocationReason) (bool, error) {
	return m.revokeSessions(ctx, `user_id = ? AND id = ?`, []any{userID, sessionID}, actorID, reason)
}

func (m *Manager) RevokeAllSessions(ctx context.Context, userID, actorID string, reason SessionRevocationReason) (int64, error) {
	changed, err := m.revokeSessionRows(ctx, `user_id = ?`, []any{userID}, actorID, reason)
	return changed, err
}

func (m *Manager) RecordSessionStepUp(ctx context.Context, userID, sessionID string, method AuthenticationMethod) (bool, error) {
	if !method.Valid() || method == AuthenticationMethodLegacy {
		return false, fmt.Errorf("invalid step-up authentication method %q", method)
	}
	now := m.clock.Now().UTC()
	result, err := m.db.Write().ExecContext(ctx, `
		UPDATE sessions
		SET step_up_at = ?, step_up_method = ?
		WHERE id = ? AND user_id = ? AND revoked_at IS NULL
		  AND idle_expires_at > ? AND absolute_expires_at > ?
		  AND EXISTS (
			SELECT 1 FROM users u
			WHERE u.id = sessions.user_id AND u.status = 'active'
			  AND u.auth_version = sessions.auth_version
		  )`, now, method, sessionID, userID, now, now)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (m *Manager) RevokeOtherSessions(ctx context.Context, userID, currentSessionID, actorID string, reason SessionRevocationReason) (int64, error) {
	if !reason.Valid() {
		return 0, fmt.Errorf("invalid session revocation reason %q", reason)
	}
	tx, err := m.db.Write().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := m.clock.Now().UTC()
	var active int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sessions
		WHERE id = ? AND user_id = ? AND revoked_at IS NULL
		  AND idle_expires_at > ? AND absolute_expires_at > ?
		  AND EXISTS (
			SELECT 1 FROM users u
			WHERE u.id = sessions.user_id AND u.status = 'active'
			  AND u.auth_version = sessions.auth_version
		  )`, currentSessionID, userID, now, now).Scan(&active); err != nil {
		return 0, err
	}
	if active != 1 {
		return 0, ErrSessionNotActive
	}
	var actor any
	if value := strings.TrimSpace(actorID); value != "" {
		actor = value
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
		WHERE revoked_at IS NULL AND user_id = ? AND id != ?`,
		now, actor, reason, userID, currentSessionID,
	)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changed, nil
}

func (m *Manager) revokeSessions(ctx context.Context, predicate string, args []any, actorID string, reason SessionRevocationReason) (bool, error) {
	changed, err := m.revokeSessionRows(ctx, predicate, args, actorID, reason)
	return changed == 1, err
}

func (m *Manager) revokeSessionRows(ctx context.Context, predicate string, args []any, actorID string, reason SessionRevocationReason) (int64, error) {
	if !reason.Valid() {
		return 0, fmt.Errorf("invalid session revocation reason %q", reason)
	}
	var actor any
	if value := strings.TrimSpace(actorID); value != "" {
		actor = value
	}
	queryArgs := []any{m.clock.Now().UTC(), actor, reason}
	queryArgs = append(queryArgs, args...)
	result, err := m.db.Write().ExecContext(ctx, `
		UPDATE sessions
		SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
		WHERE revoked_at IS NULL AND `+predicate, queryArgs...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (m *Manager) RotateSession(ctx context.Context, currentToken, userAgent string) (*Session, error) {
	if strings.TrimSpace(currentToken) == "" {
		return nil, ErrSessionNotActive
	}
	id, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate rotated session ID: %w", err)
	}
	token, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate rotated session token: %w", err)
	}
	now := m.clock.Now().UTC()
	var rotated *Session
	err = m.runSecurityTransition(ctx, SecurityTransitionSessionRotation, func(tx *sql.Tx) error {
		current, err := scanSession(tx.QueryRowContext(ctx, sessionSelect+`
		WHERE token_hash = ? AND revoked_at IS NULL
		  AND idle_expires_at > ? AND absolute_expires_at > ?
		  AND EXISTS (
			SELECT 1 FROM users u
			WHERE u.id = sessions.user_id AND u.status = 'active'
			  AND u.auth_version = sessions.auth_version
		  )`, hashToken(currentToken), now, now))
		if err == sql.ErrNoRows {
			return ErrSessionNotActive
		}
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
		WHERE id = ? AND revoked_at IS NULL`,
			now, current.UserID, SessionRevocationRotation, current.ID,
		)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrSessionNotActive
		}
		idleExpiresAt := now.Add(sessionIdleLifetime)
		if idleExpiresAt.After(current.AbsoluteExpiresAt) {
			idleExpiresAt = current.AbsoluteExpiresAt
		}
		if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method, assurance_level,
			user_agent, authenticated_at, last_used_at, idle_expires_at, absolute_expires_at,
			step_up_at, step_up_method, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, current.UserID, hashToken(token), current.AuthVersion,
			current.AuthenticationMethod, current.AssuranceLevel, userAgent,
			current.AuthenticatedAt, now, idleExpiresAt, current.AbsoluteExpiresAt,
			nullableSessionTime(current.StepUpAt), current.StepUpMethod, now,
		); err != nil {
			return err
		}
		rotated = &Session{
			ID:                   id,
			UserID:               current.UserID,
			Token:                token,
			AuthVersion:          current.AuthVersion,
			AuthenticationMethod: current.AuthenticationMethod,
			AssuranceLevel:       current.AssuranceLevel,
			UserAgent:            userAgent,
			AuthenticatedAt:      current.AuthenticatedAt,
			LastUsedAt:           now,
			IdleExpiresAt:        idleExpiresAt,
			AbsoluteExpiresAt:    current.AbsoluteExpiresAt,
			StepUpAt:             current.StepUpAt,
			StepUpMethod:         current.StepUpMethod,
			CreatedAt:            now,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rotated, nil
}

func (m *Manager) CleanupExpiredSessions(ctx context.Context) error {
	now := m.clock.Now().UTC()
	revokedBefore := now.Add(-sessionRevokedRetention)
	_, err := m.db.Write().ExecContext(ctx, `
		DELETE FROM sessions
		WHERE id IN (
			SELECT id FROM sessions
			WHERE idle_expires_at <= ? OR absolute_expires_at <= ?
			   OR (revoked_at IS NOT NULL AND revoked_at <= ?)
			ORDER BY COALESCE(revoked_at, idle_expires_at, absolute_expires_at), id
			LIMIT ?
		)`, now, now, revokedBefore, sessionCleanupBatchSize)
	return err
}

func nullableSessionTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}
