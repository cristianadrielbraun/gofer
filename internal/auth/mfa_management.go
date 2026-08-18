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
	"net/http"
	"strings"
	"time"
)

const (
	securityChallengeCookieName    = "gofer_security_challenge"
	securityManagementLifetime     = 10 * time.Minute
	securityManagementMaxAttempts  = 5
	securityStepUpMaximumAge       = 10 * time.Minute
	securityManagementDraftVersion = 1
	securityManagementDraftKey     = "gofer/auth/security-management-draft/v1"
	securityManagementKindTOTP     = "totp_replacement"
	securityManagementKindRecovery = "recovery_replacement"
)

var (
	ErrSecuritySessionInvalid     = errors.New("security settings session is invalid")
	ErrSecurityChallengeInvalid   = errors.New("security settings challenge is invalid")
	ErrTOTPManagementUnavailable  = errors.New("an active authenticator is required")
	ErrLastAuthenticator          = errors.New("cannot remove the last required authenticator")
	ErrRecoveryManagementRequired = errors.New("a pending recovery-code batch is required")
)

type SecurityFactorSummary struct {
	HasTOTP                bool
	HasPasskey             bool
	RequiresMFA            bool
	Passkeys               []PasskeyCredentialSummary
	RecoveryCodesRemaining int
	StepUpFresh            bool
	StepUpExpiresAt        *time.Time
	CanDisableTOTP         bool
	DisableTOTPReason      string
}

type TOTPManagementValidationError struct {
	Terminal bool
}

func (err *TOTPManagementValidationError) Error() string {
	return "authenticator code is invalid"
}

type TOTPManagementState struct {
	Challenge  *PreAuthChallenge
	Enrollment *SetupTOTPEnrollment
}

type RecoveryManagementState struct {
	Challenge *PreAuthChallenge
	BatchID   string
}

type RecoveryManagementBatch struct {
	RecoveryManagementState
	Codes []string
}

type securityManagementDraft struct {
	Kind               string   `json:"kind"`
	AuthVersion        int64    `json:"auth_version"`
	OriginalTOTP       string   `json:"original_totp,omitempty"`
	NewTOTP            string   `json:"new_totp,omitempty"`
	TOTPSecret         string   `json:"totp_secret,omitempty"`
	RecoveryBatchID    string   `json:"recovery_batch_id,omitempty"`
	RecoveryCodeIDs    []string `json:"recovery_code_ids,omitempty"`
	RecoveryCodeHashes []string `json:"recovery_code_hashes,omitempty"`
}

type activeTOTPCredential struct {
	id               string
	encryptedSeed    []byte
	keyVersion       int
	algorithm        string
	digits           int
	period           int
	lastAcceptedStep sql.NullInt64
}

// GetSecurityFactorSummary returns non-secret factor state for the exact
// active session. It does not refresh activity, step-up, or credential state.
func (m *Manager) GetSecurityFactorSummary(ctx context.Context, sessionToken string) (*SecurityFactorSummary, error) {
	session, err := m.GetSessionByToken(ctx, sessionToken)
	if err != nil {
		return nil, fmt.Errorf("load security settings session: %w", err)
	}
	if session == nil {
		return nil, ErrSecuritySessionInvalid
	}
	_, rpID, err := canonicalWebAuthnRelyingParty(m.config.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("validate security settings relying party: %w", err)
	}
	var hasTOTP, recoveryCount, hasPassword, passkeyCount, googleIdentityCount, microsoftIdentityCount, oidcIdentityCount int
	err = m.db.Read().QueryRowContext(ctx, `
		SELECT
			EXISTS(SELECT 1 FROM totp_credentials t WHERE t.user_id = u.id AND t.enabled = 1 AND t.revoked_at IS NULL),
			(SELECT COUNT(*) FROM recovery_codes r WHERE r.user_id = u.id AND r.used_at IS NULL AND r.revoked_at IS NULL),
			EXISTS(SELECT 1 FROM password_credentials p WHERE p.user_id = u.id),
			(SELECT COUNT(*) FROM webauthn_credentials w
			 WHERE w.user_id = u.id AND w.rp_id = ? AND w.revoked_at IS NULL
			   AND w.credential_ciphertext IS NOT NULL AND w.key_version IS NOT NULL),
			(SELECT COUNT(*) FROM auth_identities identity
			 WHERE identity.user_id = u.id AND identity.provider = ? AND identity.issuer = ?),
			(SELECT COUNT(*) FROM auth_identities identity
			 WHERE identity.user_id = u.id AND identity.provider = ?
			   AND identity.issuer LIKE 'https://login.microsoftonline.com/%/v2.0'),
			(SELECT COUNT(*) FROM auth_identities identity
			 WHERE identity.user_id = u.id AND identity.provider = ? AND identity.issuer = ?)
		FROM users u
		WHERE u.id = ? AND u.status = 'active' AND u.auth_version = ?`,
		rpID, googleIdentityProvider, googleLoginIssuer, microsoftIdentityProvider,
		oidcIdentityProvider, m.config.OIDCLoginIssuer,
		session.UserID, session.AuthVersion,
	).Scan(&hasTOTP, &recoveryCount, &hasPassword, &passkeyCount, &googleIdentityCount, &microsoftIdentityCount, &oidcIdentityCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSecuritySessionInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("load security factor summary: %w", err)
	}
	now := m.clock.Now().UTC()
	policy, err := m.loadAuthenticationPolicy(ctx, m.db.Read(), session.UserID, session.AuthVersion)
	if err != nil {
		return nil, fmt.Errorf("load security factor policy: %w", err)
	}
	summary := &SecurityFactorSummary{
		HasTOTP:                hasTOTP == 1,
		HasPasskey:             passkeyCount > 0,
		RequiresMFA:            policy.RequiresMFA,
		RecoveryCodesRemaining: recoveryCount,
		StepUpFresh:            hasRecentSecurityStepUp(session, policy, now),
	}
	summary.Passkeys, err = m.listPasskeysForUser(ctx, session.UserID)
	if err != nil {
		return nil, err
	}
	if session.StepUpAt != nil {
		expiresAt := session.StepUpAt.UTC().Add(securityStepUpMaximumAge)
		summary.StepUpExpiresAt = &expiresAt
	}
	if summary.HasTOTP {
		hasPrimary := hasPassword == 1 || passkeyCount > 0 ||
			(m.HasGoogleLogin() && googleIdentityCount > 0) ||
			(m.HasMicrosoftLogin() && microsoftIdentityCount > 0) ||
			(m.HasOIDCLogin() && oidcIdentityCount > 0)
		hasOtherStrongFactor := passkeyCount > 0
		summary.CanDisableTOTP = hasPrimary && (!policy.RequiresMFA || hasOtherStrongFactor)
		if !summary.CanDisableTOTP {
			if policy.RequiresMFA {
				summary.DisableTOTPReason = "Add another strong authenticator before disabling this one."
			} else {
				summary.DisableTOTPReason = "Add another sign-in method before disabling this authenticator."
			}
		}
	}
	return summary, nil
}

