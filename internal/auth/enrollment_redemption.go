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

var ErrEnrollmentTokenInvalid = errors.New("enrollment token is invalid or no longer active")

type RedeemEnrollmentTokenOptions struct {
	Token       string
	NewPassword string
	UserAgent   string
}

type EnrollmentRedemptionResult struct {
	UserID          string
	Purpose         EnrollmentTokenPurpose
	RevokedSessions int64
}

type enrollmentRedemptionCandidate struct {
	tokenID  string
	userID   string
	purpose  EnrollmentTokenPurpose
	status   UserStatus
	username string
	email    string
}

// RedeemEnrollmentToken establishes the subject's password credential and
// consumes the supplied enrollment or credential-reset token in one security
// transition. Credential reset also invalidates the user's authentication
// version and revokes every existing session. It intentionally does not create
// a new session: the user must authenticate with the newly established secret.
func (m *Manager) RedeemEnrollmentToken(ctx context.Context, options RedeemEnrollmentTokenOptions) (*EnrollmentRedemptionResult, error) {
	rawToken := strings.TrimSpace(options.Token)
	preparedPassword, err := PrepareNewPassword(options.NewPassword, PasswordPolicyContext{})
	if err != nil {
		return nil, err
	}

	now := m.clock.Now().UTC()
	candidate, err := m.findEnrollmentRedemptionCandidate(ctx, rawToken, now)
	if err != nil {
		return nil, err
	}
	if candidate != nil {
		preparedPassword, err = PrepareNewPassword(options.NewPassword, PasswordPolicyContext{
			Username: candidate.username,
			Email:    candidate.email,
		})
		if err != nil {
			return nil, err
		}
	}

	passwordHash, err := HashPassword(preparedPassword)
	if err != nil {
		return nil, fmt.Errorf("hash redeemed password: %w", err)
	}
	if candidate == nil {
		return nil, ErrEnrollmentTokenInvalid
	}

	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate enrollment redemption event ID: %w", err)
	}
	userAgent := boundedUserAgent(options.UserAgent)
	result := &EnrollmentRedemptionResult{UserID: candidate.userID, Purpose: candidate.purpose}
	err = m.runSecurityTransition(ctx, SecurityTransitionEnrollment, func(tx *sql.Tx) error {
		current, err := scanEnrollmentRedemptionCandidate(tx.QueryRowContext(ctx, `
			SELECT t.id, t.user_id, t.purpose, u.status,
			       COALESCE(u.username, ''), u.email
			FROM user_enrollment_tokens t
			JOIN users u ON u.id = t.user_id
			WHERE t.id = ? AND t.token_hash = ?
			  AND t.used_at IS NULL AND t.revoked_at IS NULL AND t.expires_at > ?`,
			candidate.tokenID, hashToken(rawToken), now,
		))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEnrollmentTokenInvalid
		}
		if err != nil {
			return fmt.Errorf("recheck enrollment token: %w", err)
		}
		if !sameEnrollmentRedemptionCandidate(current, candidate) || !enrollmentRedemptionStatusEligible(current.purpose, current.status) {
			return ErrEnrollmentTokenInvalid
		}

		resetAt := any(nil)
		if current.purpose == EnrollmentTokenPurposeCredentialReset {
			resetAt = now
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO password_credentials (
				user_id, password_hash, must_change, created_at, changed_at, reset_at
			) VALUES (?, ?, 0, ?, ?, ?)
			ON CONFLICT(user_id) DO UPDATE SET
				password_hash = excluded.password_hash,
				must_change = 0,
				changed_at = excluded.changed_at,
				reset_at = excluded.reset_at`,
			current.userID, passwordHash, now, now, resetAt,
		); err != nil {
			return fmt.Errorf("establish redeemed password credential: %w", err)
		}

		if current.purpose == EnrollmentTokenPurposeEnrollment {
			changed, err := tx.ExecContext(ctx, `
				UPDATE users
				SET status = 'active', disabled_at = NULL, disabled_by = NULL, updated_at = ?
				WHERE id = ? AND status = 'pending'`, now, current.userID)
			if err != nil {
				return fmt.Errorf("activate enrolled user: %w", err)
			}
			count, err := changed.RowsAffected()
			if err != nil {
				return fmt.Errorf("count activated enrolled users: %w", err)
			}
			if count != 1 {
				return ErrEnrollmentTokenInvalid
			}
		} else {
			changed, err := tx.ExecContext(ctx, `
				UPDATE users
				SET auth_version = auth_version + 1, updated_at = ?
				WHERE id = ? AND status IN ('active', 'disabled')`, now, current.userID)
			if err != nil {
				return fmt.Errorf("invalidate reset user authentication version: %w", err)
			}
			count, err := changed.RowsAffected()
			if err != nil {
				return fmt.Errorf("count reset users: %w", err)
			}
			if count != 1 {
				return ErrEnrollmentTokenInvalid
			}

			revoked, err := tx.ExecContext(ctx, `
				UPDATE sessions
				SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
				WHERE user_id = ? AND revoked_at IS NULL`,
				now, current.userID, SessionRevocationCredentialReset, current.userID,
			)
			if err != nil {
				return fmt.Errorf("revoke sessions after credential reset: %w", err)
			}
			result.RevokedSessions, err = revoked.RowsAffected()
			if err != nil {
				return fmt.Errorf("count sessions revoked after credential reset: %w", err)
			}
		}

		consumed, err := tx.ExecContext(ctx, `
			UPDATE user_enrollment_tokens
			SET used_at = ?
			WHERE id = ? AND token_hash = ?
			  AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
			now, current.tokenID, hashToken(rawToken), now,
		)
		if err != nil {
			return fmt.Errorf("consume enrollment token: %w", err)
		}
		consumedCount, err := consumed.RowsAffected()
		if err != nil {
			return fmt.Errorf("count consumed enrollment tokens: %w", err)
		}
		if consumedCount != 1 {
			return ErrEnrollmentTokenInvalid
		}

		metadata, err := enrollmentRedemptionEventJSON(current.tokenID, current.purpose, result.RevokedSessions)
		if err != nil {
			return err
		}
		eventType := AuthEventEnrollmentCompleted
		if current.purpose == EnrollmentTokenPurposeCredentialReset {
			eventType = AuthEventCredentialResetCompleted
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, event_type,
				success, reason, user_agent, metadata_json
			) VALUES (?, ?, NULL, ?, ?, 1, ?, ?, ?)`,
			eventID, now, current.userID, eventType,
			AuthEventReasonChallengeConsumed, userAgent, metadata,
		); err != nil {
			return fmt.Errorf("record enrollment redemption: %w", err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrEnrollmentTokenInvalid) {
			return nil, ErrEnrollmentTokenInvalid
		}
		return nil, err
	}
	return result, nil
}

func (m *Manager) findEnrollmentRedemptionCandidate(ctx context.Context, rawToken string, now time.Time) (*enrollmentRedemptionCandidate, error) {
	if rawToken == "" {
		return nil, nil
	}
	candidate, err := scanEnrollmentRedemptionCandidate(m.db.Read().QueryRowContext(ctx, `
		SELECT t.id, t.user_id, t.purpose, u.status,
		       COALESCE(u.username, ''), u.email
		FROM user_enrollment_tokens t
		JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ?
		  AND t.used_at IS NULL AND t.revoked_at IS NULL AND t.expires_at > ?`,
		hashToken(rawToken), now,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find enrollment token: %w", err)
	}
	if !candidate.purpose.Valid() || !enrollmentRedemptionStatusEligible(candidate.purpose, candidate.status) {
		return nil, nil
	}
	return candidate, nil
}

func scanEnrollmentRedemptionCandidate(row rowScanner) (*enrollmentRedemptionCandidate, error) {
	candidate := &enrollmentRedemptionCandidate{}
	if err := row.Scan(
		&candidate.tokenID, &candidate.userID, &candidate.purpose,
		&candidate.status, &candidate.username, &candidate.email,
	); err != nil {
		return nil, err
	}
	return candidate, nil
}

func sameEnrollmentRedemptionCandidate(left, right *enrollmentRedemptionCandidate) bool {
	return left != nil && right != nil &&
		left.tokenID == right.tokenID && left.userID == right.userID &&
		left.purpose == right.purpose && left.status == right.status &&
		left.username == right.username && left.email == right.email
}

func enrollmentRedemptionStatusEligible(purpose EnrollmentTokenPurpose, status UserStatus) bool {
	return purpose == EnrollmentTokenPurposeEnrollment && status == UserStatusPending ||
		purpose == EnrollmentTokenPurposeCredentialReset && (status == UserStatusActive || status == UserStatusDisabled)
}

func enrollmentRedemptionEventJSON(tokenID string, purpose EnrollmentTokenPurpose, revokedSessions int64) (string, error) {
	metadata := struct {
		TokenID         string                 `json:"token_id"`
		Purpose         EnrollmentTokenPurpose `json:"purpose"`
		RevokedSessions int64                  `json:"revoked_sessions,omitempty"`
	}{TokenID: tokenID, Purpose: purpose, RevokedSessions: revokedSessions}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode enrollment redemption event metadata: %w", err)
	}
	return string(encoded), nil
}
