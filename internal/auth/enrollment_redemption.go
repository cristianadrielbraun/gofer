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
	Purpose     EnrollmentTokenPurpose
	// Administrator accepts an administrator's invitation, and nothing
	// else; without it, an administrator's invitation isn't accepted.
	Administrator bool
}

type EnrollmentRedemptionResult struct {
	UserID          string
	Purpose         EnrollmentTokenPurpose
	RevokedSessions int64
	// FactorsReset is an administrator's sign-in reset by another
	// administrator: their authenticator app, recovery codes and passkeys
	// were retired, and they set up two-factor authentication again.
	FactorsReset bool
}

type enrollmentRedemptionCandidate struct {
	tokenID  string
	userID   string
	purpose  EnrollmentTokenPurpose
	status   UserStatus
	username string
	userType UserType
	// issuedBy is the administrator who issued the token; empty for a
	// token from the command line.
	issuedBy string
}

// RedeemEnrollmentToken establishes the subject's password credential and
// consumes the supplied enrollment or credential-reset token in one security
// transition. Credential reset also invalidates the user's authentication
// version and revokes every existing session. It intentionally does not create
// a new session: the user must authenticate with the newly established secret.
func (m *Manager) RedeemEnrollmentToken(ctx context.Context, options RedeemEnrollmentTokenOptions) (*EnrollmentRedemptionResult, error) {
	if !options.Purpose.Valid() {
		return nil, fmt.Errorf("invalid enrollment token purpose %q", options.Purpose)
	}
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
	if candidate != nil && !enrollmentRedemptionMatches(candidate, options) {
		candidate = nil
	}
	if candidate != nil {
		preparedPassword, err = PrepareNewPassword(options.NewPassword, PasswordPolicyContext{
			Username: candidate.username,
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
			       u.username, u.user_type, COALESCE(t.created_by, '')
			FROM user_enrollment_tokens t
			JOIN users u ON u.id = t.user_id
			WHERE t.id = ? AND t.token_hash = ?
			  AND t.used_at IS NULL AND t.revoked_at IS NULL AND t.expires_at > ?
			  AND u.deletion_pending = 0`,
			candidate.tokenID, hashToken(rawToken), now,
		))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEnrollmentTokenInvalid
		}
		if err != nil {
			return fmt.Errorf("recheck enrollment token: %w", err)
		}
		if !sameEnrollmentRedemptionCandidate(current, candidate) ||
			!enrollmentRedemptionMatches(current, options) ||
			!enrollmentRedemptionStatusEligible(current.purpose, current.status, current.userType) {
			return ErrEnrollmentTokenInvalid
		}
		// Whoever two-factor sign-in is required of sets up the authenticator
		// app at their first sign-in, administrators and everyone else alike.

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
				SET auth_version = auth_version + 1,
				    password_reset_requested_at = NULL,
				    updated_at = ?
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
			if current.resetsFactors() {
				if err := retireSignInFactors(ctx, tx, current.userID, now); err != nil {
					return err
				}
				result.FactorsReset = true
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

		metadata, err := enrollmentRedemptionEventJSON(current.tokenID, current.purpose, result.RevokedSessions, result.FactorsReset)
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
		       u.username, u.user_type, COALESCE(t.created_by, '')
		FROM user_enrollment_tokens t
		JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ?
		  AND t.used_at IS NULL AND t.revoked_at IS NULL AND t.expires_at > ?
		  AND u.deletion_pending = 0`,
		hashToken(rawToken), now,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find enrollment token: %w", err)
	}
	if !candidate.purpose.Valid() ||
		!enrollmentRedemptionStatusEligible(candidate.purpose, candidate.status, candidate.userType) {
		return nil, nil
	}
	return candidate, nil
}

func scanEnrollmentRedemptionCandidate(row rowScanner) (*enrollmentRedemptionCandidate, error) {
	candidate := &enrollmentRedemptionCandidate{}
	if err := row.Scan(
		&candidate.tokenID, &candidate.userID, &candidate.purpose,
		&candidate.status, &candidate.username, &candidate.userType, &candidate.issuedBy,
	); err != nil {
		return nil, err
	}
	return candidate, nil
}

func sameEnrollmentRedemptionCandidate(left, right *enrollmentRedemptionCandidate) bool {
	return left != nil && right != nil &&
		left.tokenID == right.tokenID && left.userID == right.userID &&
		left.purpose == right.purpose && left.status == right.status &&
		left.username == right.username && left.userType == right.userType &&
		left.issuedBy == right.issuedBy
}

// resetsFactors is a reset of an administrator's sign-in that another
// administrator issued from the console. The command line's recovery keeps
// the factors: recovery codes stay the way back from a lost authenticator.
func (candidate *enrollmentRedemptionCandidate) resetsFactors() bool {
	return candidate.purpose == EnrollmentTokenPurposeCredentialReset &&
		candidate.userType == UserTypeManagement && candidate.issuedBy != ""
}

func enrollmentRedemptionStatusEligible(purpose EnrollmentTokenPurpose, status UserStatus, userType UserType) bool {
	return purpose == EnrollmentTokenPurposeEnrollment && status == UserStatusPending && (userType == UserTypeWebmail || userType == UserTypeManagement) ||
		purpose == EnrollmentTokenPurposeCredentialReset && (status == UserStatusActive || status == UserStatusDisabled)
}

// enrollmentRedemptionMatches reports whether the page asking accepts
// candidate: the webmail pages a webmail person's invitation and every
// credential reset (the command line's recovery of an administrator
// included), the administrator pages only an administrator's invitation or
// reset.
func enrollmentRedemptionMatches(candidate *enrollmentRedemptionCandidate, options RedeemEnrollmentTokenOptions) bool {
	if candidate.purpose != options.Purpose {
		return false
	}
	if options.Administrator {
		return candidate.userType == UserTypeManagement
	}
	return candidate.purpose != EnrollmentTokenPurposeEnrollment || candidate.userType == UserTypeWebmail
}

// retireSignInFactors ends someone's authenticator app, unused recovery
// codes and passkeys.
func retireSignInFactors(ctx context.Context, tx *sql.Tx, userID string, now time.Time) error {
	for _, statement := range []string{
		`UPDATE totp_credentials SET enabled = 0, revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`,
		`UPDATE recovery_codes SET revoked_at = ? WHERE user_id = ? AND used_at IS NULL AND revoked_at IS NULL`,
		`UPDATE webauthn_credentials SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`,
	} {
		if _, err := tx.ExecContext(ctx, statement, now, userID); err != nil {
			return fmt.Errorf("retire sign-in factors: %w", err)
		}
	}
	return nil
}

func enrollmentRedemptionEventJSON(tokenID string, purpose EnrollmentTokenPurpose, revokedSessions int64, factorsReset bool) (string, error) {
	metadata := struct {
		TokenID         string                 `json:"token_id"`
		Purpose         EnrollmentTokenPurpose `json:"purpose"`
		RevokedSessions int64                  `json:"revoked_sessions,omitempty"`
		FactorsReset    bool                   `json:"factors_reset,omitempty"`
	}{TokenID: tokenID, Purpose: purpose, RevokedSessions: revokedSessions, FactorsReset: factorsReset}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode enrollment redemption event metadata: %w", err)
	}
	return string(encoded), nil
}