// VerifySecurityTOTPStepUp replay-safely verifies the active TOTP and refreshes
// only the current session's sensitive-action window.
func (m *Manager) VerifySecurityTOTPStepUp(ctx context.Context, sessionToken, code, source, userAgent string) error {
	preflight, err := m.GetSessionByToken(ctx, sessionToken)
	if err != nil {
		return err
	}
	if preflight == nil {
		return ErrSecuritySessionInvalid
	}
	buckets, err := m.totpManagementThrottleBuckets(preflight.UserID, source)
	if err != nil {
		return err
	}
	decision, err := m.checkLoginThrottleBuckets(ctx, buckets)
	if err != nil {
		return fmt.Errorf("check TOTP management throttle: %w", err)
	}
	if decision.Throttled {
		return m.loginThrottleError(decision)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return fmt.Errorf("generate security step-up event ID: %w", err)
	}
	sourceHash, err := m.authenticationThrottleBucketHash(
		totpManagementThrottleAction, loginThrottleBucketSource, strings.TrimSpace(source),
	)
	if err != nil {
		return err
	}
	now := m.clock.Now().UTC()
	invalid := false
	retryAt := time.Time{}
	userAgent = boundedUserAgent(userAgent)
	err = m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		session, err := currentSecuritySession(ctx, tx, sessionToken, now, false)
		if err != nil {
			return err
		}
		if session.ID != preflight.ID || session.UserID != preflight.UserID {
			return ErrSecuritySessionInvalid
		}
		credential, err := loadActiveTOTPCredential(ctx, tx, session.UserID)
		if err != nil {
			return err
		}
		seed, err := m.decryptTOTPSeed(session.UserID, credential.id, credential.encryptedSeed, credential.keyVersion)
		if err != nil {
			return err
		}
		matchedStep, valid, err := matchTOTPCode(seed, code, now)
		if err != nil {
			return err
		}
		if credential.lastAcceptedStep.Valid && matchedStep <= credential.lastAcceptedStep.Int64 {
			valid = false
		}
		if !valid {
			invalid = true
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
					id, occurred_at, actor_user_id, subject_user_id, session_id,
					event_type, success, reason, user_agent, source_hash, metadata_json
				) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, '{"factor":"totp","scope":"security_settings"}')`,
				eventID, now, session.UserID, session.UserID, session.ID,
				AuthEventStepUpFailed, AuthEventReasonInvalidCredentials, userAgent, sourceHash,
			); err != nil {
				return fmt.Errorf("record rejected security step-up: %w", err)
			}
			return nil
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE totp_credentials
			SET last_accepted_step = ?, last_used_at = ?
			WHERE id = ? AND user_id = ? AND enabled = 1 AND revoked_at IS NULL
			  AND (last_accepted_step IS NULL OR last_accepted_step < ?)`,
			matchedStep, now, credential.id, session.UserID, matchedStep,
		)
		if err != nil {
			return fmt.Errorf("advance security step-up replay state: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrSecuritySessionInvalid
		}
		result, err = tx.ExecContext(ctx, `
			UPDATE sessions SET step_up_at = ?, step_up_method = ?
			WHERE id = ? AND user_id = ? AND token_hash = ? AND revoked_at IS NULL`,
			now, AuthenticationMethodTOTP, session.ID, session.UserID, hashToken(sessionToken),
		)
		if err != nil {
			return fmt.Errorf("record security settings step-up: %w", err)
		}
		changed, err = result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrSecuritySessionInvalid
		}
		identifierHash, err := m.authenticationThrottleBucketHash(
			totpManagementThrottleAction, loginThrottleBucketIdentifier, session.UserID,
		)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM auth_throttle WHERE bucket_hash = ? AND action = ?`,
			identifierHash, totpManagementThrottleAction,
		); err != nil {
			return fmt.Errorf("clear security step-up throttle: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, source_hash, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, '{"factor":"totp","scope":"security_settings"}')`,
			eventID, now, session.UserID, session.UserID, session.ID,
			AuthEventStepUpSucceeded, AuthEventReasonChallengeVerified, userAgent, sourceHash,
		); err != nil {
			return fmt.Errorf("record successful security step-up: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if invalid {
		if retryAt.After(now) {
			return m.loginThrottleError(newLoginThrottleDecision(now, retryAt))
		}
		return &TOTPManagementValidationError{}
	}
	return nil
}

