package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	recoveryRepairDraftVersion    = 1
	recoveryRepairDraftKeyContext = "gofer/auth/recovery-repair-draft/v1"
	recoveryRepairLifetime        = 15 * time.Minute
	recoveryRepairMaxAttempts     = 3
)

var (
	ErrRecoveryCodeLoginChallengeInvalid = errors.New("recovery-code login challenge is invalid")
	ErrRecoveryRepairInvalid             = errors.New("recovery repair challenge is invalid")
	ErrRecoveryRepairTOTPRequired        = errors.New("a verified recovery-repair TOTP draft is required")
	ErrRecoveryRepairBatchRequired       = errors.New("a recovery-repair code batch is required")
	ErrRecoveryRepairBatchExists         = errors.New("a recovery-repair code batch already exists")
	ErrRecoveryRepairStateChanged        = errors.New("recovery repair state changed")
)

type RecoveryCodeLoginValidationError struct {
	Terminal bool
}

func (*RecoveryCodeLoginValidationError) Error() string {
	return "recovery code is invalid"
}

type RecoveryCodeLoginOptions struct {
	Token     string
	Code      string
	Origin    string
	Source    string
	UserAgent string
}

type RecoveryRepairDraft struct {
	AuthVersion        int64                `json:"auth_version"`
	PrimaryMethod      AuthenticationMethod `json:"primary_method"`
	PrimaryAssurance   AssuranceLevel       `json:"primary_assurance"`
	OriginalTOTP       string               `json:"original_totp"`
	UsedRecoveryCode   string               `json:"used_recovery_code"`
	TOTPSecret         string               `json:"totp_secret"`
	TOTPConfirmedStep  *int64               `json:"totp_confirmed_step,omitempty"`
	RecoveryBatchID    string               `json:"recovery_batch_id,omitempty"`
	RecoveryCodeHashes []string             `json:"recovery_code_hashes,omitempty"`
}

type RecoveryRepairState struct {
	UserID            string
	Enrollment        *SetupTOTPEnrollment
	TOTPConfirmed     bool
	RecoveryGenerated bool
	RecoveryBatchID   string
}

type RecoveryRepairBatch struct {
	RecoveryRepairState
	Codes []string
}

type RecoveryRepairTOTPValidationError struct {
	Terminal bool
}

func (*RecoveryRepairTOTPValidationError) Error() string {
	return "replacement authenticator code is invalid"
}

type CompleteRecoveryRepairOptions struct {
	Token     string
	Origin    string
	BatchID   string
	Saved     bool
	Source    string
	UserAgent string
}

