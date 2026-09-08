package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrTOTPLoginChallengeInvalid = errors.New("TOTP login challenge is invalid")

type TOTPLoginValidationError struct {
	Terminal bool
}

func (err *TOTPLoginValidationError) Error() string {
	return "authenticator code is invalid"
}

type TOTPLoginOptions struct {
	Token     string
	Code      string
	Origin    string
	Source    string
	UserAgent string
}

// CompleteTOTPLogin finishes a password or federated primary MFA challenge. The accepted
// time step, challenge consumption, full session, login timestamp, throttle
// reset, and redacted audit event are committed as one security transition.
func (m *Manager) CompleteTOTPLogin(ctx context.Context, options TOTPLoginOptions) (*Session, error) {
	token := strings.TrimSpace(options.Token)
	canonicalOrigin, err := canonicalAuthOrigin(options.Origin)
	if err != nil || token == "" {
		return nil, ErrTOTPLoginChallengeInvalid
	}
	now := m.clock.Now().UTC()
	preflight, primary, err := m.readMFAContinuation(ctx, token, canonicalOrigin)
	if err != nil || preflight == nil || strings.TrimSpace(preflight.UserID) == "" || preflight.SessionID != "" {
		return nil, ErrTOTPLoginChallengeInvalid
	}

	buckets, err := m.totpLoginThrottleBuckets(preflight.UserID, options.Source)
	if err != nil {
		return nil, err
	}
	decision, err := m.checkLoginThrottleBuckets(ctx, buckets)
	if err != nil {
		return nil, fmt.Errorf("check TOTP login throttle: %w", err)
	}
	if decision.Throttled {
		return nil, m.rejectThrottledAuthentication(ctx, decision, "", preflight.UserID, "", AuthEventLoginFailed, AuthenticationMethodTOTP)
	}

	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate TOTP login event ID: %w", err)
	}
	sourceHash, err := m.authenticationThrottleBucketHash(
		totpLoginThrottleAction, loginThrottleBucketSource, strings.TrimSpace(options.Source),
	)
	if err != nil {
		return nil, err
	}
	userAgent := boundedUserAgent(options.UserAgent)
	eventMetadata, err := mfaFactorEventMetadata("totp", primary.PrimaryMethod, false)
	if err != nil {
		return nil, err
	}

	var session *Session
	var sessionID, sessionToken string
	invalid := false
	terminal := false
	retryAt := time.Time{}
	err = m.runSecurityTransition(ctx, SecurityTransitionLoginCompletion, func(tx *sql.Tx) error {
		currentChallenge, currentPrimary, err := m.currentMFAContinuation(ctx, tx, token, canonicalOrigin, now)
		if err != nil || currentChallenge.ID != preflight.ID || !sameMFAContinuationDraft(currentPrimary, primary) {
			return ErrTOTPLoginChallengeInvalid
		}
		var credentialID, algorithm string
		var keyVersion, digits, period int
		var encryptedSeed []byte
		var authVersion int64
		var lastAcceptedStep sql.NullInt64
		err = tx.QueryRowContext(ctx, `
			SELECT u.auth_version, t.id, t.encrypted_seed, t.key_version, t.algorithm, t.digits,
			       t.period, t.last_accepted_step
			FROM users u
			JOIN totp_credentials t ON t.user_id = u.id
			WHERE u.id = ? AND u.status = 'active'
			  AND t.enabled = 1 AND t.revoked_at IS NULL`,
			currentChallenge.UserID,
		).Scan(
			&authVersion, &credentialID, &encryptedSeed, &keyVersion, &algorithm, &digits,
			&period, &lastAcceptedStep,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTOTPLoginChallengeInvalid
		}
		if err != nil {
			return fmt.Errorf("read TOTP login state: %w", err)
		}
		userID := currentChallenge.UserID
		policy, err := queryAuthenticationPolicy(ctx, tx, userID, authVersion)
		if err != nil {
			return ErrTOTPLoginChallengeInvalid
		}
		if userID != preflight.UserID || authVersion != primary.AuthVersion || !policy.RequiresMFA {
			return ErrTOTPLoginChallengeInvalid
		}
		if algorithm != totpAlgorithm || digits != totpDigits || period != totpPeriodSeconds {
			return fmt.Errorf("unsupported TOTP credential profile")
		}
		seed, err := m.decryptTOTPSeed(userID, credentialID, encryptedSeed, keyVersion)
		if err != nil {
			return err
		}
		matchedStep, valid, err := matchTOTPCode(seed, options.Code, now)
		if err != nil {
			return err
		}
		if lastAcceptedStep.Valid && matchedStep <= lastAcceptedStep.Int64 {
			valid = false
		}

		if !valid {
			invalid = true
			var consumedAt sql.NullTime
			if err := tx.QueryRowContext(ctx, `
				UPDATE auth_challenges
				SET attempts = attempts + 1,
				    consumed_at = CASE WHEN attempts + 1 >= max_attempts THEN ? ELSE NULL END,
				    payload_ciphertext = CASE WHEN attempts + 1 >= max_attempts THEN NULL ELSE payload_ciphertext END
				WHERE id = ? AND challenge_hash = ? AND purpose = ? AND origin = ?
				  AND consumed_at IS NULL AND expires_at > ? AND attempts < max_attempts
				RETURNING consumed_at`,
				now, currentChallenge.ID, hashToken(token), ChallengePurposeMFA, canonicalOrigin, now,
			).Scan(&consumedAt); errors.Is(err, sql.ErrNoRows) {
				return ErrTOTPLoginChallengeInvalid
			} else if err != nil {
				return fmt.Errorf("record rejected TOTP login code: %w", err)
			}
			terminal = consumedAt.Valid
			for _, bucket := range buckets {
				blockedUntil, err := recordLoginThrottleFailure(ctx, tx, bucket, now)
				if err != nil {
					return err
				}
				if blockedUntil.After(retryAt) {
					retryAt = blockedUntil
				}
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO auth_events (
					id, occurred_at, actor_user_id, subject_user_id, event_type,
					success, reason, user_agent, source_hash, metadata_json
				) VALUES (?, ?, NULL, ?, ?, 0, ?, ?, ?, ?)`,
				eventID, now, userID, AuthEventLoginFailed, AuthEventReasonInvalidCredentials,
				userAgent, sourceHash, eventMetadata,
			); err != nil {
				return fmt.Errorf("record rejected TOTP login event: %w", err)
			}
			return nil
		}
		sessionID, err = m.tokens.ID()
		if err != nil {
			return fmt.Errorf("generate TOTP login session ID: %w", err)
		}
		sessionToken, err = m.tokens.Token(32)
		if err != nil {
			return fmt.Errorf("generate TOTP login session token: %w", err)
		}

		credentialUpdate, err := tx.ExecContext(ctx, `
			UPDATE totp_credentials
			SET last_accepted_step = ?, last_used_at = ?
			WHERE id = ? AND user_id = ? AND enabled = 1 AND revoked_at IS NULL
			  AND (last_accepted_step IS NULL OR last_accepted_step < ?)`,
			matchedStep, now, credentialID, userID, matchedStep,
		)
		if err != nil {
			return fmt.Errorf("advance TOTP login replay step: %w", err)
		}
		changed, err := credentialUpdate.RowsAffected()
		if err != nil {
			return fmt.Errorf("count advanced TOTP login replay steps: %w", err)
		}
		if changed != 1 {
			return ErrTOTPLoginChallengeInvalid
		}

		challengeUpdate, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET attempts = attempts + 1, consumed_at = ?, payload_ciphertext = NULL
			WHERE id = ? AND challenge_hash = ? AND purpose = ? AND origin = ?
			  AND user_id = ? AND session_id IS NULL AND consumed_at IS NULL
			  AND expires_at > ? AND attempts < max_attempts`,
			now, currentChallenge.ID, hashToken(token), ChallengePurposeMFA, canonicalOrigin, userID, now,
		)
		if err != nil {
			return fmt.Errorf("consume TOTP login challenge: %w", err)
		}
		changed, err = challengeUpdate.RowsAffected()
		if err != nil {
			return fmt.Errorf("count consumed TOTP login challenges: %w", err)
		}
		if changed != 1 {
			return ErrTOTPLoginChallengeInvalid
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET consumed_at = COALESCE(consumed_at, ?), payload_ciphertext = NULL
			WHERE user_id = ? AND purpose = ? AND id != ? AND consumed_at IS NULL`,
			now, userID, ChallengePurposeMFA, currentChallenge.ID,
		); err != nil {
			return fmt.Errorf("terminate parallel TOTP login challenges: %w", err)
		}

		idleExpiresAt := now.Add(sessionIdleLifetime)
		absoluteExpiresAt := now.Add(sessionAbsoluteLifetime)
		if idleExpiresAt.After(absoluteExpiresAt) {
			idleExpiresAt = absoluteExpiresAt
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (
				id, user_id, token_hash, auth_version, authentication_method,
				assurance_level, user_agent, authenticated_at, last_used_at,
				idle_expires_at, absolute_expires_at, step_up_at, step_up_method,
				created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, userID, hashToken(sessionToken), authVersion,
			primary.PrimaryMethod, AssuranceLevelMultiFactor, userAgent,
			now, now, idleExpiresAt, absoluteExpiresAt, now, AuthenticationMethodTOTP, now,
		); err != nil {
			return fmt.Errorf("insert TOTP login session: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE users SET last_login_at = ?, updated_at = ?
			WHERE id = ? AND status = 'active' AND auth_version = ?`,
			now, now, userID, authVersion,
		); err != nil {
			return fmt.Errorf("update TOTP login timestamp: %w", err)
		}
		identifierHash, err := m.authenticationThrottleBucketHash(
			totpLoginThrottleAction, loginThrottleBucketIdentifier, userID,
		)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM auth_throttle WHERE bucket_hash = ? AND action = ?`,
			identifierHash, totpLoginThrottleAction,
		); err != nil {
			return fmt.Errorf("clear successful TOTP login throttle bucket: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, source_hash, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`,
			eventID, now, userID, userID, sessionID, AuthEventLoginSucceeded,
			AuthEventReasonChallengeVerified, userAgent, sourceHash, eventMetadata,
		); err != nil {
			return fmt.Errorf("record successful TOTP login event: %w", err)
		}

		stepUpAt := now
		session = &Session{
			ID: sessionID, UserID: userID, Token: sessionToken, AuthVersion: authVersion,
			AuthenticationMethod: primary.PrimaryMethod,
			AssuranceLevel:       AssuranceLevelMultiFactor,
			UserAgent:            userAgent,
			AuthenticatedAt:      now,
			LastUsedAt:           now,
			IdleExpiresAt:        idleExpiresAt,
			AbsoluteExpiresAt:    absoluteExpiresAt,
			StepUpAt:             &stepUpAt,
			StepUpMethod:         AuthenticationMethodTOTP,
			CreatedAt:            now,
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrTOTPLoginChallengeInvalid) {
			return nil, ErrTOTPLoginChallengeInvalid
		}
		return nil, err
	}
	if invalid {
		if retryAt.After(now) {
			return nil, m.loginThrottleError(newLoginThrottleDecision(now, retryAt))
		}
		return nil, &TOTPLoginValidationError{Terminal: terminal}
	}
	return session, nil
}