func (m *Manager) StartTOTPReplacement(ctx context.Context, sessionToken, origin string) (*TOTPManagementState, error) {
	session, err := m.GetSessionByToken(ctx, sessionToken)
	if err != nil || session == nil {
		return nil, ErrSecuritySessionInvalid
	}
	now := m.clock.Now().UTC()
	if err := m.requireRecentSecurityStepUp(ctx, session, now); err != nil {
		return nil, err
	}
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil {
		return nil, ErrSecurityChallengeInvalid
	}
	var originalTOTP, accountName string
	err = m.db.Read().QueryRowContext(ctx, `
		SELECT t.id, COALESCE(NULLIF(u.email_normalized, ''), NULLIF(u.username_normalized, ''), u.id)
		FROM users u JOIN totp_credentials t ON t.user_id = u.id
		WHERE u.id = ? AND u.status = 'active' AND u.auth_version = ?
		  AND t.enabled = 1 AND t.revoked_at IS NULL`,
		session.UserID, session.AuthVersion,
	).Scan(&originalTOTP, &accountName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTOTPManagementUnavailable
	}
	if err != nil {
		return nil, fmt.Errorf("load replaceable TOTP credential: %w", err)
	}
	material, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate replacement TOTP material: %w", err)
	}
	key, err := newTOTPKey(accountName, material)
	if err != nil {
		return nil, err
	}
	challengeID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate TOTP replacement challenge ID: %w", err)
	}
	challengeToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate TOTP replacement challenge token: %w", err)
	}
	newTOTP, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate replacement TOTP credential ID: %w", err)
	}
	challenge := &PreAuthChallenge{
		ID: challengeID, Token: challengeToken, UserID: session.UserID, SessionID: session.ID,
		Purpose: ChallengePurposeEnrollment, Origin: canonicalOrigin,
		MaxAttempts: securityManagementMaxAttempts, CreatedAt: now, ExpiresAt: now.Add(securityManagementLifetime),
	}
	draft := &securityManagementDraft{
		Kind: securityManagementKindTOTP, AuthVersion: session.AuthVersion,
		OriginalTOTP: originalTOTP, NewTOTP: newTOTP, TOTPSecret: key.Secret(),
	}
	err = m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		current, err := currentSecuritySession(ctx, tx, sessionToken, now, true)
		if err != nil || current.ID != session.ID || current.AuthVersion != draft.AuthVersion {
			return ErrSecuritySessionInvalid
		}
		credential, err := loadActiveTOTPCredential(ctx, tx, current.UserID)
		if err != nil {
			return err
		}
		if credential.id != draft.OriginalTOTP {
			return ErrSecurityChallengeInvalid
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET consumed_at = COALESCE(consumed_at, ?)
			WHERE user_id = ? AND session_id = ? AND purpose = ? AND consumed_at IS NULL`,
			now, current.UserID, current.ID, ChallengePurposeEnrollment,
		); err != nil {
			return fmt.Errorf("terminate prior TOTP replacement: %w", err)
		}
		payload, err := m.encryptSecurityManagementDraft(challenge, draft)
		if err != nil {
			return err
		}
		challenge.PayloadCiphertext = payload
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_challenges (
				id, user_id, session_id, challenge_hash, purpose, origin, attempts,
				max_attempts, payload_ciphertext, created_at, expires_at
			) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
			challenge.ID, challenge.UserID, challenge.SessionID, hashToken(challenge.Token),
			challenge.Purpose, challenge.Origin, challenge.MaxAttempts,
			challenge.PayloadCiphertext, challenge.CreatedAt, challenge.ExpiresAt,
		); err != nil {
			return fmt.Errorf("insert TOTP replacement challenge: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	enrollment, err := setupTOTPEnrollment(accountName, draft.TOTPSecret, false)
	if err != nil {
		return nil, err
	}
	return &TOTPManagementState{Challenge: challenge, Enrollment: enrollment}, nil
}

func (m *Manager) GetTOTPReplacement(ctx context.Context, challengeToken, sessionToken, origin string) (*TOTPManagementState, error) {
	challenge, draft, accountName, _, err := m.readSecurityManagementDraft(
		ctx, challengeToken, sessionToken, origin, ChallengePurposeEnrollment, securityManagementKindTOTP,
	)
	if err != nil {
		return nil, err
	}
	enrollment, err := setupTOTPEnrollment(accountName, draft.TOTPSecret, false)
	if err != nil {
		return nil, err
	}
	return &TOTPManagementState{Challenge: challenge, Enrollment: enrollment}, nil
}

func (m *Manager) ConfirmTOTPReplacement(ctx context.Context, challengeToken, sessionToken, origin, code, source, userAgent string) (*Session, error) {
	challenge, draft, _, current, err := m.readSecurityManagementDraft(
		ctx, challengeToken, sessionToken, origin, ChallengePurposeEnrollment, securityManagementKindTOTP,
	)
	if err != nil {
		return nil, err
	}
	buckets, err := m.totpManagementThrottleBuckets(challenge.UserID, source)
	if err != nil {
		return nil, err
	}
	decision, err := m.checkLoginThrottleBuckets(ctx, buckets)
	if err != nil {
		return nil, err
	}
	if decision.Throttled {
		return nil, m.loginThrottleError(decision)
	}
	now := m.clock.Now().UTC()
	matchedStep, valid, err := matchTOTPCode(draft.TOTPSecret, code, now)
	if err != nil {
		return nil, err
	}
	encryptedSeed, err := m.encryptTOTPSeed(challenge.UserID, draft.NewTOTP, draft.TOTPSecret)
	if err != nil {
		return nil, err
	}
	newSessionID, newSessionToken, eventID, err := m.securityRotationMaterial()
	if err != nil {
		return nil, err
	}
	sourceHash, err := m.authenticationThrottleBucketHash(
		totpManagementThrottleAction, loginThrottleBucketSource, strings.TrimSpace(source),
	)
	if err != nil {
		return nil, err
	}
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil {
		return nil, ErrSecurityChallengeInvalid
	}
	invalid := false
	terminal := false
	retryAt := time.Time{}
	userAgent = boundedUserAgent(userAgent)
	var rotated *Session
	err = m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		currentChallenge, currentDraft, _, session, err := m.currentSecurityManagementDraft(
			ctx, tx, challengeToken, sessionToken, canonicalOrigin, ChallengePurposeEnrollment,
			securityManagementKindTOTP, now, true,
		)
		if err != nil || !sameSecurityManagementDraft(currentDraft, draft) || session.ID != current.ID {
			return ErrSecurityChallengeInvalid
		}
		if !valid {
			invalid = true
			var consumedAt sql.NullTime
			if err := tx.QueryRowContext(ctx, `
				UPDATE auth_challenges
				SET attempts = attempts + 1,
				    consumed_at = CASE WHEN attempts + 1 >= max_attempts THEN ? ELSE NULL END
				WHERE id = ? AND challenge_hash = ? AND purpose = ? AND origin = ?
				  AND session_id = ? AND consumed_at IS NULL AND expires_at > ? AND attempts < max_attempts
				RETURNING consumed_at`,
				now, currentChallenge.ID, hashToken(challengeToken), ChallengePurposeEnrollment,
				canonicalOrigin, session.ID, now,
			).Scan(&consumedAt); errors.Is(err, sql.ErrNoRows) {
				return ErrSecurityChallengeInvalid
			} else if err != nil {
				return fmt.Errorf("record rejected TOTP replacement code: %w", err)
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
					id, occurred_at, actor_user_id, subject_user_id, session_id,
					event_type, success, reason, user_agent, source_hash, metadata_json
				) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, '{"action":"replace","factor":"totp"}')`,
				eventID, now, session.UserID, session.UserID, session.ID,
				AuthEventCredentialChanged, AuthEventReasonInvalidCredentials, userAgent, sourceHash,
			); err != nil {
				return fmt.Errorf("record rejected TOTP replacement: %w", err)
			}
			return nil
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE totp_credentials SET enabled = 0, revoked_at = ?
			WHERE id = ? AND user_id = ? AND enabled = 1 AND revoked_at IS NULL`,
			now, currentDraft.OriginalTOTP, session.UserID,
		)
		if err != nil {
			return fmt.Errorf("revoke replaced TOTP credential: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrSecurityChallengeInvalid
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO totp_credentials (
				id, user_id, encrypted_seed, key_version, algorithm, digits, period,
				issuer, last_accepted_step, enabled, created_at, last_used_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			currentDraft.NewTOTP, session.UserID, encryptedSeed, totpCredentialKeyVersion,
			totpAlgorithm, totpDigits, totpPeriodSeconds, totpIssuer, matchedStep, now, now,
		); err != nil {
			return fmt.Errorf("insert replacement TOTP credential: %w", err)
		}
		newAuthVersion, err := advanceSecurityAuthVersion(ctx, tx, session.UserID, session.AuthVersion, now)
		if err != nil {
			return err
		}
		if err := revokeSecuritySessions(ctx, tx, session.UserID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET consumed_at = COALESCE(consumed_at, ?), payload_ciphertext = NULL
			WHERE user_id = ? AND session_id IS NOT NULL AND consumed_at IS NULL`,
			now, session.UserID,
		); err != nil {
			return fmt.Errorf("terminate credential-management challenges: %w", err)
		}
		rotated, err = insertRotatedSecuritySession(
			ctx, tx, session, newAuthVersion, newSessionID, newSessionToken, userAgent,
			AuthenticationMethodTOTP, now,
		)
		if err != nil {
			return err
		}
		identifierHash, err := m.authenticationThrottleBucketHash(
			totpManagementThrottleAction, loginThrottleBucketIdentifier, session.UserID,
		)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM auth_throttle WHERE bucket_hash = ? AND action = ?`,
			identifierHash, totpManagementThrottleAction,
		); err != nil {
			return fmt.Errorf("clear TOTP management throttle: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, source_hash, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, '{"action":"replaced","factor":"totp"}')`,
			eventID, now, session.UserID, session.UserID, rotated.ID,
			AuthEventCredentialChanged, AuthEventReasonChallengeVerified, userAgent, sourceHash,
		); err != nil {
			return fmt.Errorf("record TOTP replacement: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if invalid {
		if retryAt.After(now) {
			return nil, m.loginThrottleError(newLoginThrottleDecision(now, retryAt))
		}
		return nil, &TOTPManagementValidationError{Terminal: terminal}
	}
	return rotated, nil
}

