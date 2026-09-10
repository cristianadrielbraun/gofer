package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrMFAEnrollmentInvalid       = errors.New("required MFA enrollment is invalid or expired")
	ErrMFAEnrollmentTOTPRequired  = errors.New("a verified authenticator is required")
	ErrMFAEnrollmentBatchRequired = errors.New("a saved recovery-code batch is required")
	ErrMFAEnrollmentBatchExists   = errors.New("a recovery-code batch already exists")
	ErrMFAEnrollmentStateChanged  = errors.New("required MFA enrollment state changed")
)

type mfaEnrollmentDraft struct {
	TOTPSecret         string   `json:"totp_secret"`
	TOTPConfirmedStep  *int64   `json:"totp_confirmed_step,omitempty"`
	RecoveryBatchID    string   `json:"recovery_batch_id,omitempty"`
	RecoveryCodeHashes []string `json:"recovery_code_hashes,omitempty"`
}

type MFAEnrollmentState struct {
	UserID            string
	Enrollment        *SetupTOTPEnrollment
	TOTPConfirmed     bool
	RecoveryGenerated bool
	RecoveryBatchID   string
}

type MFAEnrollmentBatch struct {
	MFAEnrollmentState
	Codes []string
}

type MFAEnrollmentTOTPValidationError struct {
	Terminal bool
}

func (*MFAEnrollmentTOTPValidationError) Error() string {
	return "authenticator enrollment code is invalid"
}

type CompleteMFAEnrollmentOptions struct {
	Token     string
	Origin    string
	BatchID   string
	Saved     bool
	Source    string
	UserAgent string
}

func validMFAEnrollmentDraft(draft *mfaEnrollmentDraft) bool {
	if draft == nil {
		return true
	}
	if strings.TrimSpace(draft.TOTPSecret) == "" {
		return false
	}
	if draft.TOTPConfirmedStep == nil {
		return draft.RecoveryBatchID == "" && len(draft.RecoveryCodeHashes) == 0
	}
	if draft.RecoveryBatchID == "" {
		return len(draft.RecoveryCodeHashes) == 0
	}
	return isLowerHexHash(draft.RecoveryBatchID) && len(draft.RecoveryCodeHashes) == setupRecoveryCodeCount
}

func sameMFAEnrollmentDraft(left, right *mfaEnrollmentDraft) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	if !validMFAEnrollmentDraft(left) || !validMFAEnrollmentDraft(right) ||
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

func (m *Manager) preparePrimaryMFAContinuation(
	ctx context.Context,
	queryer authenticationPolicyQueryer,
	userID string,
	authVersion int64,
	primaryMethod AuthenticationMethod,
	policy authenticationPolicy,
	origin string,
	now time.Time,
) (*PreAuthChallenge, *mfaContinuationDraft, error) {
	challenge, draft, err := m.prepareMFAContinuation(userID, authVersion, primaryMethod, origin, now)
	if err != nil {
		return nil, nil, err
	}
	if !policy.RequiresMFA {
		return nil, nil, ErrAuthenticationPolicyNotSatisfied
	}
	_, rpID, err := canonicalWebAuthnRelyingParty(m.config.BaseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve MFA enrollment relying party: %w", err)
	}
	ready, err := userHasStrongAuthenticator(ctx, queryer, userID, rpID)
	if err != nil {
		return nil, nil, err
	}
	if ready {
		return challenge, draft, nil
	}
	var accountName string
	if err := queryer.QueryRowContext(ctx, `
		SELECT username_normalized FROM users
		WHERE id = ? AND status IN ('active', 'pending') AND auth_version = ?`, userID, authVersion,
	).Scan(&accountName); errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrUserNotActive
	} else if err != nil {
		return nil, nil, fmt.Errorf("load MFA enrollment account: %w", err)
	}
	material, err := m.tokens.Token(32)
	if err != nil {
		return nil, nil, fmt.Errorf("generate MFA enrollment material: %w", err)
	}
	key, err := newTOTPKey(accountName, material)
	if err != nil {
		return nil, nil, err
	}
	draft.Enrollment = &mfaEnrollmentDraft{TOTPSecret: key.Secret()}
	payload, err := m.encryptMFAContinuationDraft(challenge, draft)
	if err != nil {
		return nil, nil, err
	}
	challenge.PayloadCiphertext = payload
	return challenge, draft, nil
}