// StartRecoveryCodeRepair atomically consumes one recovery code and rotates
// the primary MFA challenge into a restricted recovery challenge. It never
// creates a full session or changes the active authenticator.
func (m *Manager) StartRecoveryCodeRepair(ctx context.Context, options RecoveryCodeLoginOptions) (*PreAuthChallenge, error) {
	token := strings.TrimSpace(options.Token)
	canonicalOrigin, err := canonicalAuthOrigin(options.Origin)
	if err != nil || token == "" {
		return nil, ErrRecoveryCodeLoginChallengeInvalid
	}
	now := m.clock.Now().UTC()
	preflight, primary, err := m.readMFAContinuation(ctx, token, canonicalOrigin)
	if err != nil || preflight == nil || strings.TrimSpace(preflight.UserID) == "" || preflight.SessionID != "" {
		return nil, ErrRecoveryCodeLoginChallengeInvalid
	}

	buckets, err := m.recoveryCodeThrottleBuckets(preflight.UserID, options.Source)
	if err != nil {
		return nil, err
	}
	decision, err := m.checkLoginThrottleBuckets(ctx, buckets)
	if err != nil {
		return nil, fmt.Errorf("check recovery-code login throttle: %w", err)
	}
	if decision.Throttled {
		return nil, m.rejectThrottledAuthentication(ctx, decision, "", preflight.UserID, "", AuthEventLoginFailed, AuthenticationMethodRecoveryCode)
	}

	canonicalCode, validShape := canonicalRecoveryCode(options.Code)
	codeHash := hashToken(canonicalCode)
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate recovery-code event ID: %w", err)
	}
	sourceHash, err := m.authenticationThrottleBucketHash(
		recoveryCodeThrottleAction, loginThrottleBucketSource, strings.TrimSpace(options.Source),
	)
	if err != nil {
		return nil, err
	}
	userAgent := boundedUserAgent(options.UserAgent)
	rejectedMetadata, err := mfaFactorEventMetadata("recovery_code", primary.PrimaryMethod, false)
	if err != nil {
		return nil, err
	}
	acceptedMetadata, err := mfaFactorEventMetadata("recovery_code", primary.PrimaryMethod, true)
	if err != nil {
		return nil, err
	}

	var repair *PreAuthChallenge
	invalid := false
	terminal := false
	retryAt := time.Time{}
	err = m.runSecurityTransition(ctx, SecurityTransitionRecovery, func(tx *sql.Tx) error {
		currentChallenge, currentPrimary, err := m.currentMFAContinuation(ctx, tx, token, canonicalOrigin, now)
		if err != nil || currentChallenge.ID != preflight.ID || !sameMFAContinuationDraft(currentPrimary, primary) {
			return ErrRecoveryCodeLoginChallengeInvalid
		}
		var usernameNormalized, totpID string
		var authVersion int64
		err = tx.QueryRowContext(ctx, `
			SELECT u.auth_version,
			       u.username_normalized,
			       t.id
			FROM users u
			JOIN totp_credentials t ON t.user_id = u.id
			WHERE u.id = ? AND u.status = 'active'
			  AND t.enabled = 1 AND t.revoked_at IS NULL`,
			currentChallenge.UserID,
		).Scan(&authVersion, &usernameNormalized, &totpID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRecoveryCodeLoginChallengeInvalid
		}
		if err != nil {
			return fmt.Errorf("read recovery-code login state: %w", err)
		}
		userID := currentChallenge.UserID
		policy, err := queryAuthenticationPolicy(ctx, tx, userID, authVersion)
		if err != nil {
			return ErrRecoveryCodeLoginChallengeInvalid
		}
		if userID != preflight.UserID || authVersion != primary.AuthVersion || !policy.RequiresMFA {
			return ErrRecoveryCodeLoginChallengeInvalid
		}

		var recoveryCodeID string
		if validShape {
			err = tx.QueryRowContext(ctx, `
				UPDATE recovery_codes SET used_at = ?
				WHERE user_id = ? AND code_hash = ?
				  AND used_at IS NULL AND revoked_at IS NULL
				RETURNING id`,
				now, userID, codeHash,
			).Scan(&recoveryCodeID)
		} else {
			err = sql.ErrNoRows
		}
		if errors.Is(err, sql.ErrNoRows) {
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
				return ErrRecoveryCodeLoginChallengeInvalid
			} else if err != nil {
				return fmt.Errorf("record rejected recovery code: %w", err)
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
				userAgent, sourceHash, rejectedMetadata,
			); err != nil {
				return fmt.Errorf("record rejected recovery-code event: %w", err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("consume recovery code: %w", err)
		}

		repairID, err := m.tokens.ID()
		if err != nil {
			return fmt.Errorf("generate recovery-repair challenge ID: %w", err)
		}
		repairToken, err := m.tokens.Token(32)
		if err != nil {
			return fmt.Errorf("generate recovery-repair challenge token: %w", err)
		}
		seedMaterial, err := m.tokens.Token(32)
		if err != nil {
			return fmt.Errorf("generate recovery-repair TOTP material: %w", err)
		}
		key, err := newTOTPKey(usernameNormalized, seedMaterial)
		if err != nil {
			return err
		}
		repair = &PreAuthChallenge{
			ID: repairID, Token: repairToken, UserID: userID,
			Purpose: ChallengePurposeRecovery, Origin: canonicalOrigin,
			MaxAttempts: recoveryRepairMaxAttempts, CreatedAt: now,
			ExpiresAt: now.Add(recoveryRepairLifetime),
		}
		draft := &RecoveryRepairDraft{
			AuthVersion: authVersion, PrimaryMethod: primary.PrimaryMethod,
			PrimaryAssurance: primary.PrimaryAssurance, OriginalTOTP: totpID,
			UsedRecoveryCode: recoveryCodeID, TOTPSecret: key.Secret(),
		}
		payload, err := m.encryptRecoveryRepairDraft(repair, draft)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET consumed_at = COALESCE(consumed_at, ?), payload_ciphertext = NULL
			WHERE user_id = ? AND purpose = ? AND consumed_at IS NULL`,
			now, userID, ChallengePurposeMFA,
		); err != nil {
			return fmt.Errorf("terminate recovery-replaced MFA challenges: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET consumed_at = COALESCE(consumed_at, ?), payload_ciphertext = NULL
			WHERE user_id = ? AND purpose = ? AND consumed_at IS NULL`,
			now, userID, ChallengePurposeRecovery,
		); err != nil {
			return fmt.Errorf("terminate replaced recovery-repair challenges: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_challenges (
				id, user_id, session_id, challenge_hash, nonce_hash, purpose,
				origin, attempts, max_attempts, payload_ciphertext, created_at, expires_at
			) VALUES (?, ?, NULL, ?, NULL, ?, ?, 0, ?, ?, ?, ?)`,
			repair.ID, repair.UserID, hashToken(repair.Token), repair.Purpose,
			repair.Origin, repair.MaxAttempts, payload, repair.CreatedAt, repair.ExpiresAt,
		); err != nil {
			return fmt.Errorf("insert recovery-repair challenge: %w", err)
		}
		identifierHash, err := m.authenticationThrottleBucketHash(
			recoveryCodeThrottleAction, loginThrottleBucketIdentifier, userID,
		)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM auth_throttle WHERE bucket_hash = ? AND action = ?`,
			identifierHash, recoveryCodeThrottleAction,
		); err != nil {
			return fmt.Errorf("clear successful recovery-code throttle: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, event_type,
				success, reason, user_agent, source_hash, metadata_json
			) VALUES (?, ?, NULL, ?, ?, 1, ?, ?, ?, ?)`,
			eventID, now, userID, AuthEventRecoveryUsed, AuthEventReasonPolicyRequired,
			userAgent, sourceHash, acceptedMetadata,
		); err != nil {
			return fmt.Errorf("record recovery-code use event: %w", err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrRecoveryCodeLoginChallengeInvalid) {
			return nil, ErrRecoveryCodeLoginChallengeInvalid
		}
		return nil, err
	}
	if invalid {
		if retryAt.After(now) {
			return nil, m.loginThrottleError(newLoginThrottleDecision(now, retryAt))
		}
		return nil, &RecoveryCodeLoginValidationError{Terminal: terminal}
	}
	return repair, nil
}

func (m *Manager) GetRecoveryRepairState(ctx context.Context, token, origin string) (*RecoveryRepairState, error) {
	challenge, draft, email, err := m.readRecoveryRepairDraft(ctx, token, origin)
	if err != nil {
		return nil, err
	}
	enrollment, err := setupTOTPEnrollment(email, draft.TOTPSecret, draft.TOTPConfirmedStep != nil)
	if err != nil {
		return nil, err
	}
	return recoveryRepairState(challenge, draft, enrollment), nil
}

func (m *Manager) RestartRecoveryRepairTOTP(ctx context.Context, token, origin string) (*RecoveryRepairState, error) {
	material, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate replacement recovery-repair TOTP material: %w", err)
	}
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, ErrRecoveryRepairInvalid
	}
	now := m.clock.Now().UTC()
	var state *RecoveryRepairState
	err = m.runSecurityTransition(ctx, SecurityTransitionRecovery, func(tx *sql.Tx) error {
		challenge, draft, email, _, err := m.currentRecoveryRepairDraft(ctx, tx, token, canonicalOrigin, now)
		if err != nil {
			return err
		}
		key, err := newTOTPKey(email, material)
		if err != nil {
			return err
		}
		draft.TOTPSecret = key.Secret()
		draft.TOTPConfirmedStep = nil
		clearRecoveryRepairBatch(draft)
		if err := m.storeRecoveryRepairDraft(ctx, tx, challenge, draft, token, canonicalOrigin, now, false); err != nil {
			return err
		}
		enrollment, err := setupTOTPEnrollment(email, draft.TOTPSecret, false)
		if err != nil {
			return err
		}
		state = recoveryRepairState(challenge, draft, enrollment)
		return nil
	})
	return state, err
}

func (m *Manager) ConfirmRecoveryRepairTOTP(ctx context.Context, token, origin, code string) (*RecoveryRepairState, error) {
	_, preflight, _, err := m.readRecoveryRepairDraft(ctx, token, origin)
	if err != nil {
		return nil, err
	}
	now := m.clock.Now().UTC()
	matchedStep, valid, err := matchTOTPCode(preflight.TOTPSecret, code, now)
	if err != nil {
		return nil, err
	}
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, ErrRecoveryRepairInvalid
	}
	invalid := false
	terminal := false
	var state *RecoveryRepairState
	err = m.runSecurityTransition(ctx, SecurityTransitionRecovery, func(tx *sql.Tx) error {
		challenge, draft, email, _, err := m.currentRecoveryRepairDraft(ctx, tx, token, canonicalOrigin, now)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(draft.TOTPSecret), []byte(preflight.TOTPSecret)) != 1 {
			return ErrRecoveryRepairStateChanged
		}
		if draft.TOTPConfirmedStep != nil && matchedStep <= *draft.TOTPConfirmedStep {
			valid = false
		}
		if !valid {
			invalid = true
			var consumedAt sql.NullTime
			if err := tx.QueryRowContext(ctx, `
				UPDATE auth_challenges
				SET attempts = attempts + 1,
				    consumed_at = CASE WHEN attempts + 1 >= max_attempts THEN ? ELSE NULL END
				WHERE id = ? AND challenge_hash = ? AND purpose = ? AND origin = ?
				  AND consumed_at IS NULL AND expires_at > ? AND attempts < max_attempts
				RETURNING consumed_at`,
				now, challenge.ID, hashToken(token), ChallengePurposeRecovery, canonicalOrigin, now,
			).Scan(&consumedAt); errors.Is(err, sql.ErrNoRows) {
				return ErrRecoveryRepairInvalid
			} else if err != nil {
				return fmt.Errorf("record rejected recovery-repair TOTP code: %w", err)
			}
			terminal = consumedAt.Valid
			return nil
		}
		draft.TOTPConfirmedStep = &matchedStep
		clearRecoveryRepairBatch(draft)
		if err := m.storeRecoveryRepairDraft(ctx, tx, challenge, draft, token, canonicalOrigin, now, true); err != nil {
			return err
		}
		enrollment, err := setupTOTPEnrollment(email, draft.TOTPSecret, true)
		if err != nil {
			return err
		}
		state = recoveryRepairState(challenge, draft, enrollment)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if invalid {
		return nil, &RecoveryRepairTOTPValidationError{Terminal: terminal}
	}
	return state, nil
}

func (m *Manager) GenerateRecoveryRepairCodes(ctx context.Context, token, origin string, replace bool) (*RecoveryRepairBatch, error) {
	_, preflight, _, err := m.readRecoveryRepairDraft(ctx, token, origin)
	if err != nil {
		return nil, err
	}
	if preflight.TOTPConfirmedStep == nil {
		return nil, ErrRecoveryRepairTOTPRequired
	}
	if preflight.RecoveryBatchID != "" && !replace {
		return nil, ErrRecoveryRepairBatchExists
	}
	randomMaterial, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate recovery-repair code material: %w", err)
	}
	batch, hashes, err := deriveSetupRecoveryBatch(randomMaterial)
	if err != nil {
		return nil, err
	}
	if replace && subtle.ConstantTimeCompare([]byte(batch.BatchID), []byte(preflight.RecoveryBatchID)) == 1 {
		return nil, fmt.Errorf("replace recovery-repair code batch: generated batch did not change")
	}
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, ErrRecoveryRepairInvalid
	}
	now := m.clock.Now().UTC()
	var result *RecoveryRepairBatch
	err = m.runSecurityTransition(ctx, SecurityTransitionRecovery, func(tx *sql.Tx) error {
		challenge, draft, email, _, err := m.currentRecoveryRepairDraft(ctx, tx, token, canonicalOrigin, now)
		if err != nil {
			return err
		}
		if !sameRecoveryRepairDraft(draft, preflight) {
			return ErrRecoveryRepairStateChanged
		}
		if draft.TOTPConfirmedStep == nil {
			return ErrRecoveryRepairTOTPRequired
		}
		if draft.RecoveryBatchID != "" && !replace {
			return ErrRecoveryRepairBatchExists
		}
		draft.RecoveryBatchID = batch.BatchID
		draft.RecoveryCodeHashes = hashes
		if err := m.storeRecoveryRepairDraft(ctx, tx, challenge, draft, token, canonicalOrigin, now, false); err != nil {
			return err
		}
		enrollment, err := setupTOTPEnrollment(email, draft.TOTPSecret, true)
		if err != nil {
			return err
		}
		result = &RecoveryRepairBatch{
			RecoveryRepairState: *recoveryRepairState(challenge, draft, enrollment),
			Codes:               batch.Codes,
		}
		return nil
	})
	return result, err
}

// CompleteRecoveryRepair replaces the active TOTP and remaining old recovery
// batch, revokes prior sessions, consumes the repair challenge, and creates the
// first full session for this login in one transaction.
func (m *Manager) CompleteRecoveryRepair(ctx context.Context, options CompleteRecoveryRepairOptions) (*Session, error) {
	if !options.Saved {
		return nil, ErrRecoveryRepairBatchRequired
	}
	challenge, preflight, _, err := m.readRecoveryRepairDraft(ctx, options.Token, options.Origin)
	if err != nil {
		return nil, err
	}
	if preflight.TOTPConfirmedStep == nil || preflight.RecoveryBatchID == "" ||
		len(preflight.RecoveryCodeHashes) != setupRecoveryCodeCount {
		return nil, ErrRecoveryRepairBatchRequired
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(options.BatchID)), []byte(preflight.RecoveryBatchID)) != 1 {
		return nil, ErrRecoveryRepairStateChanged
	}

	totpID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate repaired TOTP credential ID: %w", err)
	}
	recoveryIDs := make([]string, setupRecoveryCodeCount)
	for index := range recoveryIDs {
		recoveryIDs[index], err = m.tokens.ID()
		if err != nil {
			return nil, fmt.Errorf("generate repaired recovery-code ID %d: %w", index, err)
		}
	}
	sessionID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate recovery-repair session ID: %w", err)
	}
	credentialEventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate recovery-repair credential event ID: %w", err)
	}
	loginEventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate recovery-repair login event ID: %w", err)
	}
	sessionToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate recovery-repair session token: %w", err)
	}
	encryptedSeed, err := m.encryptTOTPSeed(challenge.UserID, totpID, preflight.TOTPSecret)
	if err != nil {
		return nil, err
	}
	canonicalOrigin, err := canonicalAuthOrigin(options.Origin)
	if err != nil || strings.TrimSpace(options.Token) == "" {
		return nil, ErrRecoveryRepairInvalid
	}
	now := m.clock.Now().UTC()
	userAgent := boundedUserAgent(options.UserAgent)
	sourceHash, err := m.authenticationThrottleBucketHash(
		recoveryCodeThrottleAction, loginThrottleBucketSource, strings.TrimSpace(options.Source),
	)
	if err != nil {
		return nil, err
	}
	loginMetadata := fmt.Sprintf(
		`{"factor":"recovery_code","factor_repair":"completed","primary":%q}`,
		preflight.PrimaryMethod,
	)

	var session *Session
	err = m.runSecurityTransition(ctx, SecurityTransitionRecovery, func(tx *sql.Tx) error {
		currentChallenge, draft, _, authVersion, err := m.currentRecoveryRepairDraft(
			ctx, tx, options.Token, canonicalOrigin, now,
		)
		if err != nil {
			return err
		}
		if !sameRecoveryRepairDraft(draft, preflight) ||
			subtle.ConstantTimeCompare([]byte(options.BatchID), []byte(draft.RecoveryBatchID)) != 1 {
			return ErrRecoveryRepairStateChanged
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE totp_credentials SET enabled = 0, revoked_at = ?
			WHERE id = ? AND user_id = ? AND enabled = 1 AND revoked_at IS NULL`,
			now, draft.OriginalTOTP, currentChallenge.UserID,
		); err != nil {
			return fmt.Errorf("revoke recovered TOTP credential: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO totp_credentials (
				id, user_id, encrypted_seed, key_version, algorithm, digits, period,
				issuer, last_accepted_step, enabled, created_at, last_used_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			totpID, currentChallenge.UserID, encryptedSeed, totpCredentialKeyVersion,
			totpAlgorithm, totpDigits, totpPeriodSeconds, totpIssuer,
			*draft.TOTPConfirmedStep, now, now,
		); err != nil {
			return fmt.Errorf("insert repaired TOTP credential: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE recovery_codes SET revoked_at = ?
			WHERE user_id = ? AND used_at IS NULL AND revoked_at IS NULL`,
			now, currentChallenge.UserID,
		); err != nil {
			return fmt.Errorf("revoke recovered recovery-code batch: %w", err)
		}
		for index, codeHash := range draft.RecoveryCodeHashes {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO recovery_codes (id, user_id, batch_id, code_hash, created_at)
				VALUES (?, ?, ?, ?, ?)`,
				recoveryIDs[index], currentChallenge.UserID, draft.RecoveryBatchID, codeHash, now,
			); err != nil {
				return fmt.Errorf("insert repaired recovery code %d: %w", index, err)
			}
		}
		var newAuthVersion int64
		if err := tx.QueryRowContext(ctx, `
			UPDATE users
			SET auth_version = auth_version + 1, last_login_at = ?, updated_at = ?
			WHERE id = ? AND status = 'active' AND auth_version = ?
			RETURNING auth_version`,
			now, now, currentChallenge.UserID, authVersion,
		).Scan(&newAuthVersion); errors.Is(err, sql.ErrNoRows) {
			return ErrRecoveryRepairStateChanged
		} else if err != nil {
			return fmt.Errorf("advance recovered user authentication version: %w", err)
		}
		policy, err := m.requireAuthenticationAssurance(
			ctx, tx, currentChallenge.UserID, newAuthVersion, AssuranceLevelMultiFactor,
		)
		if err != nil || !policy.RequiresMFA {
			if err != nil {
				return err
			}
			return ErrRecoveryRepairStateChanged
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET revoked_at = ?, revoked_by = NULL, revocation_reason = ?
			WHERE user_id = ? AND revoked_at IS NULL`,
			now, SessionRevocationCredentialReset, currentChallenge.UserID,
		); err != nil {
			return fmt.Errorf("revoke sessions after factor repair: %w", err)
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET consumed_at = ?, payload_ciphertext = NULL
			WHERE id = ? AND challenge_hash = ? AND purpose = ? AND origin = ?
			  AND user_id = ? AND session_id IS NULL AND consumed_at IS NULL
			  AND expires_at > ? AND attempts < max_attempts`,
			now, currentChallenge.ID, hashToken(options.Token), ChallengePurposeRecovery,
			canonicalOrigin, currentChallenge.UserID, now,
		)
		if err != nil {
			return fmt.Errorf("consume completed recovery-repair challenge: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrRecoveryRepairInvalid
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET consumed_at = COALESCE(consumed_at, ?), payload_ciphertext = NULL
			WHERE user_id = ? AND purpose IN (?, ?) AND consumed_at IS NULL`,
			now, currentChallenge.UserID, ChallengePurposeMFA, ChallengePurposeRecovery,
		); err != nil {
			return fmt.Errorf("terminate parallel recovery login challenges: %w", err)
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
			sessionID, currentChallenge.UserID, hashToken(sessionToken), newAuthVersion,
			draft.PrimaryMethod, AssuranceLevelMultiFactor, userAgent,
			now, now, idleExpiresAt, absoluteExpiresAt, now, AuthenticationMethodRecoveryCode, now,
		); err != nil {
			return fmt.Errorf("insert recovery-repair session: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, source_hash, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, '{"factor":"totp","source":"recovery_code"}')`,
			credentialEventID, now, currentChallenge.UserID, currentChallenge.UserID,
			sessionID, AuthEventCredentialChanged, AuthEventReasonChallengeVerified,
			userAgent, sourceHash,
		); err != nil {
			return fmt.Errorf("record recovery-repair credential event: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, source_hash, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`,
			loginEventID, now, currentChallenge.UserID, currentChallenge.UserID,
			sessionID, AuthEventLoginSucceeded, AuthEventReasonChallengeVerified,
			userAgent, sourceHash, loginMetadata,
		); err != nil {
			return fmt.Errorf("record recovery-repair login event: %w", err)
		}
		stepUpAt := now
		session = &Session{
			ID: sessionID, UserID: currentChallenge.UserID, Token: sessionToken,
			AuthVersion: newAuthVersion, AuthenticationMethod: draft.PrimaryMethod,
			AssuranceLevel: AssuranceLevelMultiFactor, UserAgent: userAgent,
			AuthenticatedAt: now, LastUsedAt: now, IdleExpiresAt: idleExpiresAt,
			AbsoluteExpiresAt: absoluteExpiresAt, StepUpAt: &stepUpAt,
			StepUpMethod: AuthenticationMethodRecoveryCode, CreatedAt: now,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return session, nil
}

func canonicalRecoveryCode(value string) (string, bool) {
	value = strings.ToUpper(strings.TrimSpace(value))
	var canonical strings.Builder
	canonical.Grow(setupRecoveryCodeByteCount * 8 / 5)
	for _, character := range value {
		if character == '-' || character == ' ' || character == '\t' || character == '\r' || character == '\n' {
			continue
		}
		canonical.WriteRune(character)
	}
	result := canonical.String()
	if len(result) != setupRecoveryCodeByteCount*8/5 {
		return result, false
	}
	for _, character := range result {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", character) {
			return result, false
		}
	}
	return result, true
}

func recoveryRepairState(challenge *PreAuthChallenge, draft *RecoveryRepairDraft, enrollment *SetupTOTPEnrollment) *RecoveryRepairState {
	return &RecoveryRepairState{
		UserID: challenge.UserID, Enrollment: enrollment, TOTPConfirmed: draft.TOTPConfirmedStep != nil,
		RecoveryGenerated: draft.RecoveryBatchID != "" && len(draft.RecoveryCodeHashes) == setupRecoveryCodeCount,
		RecoveryBatchID:   draft.RecoveryBatchID,
	}
}

func clearRecoveryRepairBatch(draft *RecoveryRepairDraft) {
	draft.RecoveryBatchID = ""
	draft.RecoveryCodeHashes = nil
}

func validRecoveryRepairDraft(draft *RecoveryRepairDraft) bool {
	if draft == nil || draft.AuthVersion < 1 || draft.OriginalTOTP == "" ||
		draft.UsedRecoveryCode == "" || draft.TOTPSecret == "" ||
		!validMFAContinuationDraft(&mfaContinuationDraft{
			Version: mfaContinuationDraftVersion, AuthVersion: draft.AuthVersion,
			PrimaryMethod: draft.PrimaryMethod, PrimaryAssurance: draft.PrimaryAssurance,
		}) {
		return false
	}
	if draft.RecoveryBatchID == "" {
		return len(draft.RecoveryCodeHashes) == 0
	}
	if draft.TOTPConfirmedStep == nil || !isLowerHexHash(draft.RecoveryBatchID) ||
		len(draft.RecoveryCodeHashes) != setupRecoveryCodeCount {
		return false
	}
	seen := make(map[string]struct{}, len(draft.RecoveryCodeHashes))
	for _, codeHash := range draft.RecoveryCodeHashes {
		if !isLowerHexHash(codeHash) {
			return false
		}
		if _, duplicate := seen[codeHash]; duplicate {
			return false
		}
		seen[codeHash] = struct{}{}
	}
	return true
}

func sameRecoveryRepairDraft(left, right *RecoveryRepairDraft) bool {
	if !validRecoveryRepairDraft(left) || !validRecoveryRepairDraft(right) ||
		left.AuthVersion != right.AuthVersion || left.OriginalTOTP != right.OriginalTOTP ||
		left.PrimaryMethod != right.PrimaryMethod || left.PrimaryAssurance != right.PrimaryAssurance ||
		left.UsedRecoveryCode != right.UsedRecoveryCode ||
		subtle.ConstantTimeCompare([]byte(left.TOTPSecret), []byte(right.TOTPSecret)) != 1 ||
		left.RecoveryBatchID != right.RecoveryBatchID || len(left.RecoveryCodeHashes) != len(right.RecoveryCodeHashes) {
		return false
	}
	if (left.TOTPConfirmedStep == nil) != (right.TOTPConfirmedStep == nil) ||
		(left.TOTPConfirmedStep != nil && *left.TOTPConfirmedStep != *right.TOTPConfirmedStep) {
		return false
	}
	for index := range left.RecoveryCodeHashes {
		if subtle.ConstantTimeCompare([]byte(left.RecoveryCodeHashes[index]), []byte(right.RecoveryCodeHashes[index])) != 1 {
			return false
		}
	}
	return true
}

func (m *Manager) readRecoveryRepairDraft(ctx context.Context, token, origin string) (*PreAuthChallenge, *RecoveryRepairDraft, string, error) {
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, nil, "", ErrRecoveryRepairInvalid
	}
	tx, err := m.db.Read().BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, "", fmt.Errorf("begin recovery-repair read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	challenge, draft, email, _, err := m.currentRecoveryRepairDraft(ctx, tx, token, canonicalOrigin, m.clock.Now().UTC())
	if err != nil {
		return nil, nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, "", fmt.Errorf("commit recovery-repair read: %w", err)
	}
	return challenge, draft, email, nil
}

func (m *Manager) currentRecoveryRepairDraft(ctx context.Context, tx *sql.Tx, token, origin string, now time.Time) (*PreAuthChallenge, *RecoveryRepairDraft, string, int64, error) {
	challenge := &PreAuthChallenge{}
	var purpose string
	var consumedAt sql.NullTime
	var email, activeTOTP string
	var authVersion int64
	err := tx.QueryRowContext(ctx, `
		SELECT c.id, c.user_id, COALESCE(c.session_id, ''), c.purpose, c.origin,
		       c.attempts, c.max_attempts, c.payload_ciphertext, c.created_at,
		       c.expires_at, c.consumed_at,
		       u.username_normalized,
		       u.auth_version, t.id
		FROM auth_challenges c
		JOIN users u ON u.id = c.user_id
		JOIN totp_credentials t ON t.user_id = c.user_id
		WHERE c.challenge_hash = ? AND c.purpose = ? AND c.origin = ?
		  AND c.session_id IS NULL AND c.consumed_at IS NULL
		  AND c.expires_at > ? AND c.attempts < c.max_attempts
		  AND u.status = 'active'
		  AND t.enabled = 1 AND t.revoked_at IS NULL`,
		hashToken(strings.TrimSpace(token)), ChallengePurposeRecovery, origin, now,
	).Scan(
		&challenge.ID, &challenge.UserID, &challenge.SessionID, &purpose, &challenge.Origin,
		&challenge.Attempts, &challenge.MaxAttempts, &challenge.PayloadCiphertext,
		&challenge.CreatedAt, &challenge.ExpiresAt, &consumedAt,
		&email, &authVersion, &activeTOTP,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, "", 0, ErrRecoveryRepairInvalid
	}
	if err != nil {
		return nil, nil, "", 0, fmt.Errorf("read recovery-repair state: %w", err)
	}
	challenge.Purpose = ChallengePurpose(purpose)
	draft, err := m.decryptRecoveryRepairDraft(challenge, challenge.PayloadCiphertext)
	if err != nil {
		return nil, nil, "", 0, err
	}
	if !validRecoveryRepairDraft(draft) || draft.AuthVersion != authVersion || draft.OriginalTOTP != activeTOTP {
		return nil, nil, "", 0, ErrRecoveryRepairStateChanged
	}
	return challenge, draft, email, authVersion, nil
}

func (m *Manager) storeRecoveryRepairDraft(ctx context.Context, tx *sql.Tx, challenge *PreAuthChallenge, draft *RecoveryRepairDraft, token, origin string, now time.Time, resetAttempts bool) error {
	if !validRecoveryRepairDraft(draft) {
		return fmt.Errorf("recovery-repair draft is invalid")
	}
	payload, err := m.encryptRecoveryRepairDraft(challenge, draft)
	if err != nil {
		return err
	}
	attemptsAssignment := "attempts"
	if resetAttempts {
		attemptsAssignment = "0"
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE auth_challenges SET payload_ciphertext = ?, attempts = `+attemptsAssignment+`
		WHERE id = ? AND challenge_hash = ? AND purpose = ? AND origin = ?
		  AND user_id = ? AND session_id IS NULL AND consumed_at IS NULL
		  AND expires_at > ? AND attempts < max_attempts`,
		payload, challenge.ID, hashToken(token), ChallengePurposeRecovery, origin,
		challenge.UserID, now,
	)
	if err != nil {
		return fmt.Errorf("store recovery-repair draft: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return ErrRecoveryRepairInvalid
	}
	challenge.PayloadCiphertext = payload
	return nil
}

func (m *Manager) recoveryRepairAEAD() (cipher.AEAD, error) {
	if len(m.bucketHashKey) < minimumBucketHashKeyBytes {
		return nil, fmt.Errorf("recovery-repair encryption key is unavailable")
	}
	deriver := hmac.New(sha256.New, m.bucketHashKey)
	_, _ = deriver.Write([]byte(recoveryRepairDraftKeyContext))
	block, err := aes.NewCipher(deriver.Sum(nil))
	if err != nil {
		return nil, fmt.Errorf("create recovery-repair cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func (m *Manager) encryptRecoveryRepairDraft(challenge *PreAuthChallenge, draft *RecoveryRepairDraft) ([]byte, error) {
	aead, err := m.recoveryRepairAEAD()
	if err != nil {
		return nil, err
	}
	plaintext, err := json.Marshal(draft)
	if err != nil {
		return nil, fmt.Errorf("encode recovery-repair draft: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate recovery-repair nonce: %w", err)
	}
	payload := []byte{recoveryRepairDraftVersion}
	payload = append(payload, nonce...)
	payload = aead.Seal(payload, nonce, plaintext, recoveryRepairDraftAAD(challenge))
	return payload, nil
}

func (m *Manager) decryptRecoveryRepairDraft(challenge *PreAuthChallenge, payload []byte) (*RecoveryRepairDraft, error) {
	aead, err := m.recoveryRepairAEAD()
	if err != nil {
		return nil, err
	}
	if len(payload) < 1+aead.NonceSize()+aead.Overhead() || payload[0] != recoveryRepairDraftVersion {
		return nil, fmt.Errorf("recovery-repair payload is invalid")
	}
	nonce := payload[1 : 1+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, payload[1+aead.NonceSize():], recoveryRepairDraftAAD(challenge))
	if err != nil {
		return nil, fmt.Errorf("authenticate recovery-repair draft: %w", err)
	}
	var draft RecoveryRepairDraft
	if err := json.Unmarshal(plaintext, &draft); err != nil {
		return nil, fmt.Errorf("decode recovery-repair draft: %w", err)
	}
	return &draft, nil
}

func recoveryRepairDraftAAD(challenge *PreAuthChallenge) []byte {
	return []byte(challenge.ID + "\x00" + challenge.UserID + "\x00" + challenge.Origin + "\x00" + string(challenge.Purpose))
}