func (m *Manager) DisableTOTP(ctx context.Context, sessionToken, userAgent string) (*Session, error) {
	session, err := m.GetSessionByToken(ctx, sessionToken)
	if err != nil || session == nil {
		return nil, ErrSecuritySessionInvalid
	}
	now := m.clock.Now().UTC()
	if err := m.requireRecentSecurityStepUp(ctx, session, now); err != nil {
		return nil, err
	}
	_, rpID, err := canonicalWebAuthnRelyingParty(m.config.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("validate security settings relying party: %w", err)
	}
	newSessionID, newSessionToken, eventID, err := m.securityRotationMaterial()
	if err != nil {
		return nil, err
	}
	userAgent = boundedUserAgent(userAgent)
	var rotated *Session
	err = m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		current, err := currentSecuritySession(ctx, tx, sessionToken, now, true)
		if err != nil || current.ID != session.ID {
			return ErrSecuritySessionInvalid
		}
		credential, err := loadActiveTOTPCredential(ctx, tx, current.UserID)
		if err != nil {
			return err
		}
		canDisable, err := canDisableTOTPInTransaction(ctx, tx, current.UserID, rpID, m.configuredFederatedLoginAvailability())
		if err != nil {
			return err
		}
		if !canDisable {
			return ErrLastAuthenticator
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE totp_credentials SET enabled = 0, revoked_at = ?
			WHERE id = ? AND user_id = ? AND enabled = 1 AND revoked_at IS NULL`,
			now, credential.id, current.UserID,
		)
		if err != nil {
			return fmt.Errorf("disable TOTP credential: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrTOTPManagementUnavailable
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE recovery_codes SET revoked_at = ?
			WHERE user_id = ? AND used_at IS NULL AND revoked_at IS NULL`,
			now, current.UserID,
		); err != nil {
			return fmt.Errorf("revoke recovery codes after TOTP disable: %w", err)
		}
		newAuthVersion, err := advanceSecurityAuthVersion(ctx, tx, current.UserID, current.AuthVersion, now)
		if err != nil {
			return err
		}
		if err := revokeSecuritySessions(ctx, tx, current.UserID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET consumed_at = COALESCE(consumed_at, ?), payload_ciphertext = NULL
			WHERE user_id = ? AND session_id IS NOT NULL AND consumed_at IS NULL`,
			now, current.UserID,
		); err != nil {
			return fmt.Errorf("terminate disabled-factor challenges: %w", err)
		}
		rotated, err = insertRotatedSecuritySession(
			ctx, tx, current, newAuthVersion, newSessionID, newSessionToken, userAgent,
			AuthenticationMethodTOTP, now,
		)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, '{"action":"disabled","factor":"totp","recovery_codes":"revoked"}')`,
			eventID, now, current.UserID, current.UserID, rotated.ID,
			AuthEventCredentialChanged, AuthEventReasonChallengeVerified, userAgent,
		); err != nil {
			return fmt.Errorf("record TOTP disable: %w", err)
		}
		return nil
	})
	return rotated, err
}