func (m *Manager) requireMFAContinuationAuthenticatorState(
	ctx context.Context, queryer instanceSecurityPolicyQueryer, userID string, draft *mfaContinuationDraft,
) error {
	if !validMFAContinuationDraft(draft) {
		return ErrMFAContinuationInvalid
	}
	_, rpID, err := canonicalWebAuthnRelyingParty(m.config.BaseURL)
	if err != nil {
		return err
	}
	ready, err := userHasStrongAuthenticator(ctx, queryer, userID, rpID)
	if err != nil {
		return err
	}
	if (draft.Enrollment == nil) != ready {
		return ErrMFAEnrollmentStateChanged
	}
	return nil
}

func (m *Manager) GetMFAEnrollmentState(ctx context.Context, token, origin string) (*MFAEnrollmentState, error) {
	challenge, draft, accountName, err := m.readMFAEnrollmentDraft(ctx, token, origin)
	if err != nil {
		return nil, err
	}
	enrollment, err := setupTOTPEnrollment(accountName, draft.Enrollment.TOTPSecret, draft.Enrollment.TOTPConfirmedStep != nil)
	if err != nil {
		return nil, err
	}
	return mfaEnrollmentState(challenge, draft.Enrollment, enrollment), nil
}

func (m *Manager) RestartMFAEnrollment(ctx context.Context, token, origin string) (*MFAEnrollmentState, error) {
	material, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate replacement MFA enrollment material: %w", err)
	}
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, ErrMFAEnrollmentInvalid
	}
	now := m.clock.Now().UTC()
	var state *MFAEnrollmentState
	err = m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		challenge, draft, accountName, err := m.currentMFAEnrollmentDraft(ctx, tx, token, canonicalOrigin, now)
		if err != nil {
			return err
		}
		key, err := newTOTPKey(accountName, material)
		if err != nil {
			return err
		}
		draft.Enrollment = &mfaEnrollmentDraft{TOTPSecret: key.Secret()}
		if err := m.storeMFAEnrollmentDraft(ctx, tx, challenge, draft, token, canonicalOrigin, now); err != nil {
			return err
		}
		enrollment, err := setupTOTPEnrollment(accountName, draft.Enrollment.TOTPSecret, false)
		if err != nil {
			return err
		}
		state = mfaEnrollmentState(challenge, draft.Enrollment, enrollment)
		return nil
	})
	return state, err
}

func (m *Manager) ConfirmMFAEnrollmentTOTP(ctx context.Context, token, origin, code string) (*MFAEnrollmentState, error) {
	_, preflight, _, err := m.readMFAEnrollmentDraft(ctx, token, origin)
	if err != nil {
		return nil, err
	}
	now := m.clock.Now().UTC()
	matchedStep, valid, err := matchTOTPCode(preflight.Enrollment.TOTPSecret, code, now)
	if err != nil {
		return nil, err
	}
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, ErrMFAEnrollmentInvalid
	}
	invalid := false
	terminal := false
	var state *MFAEnrollmentState
	err = m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		challenge, draft, accountName, err := m.currentMFAEnrollmentDraft(ctx, tx, token, canonicalOrigin, now)
		if err != nil || !sameMFAContinuationDraft(draft, preflight) {
			return ErrMFAEnrollmentStateChanged
		}
		if draft.Enrollment.TOTPConfirmedStep != nil && matchedStep <= *draft.Enrollment.TOTPConfirmedStep {
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
				now, challenge.ID, hashToken(token), ChallengePurposeMFA, canonicalOrigin, now,
			).Scan(&consumedAt); errors.Is(err, sql.ErrNoRows) {
				return ErrMFAEnrollmentInvalid
			} else if err != nil {
				return fmt.Errorf("record rejected MFA enrollment code: %w", err)
			}
			terminal = consumedAt.Valid
			return nil
		}
		draft.Enrollment.TOTPConfirmedStep = &matchedStep
		draft.Enrollment.RecoveryBatchID = ""
		draft.Enrollment.RecoveryCodeHashes = nil
		if err := m.storeMFAEnrollmentDraft(ctx, tx, challenge, draft, token, canonicalOrigin, now); err != nil {
			return err
		}
		enrollment, err := setupTOTPEnrollment(accountName, draft.Enrollment.TOTPSecret, true)
		if err != nil {
			return err
		}
		state = mfaEnrollmentState(challenge, draft.Enrollment, enrollment)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if invalid {
		return nil, &MFAEnrollmentTOTPValidationError{Terminal: terminal}
	}
	return state, nil
}

