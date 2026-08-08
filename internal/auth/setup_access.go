package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	maximumSetupTokenAttempts    int64 = 10
	setupAccessChallengeLifetime       = 10 * time.Minute
)

var ErrSetupTokenInvalid = errors.New("setup token is invalid or no longer active")

type BeginSetupOptions struct {
	Token     string
	Origin    string
	UserAgent string
}

// BeginSetup verifies the instance setup token and exchanges it for a
// short-lived, origin-bound, hash-only pre-authentication challenge. The setup
// token remains active until owner enrollment commits; successful verification
// is not setup completion.
func (m *Manager) BeginSetup(ctx context.Context, options BeginSetupOptions) (*PreAuthChallenge, error) {
	origin, err := canonicalAuthOrigin(options.Origin)
	if err != nil {
		return nil, err
	}
	now := m.clock.Now().UTC()
	challengeID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate setup access challenge ID: %w", err)
	}
	challengeToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate setup access challenge token: %w", err)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate setup verification event ID: %w", err)
	}
	challenge := &PreAuthChallenge{
		ID:          challengeID,
		Token:       challengeToken,
		Purpose:     ChallengePurposeEnrollment,
		Origin:      origin,
		MaxAttempts: defaultPreAuthMaxAttempts,
		CreatedAt:   now,
		ExpiresAt:   now.Add(setupAccessChallengeLifetime),
	}

	invalid := false
	alreadyInitialized := false
	err = m.runSecurityTransition(ctx, SecurityTransitionSetup, func(tx *sql.Tx) error {
		var initialized int
		var storedHash sql.NullString
		var expiresAt sql.NullTime
		var attempts int64
		err := tx.QueryRowContext(ctx, `
			SELECT initialized, setup_token_hash, setup_expires_at, setup_attempts
			FROM auth_system_state WHERE id = 1`,
		).Scan(&initialized, &storedHash, &expiresAt, &attempts)
		if errors.Is(err, sql.ErrNoRows) {
			invalid = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("read setup token verification state: %w", err)
		}
		if initialized == 1 {
			alreadyInitialized = true
			return nil
		}
		if !storedHash.Valid || storedHash.String == "" || !expiresAt.Valid || !expiresAt.Time.After(now) || attempts >= maximumSetupTokenAttempts {
			invalid = true
			return nil
		}

		submittedHash := hashToken(options.Token)
		if subtle.ConstantTimeCompare([]byte(storedHash.String), []byte(submittedHash)) != 1 {
			nextAttempts := attempts + 1
			if nextAttempts > maximumSetupTokenAttempts {
				nextAttempts = maximumSetupTokenAttempts
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE auth_system_state SET setup_attempts = ?
				WHERE id = 1 AND initialized = 0`, nextAttempts); err != nil {
				return fmt.Errorf("record rejected setup token attempt: %w", err)
			}
			metadata, err := setupVerificationEventJSON(nextAttempts, nextAttempts >= maximumSetupTokenAttempts, "")
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO auth_events (
					id, occurred_at, event_type, success, reason, user_agent, metadata_json
				) VALUES (?, ?, ?, 0, ?, ?, ?)`,
				eventID, now, AuthEventSetupTokenVerificationFailed,
				AuthEventReasonInvalidCredentials, boundedUserAgent(options.UserAgent), metadata,
			); err != nil {
				return fmt.Errorf("record rejected setup token event: %w", err)
			}
			invalid = true
			return nil
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_system_state SET setup_attempts = 0
			WHERE id = 1 AND initialized = 0`); err != nil {
			return fmt.Errorf("reset setup token attempts: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET consumed_at = ?
			WHERE purpose = ? AND user_id IS NULL AND session_id IS NULL
			  AND consumed_at IS NULL`, now, ChallengePurposeEnrollment); err != nil {
			return fmt.Errorf("terminate replaced setup access challenge: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_challenges (
				id, user_id, session_id, challenge_hash, nonce_hash, purpose, origin,
				attempts, max_attempts, created_at, expires_at
			) VALUES (?, NULL, NULL, ?, NULL, ?, ?, 0, ?, ?, ?)`,
			challenge.ID, hashToken(challenge.Token), challenge.Purpose, challenge.Origin,
			challenge.MaxAttempts, challenge.CreatedAt, challenge.ExpiresAt,
		); err != nil {
			return fmt.Errorf("insert setup access challenge: %w", err)
		}
		metadata, err := setupVerificationEventJSON(0, false, challenge.ID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, event_type, success, reason, user_agent, metadata_json
			) VALUES (?, ?, ?, 1, ?, ?, ?)`,
			eventID, now, AuthEventSetupTokenVerified,
			AuthEventReasonChallengeVerified, boundedUserAgent(options.UserAgent), metadata,
		); err != nil {
			return fmt.Errorf("record setup token verification: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if alreadyInitialized {
		return nil, ErrSetupAlreadyInitialized
	}
	if invalid {
		return nil, ErrSetupTokenInvalid
	}
	return challenge, nil
}

// GetActiveSetupAccess returns only a userless, sessionless enrollment
// challenge while the instance remains uninitialized. It never returns the
// setup-token hash or advances either attempt counter.
func (m *Manager) GetActiveSetupAccess(ctx context.Context, token, origin string) (*PreAuthChallenge, error) {
	if token == "" {
		return nil, nil
	}
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil {
		return nil, nil
	}
	now := m.clock.Now().UTC()
	challenge, err := scanPreAuthChallenge(m.db.Read().QueryRowContext(ctx, `
		SELECT challenge.id, '', '', challenge.purpose, challenge.origin,
		       challenge.attempts, challenge.max_attempts, challenge.payload_ciphertext,
		       challenge.created_at, challenge.expires_at, challenge.consumed_at
		FROM auth_challenges challenge
		JOIN auth_system_state setup ON setup.id = 1
		WHERE challenge.challenge_hash = ? AND challenge.purpose = ? AND challenge.origin = ?
		  AND challenge.user_id IS NULL AND challenge.session_id IS NULL
		  AND challenge.consumed_at IS NULL AND challenge.expires_at > ?
		  AND challenge.attempts < challenge.max_attempts
		  AND setup.initialized = 0 AND setup.setup_token_hash IS NOT NULL`,
		hashToken(token), ChallengePurposeEnrollment, canonicalOrigin, now,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read active setup access challenge: %w", err)
	}
	return challenge, nil
}

func setupVerificationEventJSON(attempts int64, blocked bool, challengeID string) (string, error) {
	metadata := struct {
		Attempts    int64  `json:"attempts"`
		Blocked     bool   `json:"blocked"`
		ChallengeID string `json:"challenge_id,omitempty"`
	}{Attempts: attempts, Blocked: blocked, ChallengeID: challengeID}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode setup verification event metadata: %w", err)
	}
	return string(encoded), nil
}