func (m *Manager) StartRecoveryCodeReplacement(ctx context.Context, sessionToken, origin string) (*RecoveryManagementBatch, error) {
	session, err := m.GetSessionByToken(ctx, sessionToken)
	if err != nil || session == nil {
		return nil, ErrSecuritySessionInvalid
	}
	now := m.clock.Now().UTC()
	if err := m.requireRecentSecurityStepUp(ctx, session, now); err != nil {
		return nil, err
	}
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil {
		return nil, ErrSecurityChallengeInvalid
	}
	var hasTOTP int
	if err := m.db.Read().QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM totp_credentials
		WHERE user_id = ? AND enabled = 1 AND revoked_at IS NULL)`, session.UserID,
	).Scan(&hasTOTP); err != nil {
		return nil, fmt.Errorf("check recovery management TOTP: %w", err)
	}
	if hasTOTP != 1 {
		return nil, ErrTOTPManagementUnavailable
	}
	randomMaterial, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate managed recovery-code material: %w", err)
	}
	batch, hashes, err := deriveSetupRecoveryBatch(randomMaterial)
	if err != nil {
		return nil, err
	}
	challengeID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate recovery management challenge ID: %w", err)
	}
	challengeToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate recovery management challenge token: %w", err)
	}
	codeIDs := make([]string, setupRecoveryCodeCount)
	for index := range codeIDs {
		codeIDs[index], err = m.tokens.ID()
		if err != nil {
			return nil, fmt.Errorf("generate managed recovery-code ID %d: %w", index, err)
		}
	}
	challenge := &PreAuthChallenge{
		ID: challengeID, Token: challengeToken, UserID: session.UserID, SessionID: session.ID,
		Purpose: ChallengePurposeRecovery, Origin: canonicalOrigin,
		MaxAttempts: 1, CreatedAt: now, ExpiresAt: now.Add(securityManagementLifetime),
	}
	draft := &securityManagementDraft{
		Kind: securityManagementKindRecovery, AuthVersion: session.AuthVersion,
		RecoveryBatchID: batch.BatchID, RecoveryCodeIDs: codeIDs, RecoveryCodeHashes: hashes,
	}
	err = m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		current, err := currentSecuritySession(ctx, tx, sessionToken, now, true)
		if err != nil || current.ID != session.ID || current.AuthVersion != draft.AuthVersion {
			return ErrSecuritySessionInvalid
		}
		if _, err := loadActiveTOTPCredential(ctx, tx, current.UserID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET consumed_at = COALESCE(consumed_at, ?), payload_ciphertext = NULL
			WHERE user_id = ? AND session_id = ? AND purpose = ? AND consumed_at IS NULL`,
			now, current.UserID, current.ID, ChallengePurposeRecovery,
		); err != nil {
			return fmt.Errorf("terminate prior recovery-code replacement: %w", err)
		}
		payload, err := m.encryptSecurityManagementDraft(challenge, draft)
		if err != nil {
			return err
		}
		challenge.PayloadCiphertext = payload
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_challenges (
				id, user_id, session_id, challenge_hash, purpose, origin, attempts,
				max_attempts, payload_ciphertext, created_at, expires_at
			) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
			challenge.ID, challenge.UserID, challenge.SessionID, hashToken(challenge.Token),
			challenge.Purpose, challenge.Origin, challenge.MaxAttempts,
			challenge.PayloadCiphertext, challenge.CreatedAt, challenge.ExpiresAt,
		); err != nil {
			return fmt.Errorf("insert recovery-code replacement challenge: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &RecoveryManagementBatch{
		RecoveryManagementState: RecoveryManagementState{Challenge: challenge, BatchID: batch.BatchID},
		Codes:                   batch.Codes,
	}, nil
}

func (m *Manager) GetRecoveryCodeReplacement(ctx context.Context, challengeToken, sessionToken, origin string) (*RecoveryManagementState, error) {
	challenge, draft, _, _, err := m.readSecurityManagementDraft(
		ctx, challengeToken, sessionToken, origin, ChallengePurposeRecovery, securityManagementKindRecovery,
	)
	if err != nil {
		return nil, err
	}
	return &RecoveryManagementState{Challenge: challenge, BatchID: draft.RecoveryBatchID}, nil
}