func (m *Manager) GenerateMFAEnrollmentRecoveryCodes(ctx context.Context, token, origin string, replace bool) (*MFAEnrollmentBatch, error) {
	_, preflight, _, err := m.readMFAEnrollmentDraft(ctx, token, origin)
	if err != nil {
		return nil, err
	}
	if preflight.Enrollment.TOTPConfirmedStep == nil {
		return nil, ErrMFAEnrollmentTOTPRequired
	}
	if preflight.Enrollment.RecoveryBatchID != "" && !replace {
		return nil, ErrMFAEnrollmentBatchExists
	}
	material, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate MFA enrollment recovery material: %w", err)
	}
	batch, hashes, err := deriveSetupRecoveryBatch(material)
	if err != nil {
		return nil, err
	}
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, ErrMFAEnrollmentInvalid
	}
	now := m.clock.Now().UTC()
	var result *MFAEnrollmentBatch
	err = m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		challenge, draft, accountName, err := m.currentMFAEnrollmentDraft(ctx, tx, token, canonicalOrigin, now)
		if err != nil || !sameMFAContinuationDraft(draft, preflight) {
			return ErrMFAEnrollmentStateChanged
		}
		if draft.Enrollment.TOTPConfirmedStep == nil {
			return ErrMFAEnrollmentTOTPRequired
		}
		if draft.Enrollment.RecoveryBatchID != "" && !replace {
			return ErrMFAEnrollmentBatchExists
		}
		draft.Enrollment.RecoveryBatchID = batch.BatchID
		draft.Enrollment.RecoveryCodeHashes = hashes
		if err := m.storeMFAEnrollmentDraft(ctx, tx, challenge, draft, token, canonicalOrigin, now); err != nil {
			return err
		}
		enrollment, err := setupTOTPEnrollment(accountName, draft.Enrollment.TOTPSecret, true)
		if err != nil {
			return err
		}
		result = &MFAEnrollmentBatch{
			MFAEnrollmentState: *mfaEnrollmentState(challenge, draft.Enrollment, enrollment),
			Codes:              batch.Codes,
		}
		return nil
	})
	return result, err
}

