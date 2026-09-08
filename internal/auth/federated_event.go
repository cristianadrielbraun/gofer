package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (m *Manager) consumeFederatedLoginForEvent(ctx context.Context, token, nonce string) func(*sql.Tx, time.Time) error {
	return func(tx *sql.Tx, now time.Time) error {
		origin, err := canonicalAuthOrigin(m.config.BaseURL)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges
			SET attempts = attempts + 1, consumed_at = ?, payload_ciphertext = NULL
			WHERE challenge_hash = ? AND nonce_hash = ? AND purpose = ? AND origin = ?
			  AND user_id IS NULL AND session_id IS NULL AND consumed_at IS NULL
			  AND expires_at > ? AND attempts < max_attempts`,
			now, hashToken(token), hashToken(nonce), ChallengePurposeFederatedLogin, origin, now)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrPreAuthChallengeInvalid
		}
		return nil
	}
}

// Failed callback verification consumes the bound challenge with its event.
// A foreign/unknown bearer supplies no trustworthy actor, subject, or session.
func (m *Manager) rejectFederatedAuthorization(ctx context.Context, token string, purpose ChallengePurpose, method AuthenticationMethod, failure FederatedLoginFailureReason) error {
	origin, err := canonicalAuthOrigin(m.config.BaseURL)
	if err != nil {
		return federatedLoginError(FederatedLoginFailureInternal)
	}
	err = m.runSecurityTransition(ctx, SecurityTransitionIdentityChange, func(tx *sql.Tx) error {
		var user, session sql.NullString
		var consumed sql.NullTime
		err := tx.QueryRowContext(ctx, `
			SELECT user_id, session_id, consumed_at FROM auth_challenges
			WHERE challenge_hash = ? AND purpose = ? AND origin = ?`,
			hashToken(token), purpose, origin).Scan(&user, &session, &consumed)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		subject, sessionID := "", ""
		kind := AuthEventLoginFailed
		// A previously consumed challenge has already produced its transition event;
		// replays are anonymous failures, never another identity mutation event.
		if err == nil && !consumed.Valid {
			subject = user.String
			if purpose == ChallengePurposeFederatedLink {
				kind = AuthEventIdentityLinked
				sessionID = session.String
			} else if purpose == ChallengePurposeFederatedEnrollment {
				kind = AuthEventEnrollmentCompleted
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE auth_challenges
				SET attempts = CASE WHEN attempts < max_attempts THEN attempts + 1 ELSE attempts END,
				    consumed_at = ?, payload_ciphertext = NULL
				WHERE challenge_hash = ? AND purpose = ? AND origin = ? AND consumed_at IS NULL`,
				m.clock.Now().UTC(), hashToken(token), purpose, origin); err != nil {
				return err
			}
		}
		return m.appendTransitionEvent(ctx, tx, "", subject, sessionID, kind, false, AuthEventReasonInvalidCredentials, transitionEventMetadata{Method: method, Stage: "provider_callback"})
	})
	if err != nil {
		return federatedLoginError(FederatedLoginFailureInternal)
	}
	return federatedLoginError(failure)
}

// endFederatedAuthorization records browser cancellation/restart and handler
// rejection before provider verification. Already consumed flows are no-ops:
// their completion or verification failure has its own event.
func (m *Manager) endFederatedAuthorization(ctx context.Context, token string, purpose ChallengePurpose, method AuthenticationMethod) error {
	origin, err := canonicalAuthOrigin(m.config.BaseURL)
	if err != nil {
		return ErrPreAuthChallengeInvalid
	}
	return m.runSecurityTransition(ctx, SecurityTransitionIdentityChange, func(tx *sql.Tx) error {
		var user, session sql.NullString
		err := tx.QueryRowContext(ctx, `
			UPDATE auth_challenges
			SET attempts = CASE WHEN attempts < max_attempts THEN attempts + 1 ELSE attempts END,
			    consumed_at = ?, payload_ciphertext = NULL
			WHERE challenge_hash = ? AND purpose = ? AND origin = ? AND consumed_at IS NULL
			RETURNING user_id, session_id`,
			m.clock.Now().UTC(), hashToken(token), purpose, origin).Scan(&user, &session)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		kind := AuthEventLoginFailed
		if purpose == ChallengePurposeFederatedLink {
			kind = AuthEventIdentityLinked
		}
		if purpose == ChallengePurposeFederatedEnrollment {
			kind = AuthEventEnrollmentCompleted
		}
		return m.appendTransitionEvent(ctx, tx, "", user.String, session.String, kind, false, AuthEventReasonUserAction, transitionEventMetadata{Method: method, Stage: "authorization_ended"})
	})
}