func (m *Manager) CompleteRecoveryCodeReplacement(ctx context.Context, challengeToken, sessionToken, origin, batchID, userAgent string, saved bool) error {
	if !saved {
		return ErrRecoveryManagementRequired
	}
	challenge, draft, _, _, err := m.readSecurityManagementDraft(
		ctx, challengeToken, sessionToken, origin, ChallengePurposeRecovery, securityManagementKindRecovery,
	)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(batchID)), []byte(draft.RecoveryBatchID)) != 1 {
		return ErrSecurityChallengeInvalid
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return fmt.Errorf("generate recovery replacement event ID: %w", err)
	}
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil {
		return ErrSecurityChallengeInvalid
	}
	now := m.clock.Now().UTC()
	userAgent = boundedUserAgent(userAgent)
	return m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		currentChallenge, currentDraft, _, session, err := m.currentSecurityManagementDraft(
			ctx, tx, challengeToken, sessionToken, canonicalOrigin, ChallengePurposeRecovery,
			securityManagementKindRecovery, now, true,
		)
		if err != nil || !sameSecurityManagementDraft(currentDraft, draft) || currentChallenge.ID != challenge.ID {
			return ErrSecurityChallengeInvalid
		}
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(batchID)), []byte(currentDraft.RecoveryBatchID)) != 1 {
			return ErrSecurityChallengeInvalid
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE recovery_codes SET revoked_at = ?
			WHERE user_id = ? AND used_at IS NULL AND revoked_at IS NULL`,
			now, session.UserID,
		); err != nil {
			return fmt.Errorf("revoke prior recovery-code batch: %w", err)
		}
		for index, codeHash := range currentDraft.RecoveryCodeHashes {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO recovery_codes (id, user_id, batch_id, code_hash, created_at)
				VALUES (?, ?, ?, ?, ?)`,
				currentDraft.RecoveryCodeIDs[index], session.UserID,
				currentDraft.RecoveryBatchID, codeHash, now,
			); err != nil {
				return fmt.Errorf("insert managed recovery code %d: %w", index, err)
			}
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET attempts = attempts + 1, consumed_at = ?, payload_ciphertext = NULL
			WHERE id = ? AND challenge_hash = ? AND purpose = ? AND origin = ?
			  AND user_id = ? AND session_id = ? AND consumed_at IS NULL
			  AND expires_at > ? AND attempts < max_attempts`,
			now, currentChallenge.ID, hashToken(challengeToken), ChallengePurposeRecovery,
			canonicalOrigin, session.UserID, session.ID, now,
		)
		if err != nil {
			return fmt.Errorf("consume recovery-code replacement: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrSecurityChallengeInvalid
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET updated_at = ? WHERE id = ?`, now, session.UserID); err != nil {
			return fmt.Errorf("update recovery-code user timestamp: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, '{"action":"replaced","factor":"recovery_codes"}')`,
			eventID, now, session.UserID, session.UserID, session.ID,
			AuthEventCredentialChanged, AuthEventReasonChallengeVerified, userAgent,
		); err != nil {
			return fmt.Errorf("record recovery-code replacement: %w", err)
		}
		return nil
	})
}

func (m *Manager) RevokeRecoveryCodes(ctx context.Context, sessionToken, userAgent string) (int64, error) {
	session, err := m.GetSessionByToken(ctx, sessionToken)
	if err != nil || session == nil {
		return 0, ErrSecuritySessionInvalid
	}
	now := m.clock.Now().UTC()
	if err := m.requireRecentSecurityStepUp(ctx, session, now); err != nil {
		return 0, err
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return 0, fmt.Errorf("generate recovery revocation event ID: %w", err)
	}
	userAgent = boundedUserAgent(userAgent)
	var revoked int64
	err = m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		current, err := currentSecuritySession(ctx, tx, sessionToken, now, true)
		if err != nil || current.ID != session.ID {
			return ErrSecuritySessionInvalid
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE recovery_codes SET revoked_at = ?
			WHERE user_id = ? AND used_at IS NULL AND revoked_at IS NULL`,
			now, current.UserID,
		)
		if err != nil {
			return fmt.Errorf("revoke managed recovery codes: %w", err)
		}
		revoked, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count revoked managed recovery codes: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET consumed_at = COALESCE(consumed_at, ?), payload_ciphertext = NULL
			WHERE user_id = ? AND session_id = ? AND purpose = ? AND consumed_at IS NULL`,
			now, current.UserID, current.ID, ChallengePurposeRecovery,
		); err != nil {
			return fmt.Errorf("terminate pending recovery-code replacement: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, '{"action":"revoked","factor":"recovery_codes"}')`,
			eventID, now, current.UserID, current.UserID, current.ID,
			AuthEventCredentialChanged, AuthEventReasonChallengeVerified, userAgent,
		); err != nil {
			return fmt.Errorf("record recovery-code revocation: %w", err)
		}
		return nil
	})
	return revoked, err
}