func (m *Manager) CompleteMFAEnrollment(ctx context.Context, options CompleteMFAEnrollmentOptions) (*Session, error) {
	if !options.Saved {
		return nil, ErrMFAEnrollmentBatchRequired
	}
	challenge, preflight, _, err := m.readMFAEnrollmentDraft(ctx, options.Token, options.Origin)
	if err != nil {
		return nil, err
	}
	if preflight.Enrollment.TOTPConfirmedStep == nil || preflight.Enrollment.RecoveryBatchID == "" ||
		len(preflight.Enrollment.RecoveryCodeHashes) != setupRecoveryCodeCount {
		return nil, ErrMFAEnrollmentBatchRequired
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(options.BatchID)), []byte(preflight.Enrollment.RecoveryBatchID)) != 1 {
		return nil, ErrMFAEnrollmentStateChanged
	}
	totpID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate enrolled TOTP credential ID: %w", err)
	}
	recoveryIDs := make([]string, setupRecoveryCodeCount)
	for index := range recoveryIDs {
		recoveryIDs[index], err = m.tokens.ID()
		if err != nil {
			return nil, fmt.Errorf("generate enrolled recovery-code ID %d: %w", index, err)
		}
	}
	sessionID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate enrolled session ID: %w", err)
	}
	sessionToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate enrolled session token: %w", err)
	}
	credentialEventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate MFA enrollment event ID: %w", err)
	}
	loginEventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate MFA enrollment login event ID: %w", err)
	}
	encryptedSeed, err := m.encryptTOTPSeed(challenge.UserID, totpID, preflight.Enrollment.TOTPSecret)
	if err != nil {
		return nil, err
	}
	canonicalOrigin, err := canonicalAuthOrigin(options.Origin)
	if err != nil || strings.TrimSpace(options.Token) == "" {
		return nil, ErrMFAEnrollmentInvalid
	}
	now := m.clock.Now().UTC()
	userAgent := boundedUserAgent(options.UserAgent)
	sourceHash, err := m.authenticationThrottleBucketHash(
		totpLoginThrottleAction, loginThrottleBucketSource, strings.TrimSpace(options.Source),
	)
	if err != nil {
		return nil, err
	}
	var session *Session
	err = m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		currentChallenge, draft, _, err := m.currentMFAEnrollmentDraft(ctx, tx, options.Token, canonicalOrigin, now)
		if err != nil || currentChallenge.ID != challenge.ID || !sameMFAContinuationDraft(draft, preflight) {
			return ErrMFAEnrollmentStateChanged
		}
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(options.BatchID)), []byte(draft.Enrollment.RecoveryBatchID)) != 1 {
			return ErrMFAEnrollmentStateChanged
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO totp_credentials (
				id, user_id, encrypted_seed, key_version, algorithm, digits, period,
				issuer, last_accepted_step, enabled, created_at, last_used_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			totpID, currentChallenge.UserID, encryptedSeed, totpCredentialKeyVersion,
			totpAlgorithm, totpDigits, totpPeriodSeconds, totpIssuer,
			*draft.Enrollment.TOTPConfirmedStep, now, now,
		); err != nil {
			return fmt.Errorf("insert required TOTP enrollment: %w", err)
		}
		for index, codeHash := range draft.Enrollment.RecoveryCodeHashes {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO recovery_codes (id, user_id, batch_id, code_hash, created_at)
				VALUES (?, ?, ?, ?, ?)`,
				recoveryIDs[index], currentChallenge.UserID, draft.Enrollment.RecoveryBatchID, codeHash, now,
			); err != nil {
				return fmt.Errorf("insert enrolled recovery code %d: %w", index, err)
			}
		}
		consumed, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET consumed_at = ?, payload_ciphertext = NULL
			WHERE id = ? AND challenge_hash = ? AND purpose = ? AND origin = ?
			  AND user_id = ? AND session_id IS NULL AND consumed_at IS NULL
			  AND expires_at > ? AND attempts < max_attempts`,
			now, currentChallenge.ID, hashToken(options.Token), ChallengePurposeMFA,
			canonicalOrigin, currentChallenge.UserID, now,
		)
		if err != nil {
			return fmt.Errorf("consume required MFA enrollment: %w", err)
		}
		if changed, err := consumed.RowsAffected(); err != nil || changed != 1 {
			return ErrMFAEnrollmentInvalid
		}
		if err := revokeSecuritySessions(ctx, tx, currentChallenge.UserID, now); err != nil {
			return err
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
				idle_expires_at, absolute_expires_at, step_up_at, step_up_method, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, currentChallenge.UserID, hashToken(sessionToken), draft.AuthVersion,
			draft.PrimaryMethod, AssuranceLevelMultiFactor, userAgent, now, now,
			idleExpiresAt, absoluteExpiresAt, now, AuthenticationMethodTOTP, now,
		); err != nil {
			return fmt.Errorf("insert session after required MFA enrollment: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE users SET last_login_at = ?, updated_at = ?
			WHERE id = ? AND status = 'active' AND auth_version = ?`,
			now, now, currentChallenge.UserID, draft.AuthVersion,
		); err != nil {
			return fmt.Errorf("update required MFA enrollment login timestamp: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, source_hash, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, '{"action":"enrolled","factor":"totp","scope":"required_login"}')`,
			credentialEventID, now, currentChallenge.UserID, currentChallenge.UserID, sessionID,
			AuthEventCredentialChanged, AuthEventReasonChallengeVerified, userAgent, sourceHash,
		); err != nil {
			return fmt.Errorf("record required MFA enrollment event: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, source_hash, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, '{"factor":"totp","primary":"`+string(draft.PrimaryMethod)+`","enrollment":"completed"}')`,
			loginEventID, now, currentChallenge.UserID, currentChallenge.UserID, sessionID,
			AuthEventLoginSucceeded, AuthEventReasonChallengeVerified, userAgent, sourceHash,
		); err != nil {
			return fmt.Errorf("record login after required MFA enrollment: %w", err)
		}
		session = &Session{
			ID: sessionID, UserID: currentChallenge.UserID, Token: sessionToken,
			AuthVersion: draft.AuthVersion, AuthenticationMethod: draft.PrimaryMethod,
			AssuranceLevel: AssuranceLevelMultiFactor, UserAgent: userAgent,
			AuthenticatedAt: now, LastUsedAt: now, IdleExpiresAt: idleExpiresAt,
			AbsoluteExpiresAt: absoluteExpiresAt, StepUpAt: &now,
			StepUpMethod: AuthenticationMethodTOTP, CreatedAt: now,
		}
		return nil
	})
	return session, err
}

