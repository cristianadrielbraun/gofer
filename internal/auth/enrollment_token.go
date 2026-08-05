package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrAdministratorRequired        = errors.New("active administrator required")
	ErrRecentStepUpRequired         = errors.New("recent administrator verification required")
	ErrEnrollmentTokenTargetInvalid = errors.New("user is not eligible for this token purpose")
)

type EnrollmentTokenPurpose string

const (
	EnrollmentTokenPurposeEnrollment      EnrollmentTokenPurpose = "enrollment"
	EnrollmentTokenPurposeCredentialReset EnrollmentTokenPurpose = "credential_reset"

	defaultEnrollmentTokenLifetime      = 24 * time.Hour
	maximumEnrollmentTokenLifetime      = 7 * 24 * time.Hour
	defaultCredentialResetTokenLifetime = 30 * time.Minute
	maximumCredentialResetTokenLifetime = 24 * time.Hour
	minimumEnrollmentTokenLifetime      = time.Minute
	enrollmentTokenRetention            = 30 * 24 * time.Hour
	enrollmentTokenCleanupBatchSize     = 500
	administratorStepUpMaximumAge       = 10 * time.Minute
)

func (purpose EnrollmentTokenPurpose) Valid() bool {
	return purpose == EnrollmentTokenPurposeEnrollment || purpose == EnrollmentTokenPurposeCredentialReset
}

type EnrollmentToken struct {
	ID        string
	Token     string
	UserID    string
	CreatedBy string
	Purpose   EnrollmentTokenPurpose
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
	RevokedAt *time.Time
}

type IssueEnrollmentTokenOptions struct {
	UserID         string
	CreatedBy      string
	ActorSessionID string
	Purpose        EnrollmentTokenPurpose
	Lifetime       time.Duration
}

const enrollmentTokenSelect = `SELECT id, user_id, COALESCE(created_by, ''), purpose,
	created_at, expires_at, used_at, revoked_at
	FROM user_enrollment_tokens`

func scanEnrollmentToken(row rowScanner) (*EnrollmentToken, error) {
	token := &EnrollmentToken{}
	var usedAt, revokedAt sql.NullTime
	if err := row.Scan(
		&token.ID, &token.UserID, &token.CreatedBy, &token.Purpose,
		&token.CreatedAt, &token.ExpiresAt, &usedAt, &revokedAt,
	); err != nil {
		return nil, err
	}
	if usedAt.Valid {
		token.UsedAt = &usedAt.Time
	}
	if revokedAt.Valid {
		token.RevokedAt = &revokedAt.Time
	}
	return token, nil
}