func (m *Manager) CancelSecurityManagement(ctx context.Context, challengeToken, sessionToken, origin string) error {
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(challengeToken) == "" || strings.TrimSpace(sessionToken) == "" {
		return ErrSecurityChallengeInvalid
	}
	now := m.clock.Now().UTC()
	result, err := m.db.Write().ExecContext(ctx, `
		UPDATE auth_challenges SET consumed_at = COALESCE(consumed_at, ?), payload_ciphertext = NULL
		WHERE challenge_hash = ? AND origin = ? AND session_id IN (
			SELECT id FROM sessions WHERE token_hash = ? AND revoked_at IS NULL
		)`,
		now, hashToken(challengeToken), canonicalOrigin, hashToken(sessionToken),
	)
	if err != nil {
		return fmt.Errorf("cancel security management challenge: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrSecurityChallengeInvalid
	}
	return nil
}

func SetSecurityChallengeCookie(w http.ResponseWriter, token string, secure bool, lifetime time.Duration) {
	maxAge := int(lifetime / time.Second)
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name: securityChallengeCookieName, Value: token, Path: "/settings/security",
		MaxAge: maxAge, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

func ClearSecurityChallengeCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: securityChallengeCookieName, Value: "", Path: "/settings/security",
		MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

func GetSecurityChallengeToken(r *http.Request) string {
	cookie, err := r.Cookie(securityChallengeCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func currentSecuritySession(ctx context.Context, tx *sql.Tx, sessionToken string, now time.Time, requireStepUp bool) (*Session, error) {
	session, err := scanSession(tx.QueryRowContext(ctx, sessionSelect+`
		WHERE token_hash = ? AND revoked_at IS NULL
		  AND idle_expires_at > ? AND absolute_expires_at > ?
		  AND EXISTS (
			SELECT 1 FROM users u
			WHERE u.id = sessions.user_id AND u.status = 'active'
			  AND u.auth_version = sessions.auth_version
		  )`, hashToken(strings.TrimSpace(sessionToken)), now, now))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSecuritySessionInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("load current security session: %w", err)
	}
	policy, err := queryAuthenticationPolicy(ctx, tx, session.UserID, session.AuthVersion)
	if errors.Is(err, ErrUserNotActive) {
		return nil, ErrSecuritySessionInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("load current security policy: %w", err)
	}
	if !policy.allowsAssurance(session.AssuranceLevel) {
		return nil, ErrSecuritySessionInvalid
	}
	if requireStepUp && !hasRecentSecurityStepUp(session, policy, now) {
		return nil, ErrRecentStepUpRequired
	}
	return session, nil
}

func loadActiveTOTPCredential(ctx context.Context, tx *sql.Tx, userID string) (*activeTOTPCredential, error) {
	credential := &activeTOTPCredential{}
	err := tx.QueryRowContext(ctx, `
		SELECT id, encrypted_seed, key_version, algorithm, digits, period, last_accepted_step
		FROM totp_credentials
		WHERE user_id = ? AND enabled = 1 AND revoked_at IS NULL`, userID,
	).Scan(
		&credential.id, &credential.encryptedSeed, &credential.keyVersion,
		&credential.algorithm, &credential.digits, &credential.period, &credential.lastAcceptedStep,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTOTPManagementUnavailable
	}
	if err != nil {
		return nil, fmt.Errorf("load active TOTP credential: %w", err)
	}
	if credential.algorithm != totpAlgorithm || credential.digits != totpDigits || credential.period != totpPeriodSeconds {
		return nil, fmt.Errorf("unsupported TOTP credential profile")
	}
	return credential, nil
}

func canDisableTOTPInTransaction(ctx context.Context, tx *sql.Tx, userID, rpID string, availability federatedLoginAvailability) (bool, error) {
	return canRemoveAuthenticatorInTransaction(
		ctx, tx, userID, rpID, availability, authenticatorRemoval{TOTP: true},
	)
}

func (m *Manager) readSecurityManagementDraft(ctx context.Context, challengeToken, sessionToken, origin string, purpose ChallengePurpose, kind string) (*PreAuthChallenge, *securityManagementDraft, string, *Session, error) {
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(challengeToken) == "" || strings.TrimSpace(sessionToken) == "" {
		return nil, nil, "", nil, ErrSecurityChallengeInvalid
	}
	tx, err := m.db.Read().BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("begin security management read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	challenge, draft, accountName, session, err := m.currentSecurityManagementDraft(
		ctx, tx, challengeToken, sessionToken, canonicalOrigin, purpose, kind,
		m.clock.Now().UTC(), true,
	)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, "", nil, fmt.Errorf("commit security management read: %w", err)
	}
	return challenge, draft, accountName, session, nil
}

func (m *Manager) currentSecurityManagementDraft(ctx context.Context, tx *sql.Tx, challengeToken, sessionToken, origin string, purpose ChallengePurpose, kind string, now time.Time, requireStepUp bool) (*PreAuthChallenge, *securityManagementDraft, string, *Session, error) {
	session, err := currentSecuritySession(ctx, tx, sessionToken, now, requireStepUp)
	if err != nil {
		return nil, nil, "", nil, err
	}
	challenge, err := scanPreAuthChallenge(tx.QueryRowContext(ctx, preAuthChallengeSelect+`
		WHERE challenge_hash = ? AND purpose = ? AND origin = ?
		  AND user_id = ? AND session_id = ? AND consumed_at IS NULL
		  AND expires_at > ? AND attempts < max_attempts`,
		hashToken(strings.TrimSpace(challengeToken)), purpose, origin,
		session.UserID, session.ID, now,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, "", nil, ErrSecurityChallengeInvalid
	}
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("load security management challenge: %w", err)
	}
	draft, err := m.decryptSecurityManagementDraft(challenge, challenge.PayloadCiphertext)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if !validSecurityManagementDraft(draft) || draft.Kind != kind || draft.AuthVersion != session.AuthVersion {
		return nil, nil, "", nil, ErrSecurityChallengeInvalid
	}
	if kind == securityManagementKindTOTP {
		credential, err := loadActiveTOTPCredential(ctx, tx, session.UserID)
		if err != nil || credential.id != draft.OriginalTOTP {
			return nil, nil, "", nil, ErrSecurityChallengeInvalid
		}
	}
	var accountName string
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(NULLIF(email_normalized, ''), NULLIF(username_normalized, ''), id)
		FROM users WHERE id = ? AND status = 'active' AND auth_version = ?`,
		session.UserID, session.AuthVersion,
	).Scan(&accountName); err != nil {
		return nil, nil, "", nil, ErrSecurityChallengeInvalid
	}
	return challenge, draft, accountName, session, nil
}

func validSecurityManagementDraft(draft *securityManagementDraft) bool {
	if draft == nil || draft.AuthVersion < 1 {
		return false
	}
	switch draft.Kind {
	case securityManagementKindTOTP:
		return draft.OriginalTOTP != "" && draft.NewTOTP != "" && draft.TOTPSecret != "" &&
			draft.RecoveryBatchID == "" && len(draft.RecoveryCodeIDs) == 0 && len(draft.RecoveryCodeHashes) == 0
	case securityManagementKindRecovery:
		if !isLowerHexHash(draft.RecoveryBatchID) || len(draft.RecoveryCodeIDs) != setupRecoveryCodeCount ||
			len(draft.RecoveryCodeHashes) != setupRecoveryCodeCount || draft.OriginalTOTP != "" ||
			draft.NewTOTP != "" || draft.TOTPSecret != "" {
			return false
		}
		seenIDs := make(map[string]struct{}, len(draft.RecoveryCodeIDs))
		seenHashes := make(map[string]struct{}, len(draft.RecoveryCodeHashes))
		for index := range draft.RecoveryCodeIDs {
			if strings.TrimSpace(draft.RecoveryCodeIDs[index]) == "" || !isLowerHexHash(draft.RecoveryCodeHashes[index]) {
				return false
			}
			if _, exists := seenIDs[draft.RecoveryCodeIDs[index]]; exists {
				return false
			}
			if _, exists := seenHashes[draft.RecoveryCodeHashes[index]]; exists {
				return false
			}
			seenIDs[draft.RecoveryCodeIDs[index]] = struct{}{}
			seenHashes[draft.RecoveryCodeHashes[index]] = struct{}{}
		}
		return true
	default:
		return false
	}
}

func sameSecurityManagementDraft(left, right *securityManagementDraft) bool {
	if !validSecurityManagementDraft(left) || !validSecurityManagementDraft(right) ||
		left.Kind != right.Kind || left.AuthVersion != right.AuthVersion ||
		left.OriginalTOTP != right.OriginalTOTP || left.NewTOTP != right.NewTOTP ||
		left.RecoveryBatchID != right.RecoveryBatchID ||
		len(left.RecoveryCodeIDs) != len(right.RecoveryCodeIDs) ||
		len(left.RecoveryCodeHashes) != len(right.RecoveryCodeHashes) ||
		subtle.ConstantTimeCompare([]byte(left.TOTPSecret), []byte(right.TOTPSecret)) != 1 {
		return false
	}
	for index := range left.RecoveryCodeIDs {
		if left.RecoveryCodeIDs[index] != right.RecoveryCodeIDs[index] ||
			subtle.ConstantTimeCompare([]byte(left.RecoveryCodeHashes[index]), []byte(right.RecoveryCodeHashes[index])) != 1 {
			return false
		}
	}
	return true
}

func (m *Manager) securityManagementAEAD() (cipher.AEAD, error) {
	if len(m.bucketHashKey) < minimumBucketHashKeyBytes {
		return nil, fmt.Errorf("security management encryption key is unavailable")
	}
	deriver := hmac.New(sha256.New, m.bucketHashKey)
	_, _ = deriver.Write([]byte(securityManagementDraftKey))
	block, err := aes.NewCipher(deriver.Sum(nil))
	if err != nil {
		return nil, fmt.Errorf("create security management cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func (m *Manager) encryptSecurityManagementDraft(challenge *PreAuthChallenge, draft *securityManagementDraft) ([]byte, error) {
	if !validSecurityManagementDraft(draft) {
		return nil, fmt.Errorf("security management draft is invalid")
	}
	aead, err := m.securityManagementAEAD()
	if err != nil {
		return nil, err
	}
	plaintext, err := json.Marshal(draft)
	if err != nil {
		return nil, fmt.Errorf("encode security management draft: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate security management nonce: %w", err)
	}
	payload := []byte{securityManagementDraftVersion}
	payload = append(payload, nonce...)
	payload = aead.Seal(payload, nonce, plaintext, securityManagementDraftAAD(challenge))
	return payload, nil
}

func (m *Manager) decryptSecurityManagementDraft(challenge *PreAuthChallenge, payload []byte) (*securityManagementDraft, error) {
	aead, err := m.securityManagementAEAD()
	if err != nil {
		return nil, err
	}
	if len(payload) < 1+aead.NonceSize()+aead.Overhead() || payload[0] != securityManagementDraftVersion {
		return nil, ErrSecurityChallengeInvalid
	}
	nonce := payload[1 : 1+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, payload[1+aead.NonceSize():], securityManagementDraftAAD(challenge))
	if err != nil {
		return nil, ErrSecurityChallengeInvalid
	}
	var draft securityManagementDraft
	if err := json.Unmarshal(plaintext, &draft); err != nil {
		return nil, ErrSecurityChallengeInvalid
	}
	return &draft, nil
}

func securityManagementDraftAAD(challenge *PreAuthChallenge) []byte {
	return []byte(challenge.ID + "\x00" + challenge.UserID + "\x00" + challenge.SessionID + "\x00" + challenge.Origin + "\x00" + string(challenge.Purpose))
}

func (m *Manager) securityRotationMaterial() (string, string, string, error) {
	sessionID, err := m.tokens.ID()
	if err != nil {
		return "", "", "", fmt.Errorf("generate rotated security session ID: %w", err)
	}
	sessionToken, err := m.tokens.Token(32)
	if err != nil {
		return "", "", "", fmt.Errorf("generate rotated security session token: %w", err)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return "", "", "", fmt.Errorf("generate security credential event ID: %w", err)
	}
	return sessionID, sessionToken, eventID, nil
}

func advanceSecurityAuthVersion(ctx context.Context, tx *sql.Tx, userID string, authVersion int64, now time.Time) (int64, error) {
	var next int64
	err := tx.QueryRowContext(ctx, `
		UPDATE users SET auth_version = auth_version + 1, updated_at = ?
		WHERE id = ? AND status = 'active' AND auth_version = ?
		RETURNING auth_version`, now, userID, authVersion,
	).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrSecuritySessionInvalid
	}
	if err != nil {
		return 0, fmt.Errorf("advance security authentication version: %w", err)
	}
	return next, nil
}

func revokeSecuritySessions(ctx context.Context, tx *sql.Tx, userID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
		WHERE user_id = ? AND revoked_at IS NULL`,
		now, userID, SessionRevocationCredentialReset, userID,
	)
	if err != nil {
		return fmt.Errorf("revoke sessions after security credential change: %w", err)
	}
	return nil
}

func insertRotatedSecuritySession(ctx context.Context, tx *sql.Tx, current *Session, authVersion int64, sessionID, sessionToken, userAgent string, stepUpMethod AuthenticationMethod, now time.Time) (*Session, error) {
	idleExpiresAt := now.Add(sessionIdleLifetime)
	absoluteExpiresAt := current.AbsoluteExpiresAt
	if idleExpiresAt.After(absoluteExpiresAt) {
		idleExpiresAt = absoluteExpiresAt
	}
	if !idleExpiresAt.After(now) {
		return nil, ErrSecuritySessionInvalid
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method,
			assurance_level, user_agent, authenticated_at, last_used_at,
			idle_expires_at, absolute_expires_at, step_up_at, step_up_method, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, current.UserID, hashToken(sessionToken), authVersion,
		current.AuthenticationMethod, current.AssuranceLevel, userAgent,
		current.AuthenticatedAt, now, idleExpiresAt, absoluteExpiresAt,
		now, stepUpMethod, now,
	); err != nil {
		return nil, fmt.Errorf("insert rotated security session: %w", err)
	}
	stepUpAt := now
	return &Session{
		ID: sessionID, UserID: current.UserID, Token: sessionToken, AuthVersion: authVersion,
		AuthenticationMethod: current.AuthenticationMethod, AssuranceLevel: current.AssuranceLevel,
		UserAgent: userAgent, AuthenticatedAt: current.AuthenticatedAt, LastUsedAt: now,
		IdleExpiresAt: idleExpiresAt, AbsoluteExpiresAt: absoluteExpiresAt,
		StepUpAt: &stepUpAt, StepUpMethod: stepUpMethod, CreatedAt: now,
	}, nil
}