func (m *Manager) readMFAEnrollmentDraft(ctx context.Context, token, origin string) (*PreAuthChallenge, *mfaContinuationDraft, string, error) {
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, nil, "", ErrMFAEnrollmentInvalid
	}
	tx, err := m.db.Read().BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, "", fmt.Errorf("begin MFA enrollment read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	challenge, draft, accountName, err := m.currentMFAEnrollmentDraft(ctx, tx, token, canonicalOrigin, m.clock.Now().UTC())
	if err != nil {
		return nil, nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, "", fmt.Errorf("commit MFA enrollment read: %w", err)
	}
	return challenge, draft, accountName, nil
}

func (m *Manager) currentMFAEnrollmentDraft(
	ctx context.Context, tx *sql.Tx, token, origin string, now time.Time,
) (*PreAuthChallenge, *mfaContinuationDraft, string, error) {
	challenge, draft, err := m.currentMFAContinuation(ctx, tx, token, origin, now)
	if err != nil || draft.Enrollment == nil {
		return nil, nil, "", ErrMFAEnrollmentInvalid
	}
	policy, err := m.loadAuthenticationPolicy(ctx, tx, challenge.UserID, draft.AuthVersion)
	if err != nil || !policy.RequiresMFA {
		return nil, nil, "", ErrMFAEnrollmentInvalid
	}
	if err := m.requireMFAContinuationAuthenticatorState(ctx, tx, challenge.UserID, draft); err != nil {
		return nil, nil, "", ErrMFAEnrollmentStateChanged
	}
	var accountName string
	if err := tx.QueryRowContext(ctx, `
		SELECT username_normalized FROM users
		WHERE id = ? AND status = 'active' AND auth_version = ?`, challenge.UserID, draft.AuthVersion,
	).Scan(&accountName); err != nil {
		return nil, nil, "", ErrMFAEnrollmentInvalid
	}
	return challenge, draft, accountName, nil
}

func (m *Manager) storeMFAEnrollmentDraft(
	ctx context.Context, tx *sql.Tx, challenge *PreAuthChallenge, draft *mfaContinuationDraft,
	token, origin string, now time.Time,
) error {
	if challenge == nil || draft == nil || draft.Enrollment == nil || !validMFAContinuationDraft(draft) {
		return ErrMFAEnrollmentInvalid
	}
	payload, err := m.encryptMFAContinuationDraft(challenge, draft)
	if err != nil {
		return err
	}
	updated, err := tx.ExecContext(ctx, `
		UPDATE auth_challenges SET payload_ciphertext = ?
		WHERE id = ? AND challenge_hash = ? AND purpose = ? AND origin = ?
		  AND user_id = ? AND session_id IS NULL AND consumed_at IS NULL
		  AND expires_at > ? AND attempts < max_attempts`,
		payload, challenge.ID, hashToken(token), ChallengePurposeMFA, origin,
		challenge.UserID, now,
	)
	if err != nil {
		return fmt.Errorf("store required MFA enrollment: %w", err)
	}
	if changed, err := updated.RowsAffected(); err != nil || changed != 1 {
		return ErrMFAEnrollmentInvalid
	}
	challenge.PayloadCiphertext = payload
	return nil
}

func mfaEnrollmentState(challenge *PreAuthChallenge, draft *mfaEnrollmentDraft, enrollment *SetupTOTPEnrollment) *MFAEnrollmentState {
	return &MFAEnrollmentState{
		UserID: challenge.UserID, Enrollment: enrollment,
		TOTPConfirmed:     draft.TOTPConfirmedStep != nil,
		RecoveryGenerated: draft.RecoveryBatchID != "" && len(draft.RecoveryCodeHashes) == setupRecoveryCodeCount,
		RecoveryBatchID:   draft.RecoveryBatchID,
	}
}