// IssueEnrollmentToken returns the raw secret only in the newly issued value.
// SQLite stores only its hash. Issuing another token atomically revokes every
// unused token for the same user and purpose before inserting the replacement.
func (m *Manager) IssueEnrollmentToken(ctx context.Context, options IssueEnrollmentTokenOptions) (*EnrollmentToken, error) {
	userID := strings.TrimSpace(options.UserID)
	createdBy := strings.TrimSpace(options.CreatedBy)
	actorSessionID := strings.TrimSpace(options.ActorSessionID)
	if userID == "" {
		return nil, ErrEnrollmentTokenTargetInvalid
	}
	if createdBy == "" {
		return nil, ErrAdministratorRequired
	}
	if actorSessionID == "" {
		return nil, ErrRecentStepUpRequired
	}
	if !options.Purpose.Valid() {
		return nil, fmt.Errorf("invalid enrollment token purpose %q", options.Purpose)
	}
	lifetime, err := enrollmentTokenLifetime(options.Purpose, options.Lifetime)
	if err != nil {
		return nil, err
	}

	id, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate enrollment token ID: %w", err)
	}
	rawToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate enrollment token: %w", err)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate enrollment token event ID: %w", err)
	}

	now := m.clock.Now().UTC()
	token := &EnrollmentToken{
		ID:        id,
		Token:     rawToken,
		UserID:    userID,
		CreatedBy: createdBy,
		Purpose:   options.Purpose,
		CreatedAt: now,
		ExpiresAt: now.Add(lifetime),
	}
	err = m.runSecurityTransition(ctx, SecurityTransitionEnrollment, func(tx *sql.Tx) error {
		if err := requireActiveAdministrator(ctx, tx, createdBy); err != nil {
			return err
		}
		if err := requireRecentAdministratorStepUp(ctx, tx, createdBy, actorSessionID, now); err != nil {
			return err
		}
		if err := requireEnrollmentTokenTarget(ctx, tx, userID, options.Purpose); err != nil {
			return err
		}

		replaced, err := tx.ExecContext(ctx, `
			UPDATE user_enrollment_tokens
			SET revoked_at = ?
			WHERE user_id = ? AND purpose = ?
			  AND used_at IS NULL AND revoked_at IS NULL`,
			now, userID, options.Purpose,
		)
		if err != nil {
			return fmt.Errorf("revoke replaced enrollment tokens: %w", err)
		}
		replacedCount, err := replaced.RowsAffected()
		if err != nil {
			return fmt.Errorf("count replaced enrollment tokens: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_enrollment_tokens (
				id, user_id, created_by, token_hash, purpose, created_at, expires_at
			) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			token.ID, token.UserID, token.CreatedBy, hashToken(token.Token), token.Purpose,
			token.CreatedAt, token.ExpiresAt,
		); err != nil {
			return fmt.Errorf("insert enrollment token: %w", err)
		}

		metadata, err := enrollmentTokenEventJSON(token.ID, token.Purpose, replacedCount)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
				success, reason, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, createdBy, userID, actorSessionID, AuthEventEnrollmentIssued,
			AuthEventReasonAdministratorAction, metadata,
		); err != nil {
			return fmt.Errorf("record enrollment token issuance: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return token, nil
}

// ListEnrollmentTokens returns metadata only; raw token values and hashes are
// never exposed through this read path.
func (m *Manager) ListEnrollmentTokens(ctx context.Context, actorID, userID string) ([]EnrollmentToken, error) {
	actorID = strings.TrimSpace(actorID)
	userID = strings.TrimSpace(userID)
	if err := m.requireActiveAdministrator(ctx, actorID); err != nil {
		return nil, err
	}
	rows, err := m.db.Read().QueryContext(ctx, enrollmentTokenSelect+`
		WHERE user_id = ? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list enrollment tokens: %w", err)
	}
	defer rows.Close()
	var tokens []EnrollmentToken
	for rows.Next() {
		token, err := scanEnrollmentToken(rows)
		if err != nil {
			return nil, fmt.Errorf("scan enrollment token: %w", err)
		}
		tokens = append(tokens, *token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate enrollment tokens: %w", err)
	}
	return tokens, nil
}

// RevokeEnrollmentToken uses the target user and token ID together so foreign
// and missing records are indistinguishable to the caller.
func (m *Manager) RevokeEnrollmentToken(ctx context.Context, actorID, actorSessionID, userID, tokenID string) (bool, error) {
	actorID = strings.TrimSpace(actorID)
	actorSessionID = strings.TrimSpace(actorSessionID)
	userID = strings.TrimSpace(userID)
	tokenID = strings.TrimSpace(tokenID)
	if actorID == "" {
		return false, ErrAdministratorRequired
	}
	if actorSessionID == "" {
		return false, ErrRecentStepUpRequired
	}
	if userID == "" || tokenID == "" {
		return false, nil
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return false, fmt.Errorf("generate enrollment revocation event ID: %w", err)
	}
	now := m.clock.Now().UTC()
	revoked := false
	err = m.runSecurityTransition(ctx, SecurityTransitionEnrollment, func(tx *sql.Tx) error {
		if err := requireActiveAdministrator(ctx, tx, actorID); err != nil {
			return err
		}
		if err := requireRecentAdministratorStepUp(ctx, tx, actorID, actorSessionID, now); err != nil {
			return err
		}
		var purpose EnrollmentTokenPurpose
		err := tx.QueryRowContext(ctx, `
			UPDATE user_enrollment_tokens
			SET revoked_at = ?
			WHERE id = ? AND user_id = ? AND used_at IS NULL AND revoked_at IS NULL
			  AND expires_at > ?
			RETURNING purpose`,
			now, tokenID, userID, now,
		).Scan(&purpose)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("revoke enrollment token: %w", err)
		}
		metadata, err := enrollmentTokenEventJSON(tokenID, purpose, 0)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
				success, reason, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, actorID, userID, actorSessionID, AuthEventEnrollmentRevoked,
			AuthEventReasonAdministratorAction, metadata,
		); err != nil {
			return fmt.Errorf("record enrollment token revocation: %w", err)
		}
		revoked = true
		return nil
	})
	return revoked, err
}

func (m *Manager) CleanupEnrollmentTokens(ctx context.Context) error {
	now := m.clock.Now().UTC()
	retainedAfter := now.Add(-enrollmentTokenRetention)
	_, err := m.db.Write().ExecContext(ctx, `
		DELETE FROM user_enrollment_tokens
		WHERE id IN (
			SELECT id FROM user_enrollment_tokens
			WHERE (used_at IS NULL AND revoked_at IS NULL AND expires_at <= ?)
			   OR (used_at IS NOT NULL AND used_at <= ?)
			   OR (revoked_at IS NOT NULL AND revoked_at <= ?)
			ORDER BY COALESCE(used_at, revoked_at, expires_at), id
			LIMIT ?
		)`, now, retainedAfter, retainedAfter, enrollmentTokenCleanupBatchSize)
	if err != nil {
		return fmt.Errorf("clean enrollment tokens: %w", err)
	}
	return nil
}

func enrollmentTokenLifetime(purpose EnrollmentTokenPurpose, requested time.Duration) (time.Duration, error) {
	defaultLifetime := defaultEnrollmentTokenLifetime
	maximumLifetime := maximumEnrollmentTokenLifetime
	if purpose == EnrollmentTokenPurposeCredentialReset {
		defaultLifetime = defaultCredentialResetTokenLifetime
		maximumLifetime = maximumCredentialResetTokenLifetime
	}
	if requested == 0 {
		return defaultLifetime, nil
	}
	if requested < minimumEnrollmentTokenLifetime || requested > maximumLifetime {
		return 0, fmt.Errorf("%s token lifetime must be between %s and %s", purpose, minimumEnrollmentTokenLifetime, maximumLifetime)
	}
	return requested, nil
}

func requireActiveAdministrator(ctx context.Context, tx *sql.Tx, userID string) error {
	var authorized int
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM users WHERE id = ? AND status = 'active' AND is_admin = 1
		)`, userID).Scan(&authorized); err != nil {
		return fmt.Errorf("check active administrator: %w", err)
	}
	if authorized != 1 {
		return ErrAdministratorRequired
	}
	return nil
}

func requireRecentAdministratorStepUp(ctx context.Context, tx *sql.Tx, userID, sessionID string, now time.Time) error {
	var authorized int
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM users u
			JOIN sessions s ON s.user_id = u.id
			WHERE u.id = ? AND u.status = 'active' AND u.is_admin = 1
			  AND s.id = ? AND s.revoked_at IS NULL
			  AND s.auth_version = u.auth_version
			  AND s.idle_expires_at > ? AND s.absolute_expires_at > ?
			  AND s.step_up_at IS NOT NULL AND s.step_up_at <= ? AND s.step_up_at >= ?
			  AND s.step_up_method <> ''
		)`, userID, sessionID, now, now, now, now.Add(-administratorStepUpMaximumAge)).Scan(&authorized); err != nil {
		return fmt.Errorf("check recent administrator verification: %w", err)
	}
	if authorized != 1 {
		return ErrRecentStepUpRequired
	}
	return nil
}

func (m *Manager) requireActiveAdministrator(ctx context.Context, userID string) error {
	var authorized int
	if err := m.db.Read().QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM users WHERE id = ? AND status = 'active' AND is_admin = 1
		)`, strings.TrimSpace(userID)).Scan(&authorized); err != nil {
		return fmt.Errorf("check active administrator: %w", err)
	}
	if authorized != 1 {
		return ErrAdministratorRequired
	}
	return nil
}

func requireEnrollmentTokenTarget(ctx context.Context, tx *sql.Tx, userID string, purpose EnrollmentTokenPurpose) error {
	var status UserStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM users WHERE id = ?`, userID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEnrollmentTokenTargetInvalid
		}
		return fmt.Errorf("read enrollment token target: %w", err)
	}
	eligible := purpose == EnrollmentTokenPurposeEnrollment && status == UserStatusPending ||
		purpose == EnrollmentTokenPurposeCredentialReset && (status == UserStatusActive || status == UserStatusDisabled)
	if !eligible {
		return ErrEnrollmentTokenTargetInvalid
	}
	return nil
}

func enrollmentTokenEventJSON(tokenID string, purpose EnrollmentTokenPurpose, replacedCount int64) (string, error) {
	metadata := struct {
		TokenID       string                 `json:"token_id"`
		Purpose       EnrollmentTokenPurpose `json:"purpose"`
		ReplacedCount int64                  `json:"replaced_count,omitempty"`
	}{TokenID: tokenID, Purpose: purpose, ReplacedCount: replacedCount}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode enrollment token event metadata: %w", err)
	}
	return string(encoded), nil
}
