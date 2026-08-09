package auth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	passkeyLoginChallengeCookieName  = "gofer_passkey_login"
	passkeyAssertionDraftVersion     = 1
	passkeyAssertionDraftKey         = "gofer/auth/passkey-assertion-draft/v1"
	passkeyAssertionMaxBodyBytes     = 1 << 20
	passkeyAssertionModeIdentifier   = "identifier"
	passkeyAssertionModeDiscoverable = "discoverable"
	passkeyAssertionModeStepUp       = "step_up"
)

var (
	ErrPasskeyAuthenticationInvalid     = errors.New("passkey authentication is invalid or expired")
	ErrPasskeyAuthenticationUnavailable = errors.New("passkey authentication is unavailable")
	ErrPasskeyCloneWarning              = errors.New("passkey counter regression detected")
)

type PasskeyAuthenticationValidationError struct{}

func (*PasskeyAuthenticationValidationError) Error() string {
	return "passkey assertion response is invalid"
}

type PasskeyAuthenticationOptions struct {
	Challenge   *PreAuthChallenge
	RequestJSON json.RawMessage
}

type passkeyAssertionDraft struct {
	Version     int             `json:"version"`
	Mode        string          `json:"mode"`
	AuthVersion int64           `json:"auth_version,omitempty"`
	UserID      string          `json:"user_id,omitempty"`
	SessionID   string          `json:"session_id,omitempty"`
	Identifier  string          `json:"identifier,omitempty"`
	RPID        string          `json:"rp_id"`
	UserHandle  []byte          `json:"user_handle,omitempty"`
	SessionJSON json.RawMessage `json:"session"`
}

type passkeyCredentialSnapshot struct {
	RowID          string
	UserID         string
	RPID           string
	CredentialID   []byte
	PublicKey      []byte
	Ciphertext     []byte
	KeyVersion     int
	SignCount      uint32
	AAGUID         []byte
	TransportsJSON string
	Attachment     string
	BackupEligible bool
	BackupState    bool
	Flags          byte
	CloneWarning   bool
	Credential     webauthn.Credential
}

type passkeyAuthenticationUser struct {
	UserID      string
	AuthVersion int64
	WebAuthn    passkeyUser
	Credentials map[string]*passkeyCredentialSnapshot
}

func (m *Manager) StartPasskeyLogin(ctx context.Context, identifier, origin, source string) (*PasskeyAuthenticationOptions, error) {
	canonicalOrigin, rpID, err := canonicalWebAuthnRelyingParty(origin)
	if err != nil {
		return nil, fmt.Errorf("validate passkey login relying party: %w", err)
	}
	identifier = normalizeLoginIdentifier(identifier)
	buckets, err := m.passkeyLoginThrottleBuckets(identifier, source)
	if err != nil {
		return nil, err
	}
	decision, err := m.checkLoginThrottleBuckets(ctx, buckets)
	if err != nil {
		return nil, fmt.Errorf("check passkey login throttle: %w", err)
	}
	if decision.Throttled {
		return nil, m.loginThrottleError(decision)
	}

	assertion, err := m.passkeyAssertionFactory(canonicalOrigin)
	if err != nil {
		return nil, err
	}
	draft := &passkeyAssertionDraft{
		Version: passkeyAssertionDraftVersion, Identifier: identifier, RPID: rpID,
	}
	var user *passkeyAuthenticationUser
	if identifier == "" {
		draft.Mode = passkeyAssertionModeDiscoverable
	} else {
		draft.Mode = passkeyAssertionModeIdentifier
		userID, err := m.findPasskeyLoginUser(ctx, identifier, rpID)
		if err != nil {
			return nil, err
		}
		if userID == "" {
			return nil, m.rejectPasskeyLoginStart(ctx, buckets)
		}
		user, err = m.loadPasskeyAuthenticationUser(ctx, userID, rpID)
		if err != nil {
			if errors.Is(err, ErrPasskeyAuthenticationUnavailable) {
				return nil, m.rejectPasskeyLoginStart(ctx, buckets)
			}
			return nil, err
		}
		draft.UserID = user.UserID
		draft.AuthVersion = user.AuthVersion
		draft.UserHandle = append([]byte(nil), user.WebAuthn.ID...)
	}

	var webAuthnUser *passkeyUser
	if user != nil {
		webAuthnUser = &user.WebAuthn
	}
	requestJSON, sessionJSON, err := assertion.Begin(webAuthnUser)
	if err != nil {
		return nil, fmt.Errorf("begin passkey login: %w", err)
	}
	if len(requestJSON) == 0 || len(sessionJSON) == 0 || len(sessionJSON) > passkeyRecordMaximumBytes {
		return nil, fmt.Errorf("WebAuthn assertion adapter returned invalid login state")
	}
	draft.SessionJSON = append(json.RawMessage(nil), sessionJSON...)
	return m.createPasskeyAssertionChallenge(ctx, canonicalOrigin, ChallengePurposeLogin, draft, nil, requestJSON)
}

func (m *Manager) StartPasskeyStepUp(ctx context.Context, sessionToken, origin, source string) (*PasskeyAuthenticationOptions, error) {
	session, err := m.GetSessionByToken(ctx, sessionToken)
	if err != nil || session == nil {
		return nil, ErrSecuritySessionInvalid
	}
	session.Token = strings.TrimSpace(sessionToken)
	buckets, err := m.passkeyStepUpThrottleBuckets(session.UserID, source)
	if err != nil {
		return nil, err
	}
	decision, err := m.checkLoginThrottleBuckets(ctx, buckets)
	if err != nil {
		return nil, fmt.Errorf("check passkey step-up throttle: %w", err)
	}
	if decision.Throttled {
		return nil, m.loginThrottleError(decision)
	}
	canonicalOrigin, rpID, err := canonicalWebAuthnRelyingParty(origin)
	if err != nil {
		return nil, fmt.Errorf("validate passkey step-up relying party: %w", err)
	}
	user, err := m.loadPasskeyAuthenticationUser(ctx, session.UserID, rpID)
	if err != nil {
		return nil, err
	}
	if user.AuthVersion != session.AuthVersion {
		return nil, ErrSecuritySessionInvalid
	}
	assertion, err := m.passkeyAssertionFactory(canonicalOrigin)
	if err != nil {
		return nil, err
	}
	requestJSON, sessionJSON, err := assertion.Begin(&user.WebAuthn)
	if err != nil {
		return nil, fmt.Errorf("begin passkey step-up: %w", err)
	}
	if len(requestJSON) == 0 || len(sessionJSON) == 0 || len(sessionJSON) > passkeyRecordMaximumBytes {
		return nil, fmt.Errorf("WebAuthn assertion adapter returned invalid step-up state")
	}
	draft := &passkeyAssertionDraft{
		Version: passkeyAssertionDraftVersion, Mode: passkeyAssertionModeStepUp,
		AuthVersion: session.AuthVersion, UserID: session.UserID, SessionID: session.ID,
		RPID: rpID, UserHandle: append([]byte(nil), user.WebAuthn.ID...),
		SessionJSON: append(json.RawMessage(nil), sessionJSON...),
	}
	return m.createPasskeyAssertionChallenge(ctx, canonicalOrigin, ChallengePurposeStepUp, draft, session, requestJSON)
}

func (m *Manager) createPasskeyAssertionChallenge(ctx context.Context, origin string, purpose ChallengePurpose, draft *passkeyAssertionDraft, session *Session, requestJSON []byte) (*PasskeyAuthenticationOptions, error) {
	challengeID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate passkey assertion challenge ID: %w", err)
	}
	challengeToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate passkey assertion challenge token: %w", err)
	}
	now := m.clock.Now().UTC()
	challenge := &PreAuthChallenge{
		ID: challengeID, Token: challengeToken, UserID: draft.UserID, SessionID: draft.SessionID,
		Purpose: purpose, Origin: origin, MaxAttempts: 1,
		CreatedAt: now, ExpiresAt: now.Add(passkeyCeremonyLifetime),
	}
	err = m.runSecurityTransition(ctx, SecurityTransitionLoginCompletion, func(tx *sql.Tx) error {
		if session != nil {
			current, err := currentSecuritySession(ctx, tx, session.Token, now, false)
			if err != nil || current.ID != session.ID || current.UserID != draft.UserID || current.AuthVersion != draft.AuthVersion {
				return ErrSecuritySessionInvalid
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE auth_challenges SET consumed_at = COALESCE(consumed_at, ?), payload_ciphertext = NULL
				WHERE user_id = ? AND session_id = ? AND purpose = ? AND consumed_at IS NULL`,
				now, current.UserID, current.ID, purpose,
			); err != nil {
				return fmt.Errorf("terminate prior passkey assertion challenge: %w", err)
			}
		}
		payload, err := m.encryptPasskeyAssertionDraft(challenge, draft)
		if err != nil {
			return err
		}
		challenge.PayloadCiphertext = payload
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_challenges (
				id, user_id, session_id, challenge_hash, purpose, origin, attempts,
				max_attempts, payload_ciphertext, created_at, expires_at
			) VALUES (?, ?, ?, ?, ?, ?, 0, 1, ?, ?, ?)`,
			challenge.ID, nullableIdentifier(challenge.UserID), nullableIdentifier(challenge.SessionID),
			hashToken(challenge.Token), challenge.Purpose, challenge.Origin,
			challenge.PayloadCiphertext, challenge.CreatedAt, challenge.ExpiresAt,
		); err != nil {
			return fmt.Errorf("insert passkey assertion challenge: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &PasskeyAuthenticationOptions{Challenge: challenge, RequestJSON: append(json.RawMessage(nil), requestJSON...)}, nil
}

func (m *Manager) FinishPasskeyLogin(ctx context.Context, challengeToken, origin string, responseJSON []byte, source, userAgent string) (*Session, error) {
	invalidBody := len(responseJSON) == 0 || len(responseJSON) > passkeyAssertionMaxBodyBytes
	canonicalOrigin, rpID, err := canonicalWebAuthnRelyingParty(origin)
	if err != nil {
		return nil, ErrPasskeyAuthenticationInvalid
	}
	challenge, draft, err := m.readPasskeyAssertionDraft(ctx, challengeToken, "", canonicalOrigin, ChallengePurposeLogin)
	if err != nil {
		return nil, err
	}
	if draft.RPID != rpID || (draft.Mode != passkeyAssertionModeIdentifier && draft.Mode != passkeyAssertionModeDiscoverable) {
		return nil, ErrPasskeyAuthenticationInvalid
	}
	if invalidBody {
		return nil, m.rejectPasskeyAssertion(ctx, challenge, draft, challengeToken, "", canonicalOrigin, source, userAgent, nil, &PasskeyAuthenticationValidationError{})
	}
	assertion, err := m.passkeyAssertionFactory(canonicalOrigin)
	if err != nil {
		return nil, err
	}
	var user *passkeyAuthenticationUser
	var record *passkeyCredentialRecord
	if draft.Mode == passkeyAssertionModeIdentifier {
		user, err = m.loadPasskeyAuthenticationUser(ctx, draft.UserID, rpID)
		if err != nil && !errors.Is(err, ErrPasskeyAuthenticationUnavailable) {
			return nil, err
		}
		if err == nil && (user.AuthVersion != draft.AuthVersion || !bytes.Equal(user.WebAuthn.ID, draft.UserHandle)) {
			err = ErrPasskeyAuthenticationInvalid
		}
		if err == nil {
			record, err = assertion.Finish(user.WebAuthn, draft.SessionJSON, responseJSON)
		}
	} else {
		var lookupFailure error
		record, err = assertion.FinishDiscoverable(func(rawID, userHandle []byte) (passkeyUser, error) {
			resolved, lookupErr := m.loadDiscoverablePasskeyUser(ctx, rpID, rawID, userHandle)
			if lookupErr != nil {
				lookupFailure = lookupErr
				return passkeyUser{}, lookupErr
			}
			user = resolved
			return resolved.WebAuthn, nil
		}, draft.SessionJSON, responseJSON)
		if lookupFailure != nil && !errors.Is(lookupFailure, ErrPasskeyAuthenticationUnavailable) {
			return nil, lookupFailure
		}
	}
	if err != nil || user == nil {
		return nil, m.rejectPasskeyAssertion(ctx, challenge, draft, challengeToken, "", canonicalOrigin, source, userAgent, user, err)
	}
	snapshot, err := validatePreparedPasskeyAssertion(user, record)
	if err != nil {
		return nil, m.rejectPasskeyAssertion(ctx, challenge, draft, challengeToken, "", canonicalOrigin, source, userAgent, user, err)
	}
	return m.completePasskeyLogin(ctx, challenge, draft, challengeToken, canonicalOrigin, source, userAgent, user, snapshot, record)
}

func (m *Manager) FinishPasskeyStepUp(ctx context.Context, challengeToken, sessionToken, origin string, responseJSON []byte, source, userAgent string) error {
	invalidBody := len(responseJSON) == 0 || len(responseJSON) > passkeyAssertionMaxBodyBytes
	canonicalOrigin, rpID, err := canonicalWebAuthnRelyingParty(origin)
	if err != nil {
		return ErrPasskeyAuthenticationInvalid
	}
	challenge, draft, err := m.readPasskeyAssertionDraft(ctx, challengeToken, sessionToken, canonicalOrigin, ChallengePurposeStepUp)
	if err != nil {
		return err
	}
	if draft.Mode != passkeyAssertionModeStepUp || draft.RPID != rpID {
		return ErrPasskeyAuthenticationInvalid
	}
	if invalidBody {
		return m.rejectPasskeyAssertion(ctx, challenge, draft, challengeToken, sessionToken, canonicalOrigin, source, userAgent, nil, &PasskeyAuthenticationValidationError{})
	}
	user, err := m.loadPasskeyAuthenticationUser(ctx, draft.UserID, rpID)
	if err != nil && !errors.Is(err, ErrPasskeyAuthenticationUnavailable) {
		return err
	}
	if err != nil || user.AuthVersion != draft.AuthVersion || !bytes.Equal(user.WebAuthn.ID, draft.UserHandle) {
		if err == nil {
			err = ErrPasskeyAuthenticationInvalid
		}
		return m.rejectPasskeyAssertion(ctx, challenge, draft, challengeToken, sessionToken, canonicalOrigin, source, userAgent, user, err)
	}
	assertion, err := m.passkeyAssertionFactory(canonicalOrigin)
	if err != nil {
		return err
	}
	record, err := assertion.Finish(user.WebAuthn, draft.SessionJSON, responseJSON)
	if err != nil {
		return m.rejectPasskeyAssertion(ctx, challenge, draft, challengeToken, sessionToken, canonicalOrigin, source, userAgent, user, err)
	}
	snapshot, err := validatePreparedPasskeyAssertion(user, record)
	if err != nil {
		return m.rejectPasskeyAssertion(ctx, challenge, draft, challengeToken, sessionToken, canonicalOrigin, source, userAgent, user, err)
	}
	return m.completePasskeyStepUp(ctx, challenge, draft, challengeToken, sessionToken, canonicalOrigin, source, userAgent, user, snapshot, record)
}

func (m *Manager) completePasskeyLogin(ctx context.Context, challenge *PreAuthChallenge, draft *passkeyAssertionDraft, challengeToken, origin, source, userAgent string, user *passkeyAuthenticationUser, snapshot *passkeyCredentialSnapshot, record *passkeyCredentialRecord) (*Session, error) {
	sessionID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate passkey session ID: %w", err)
	}
	sessionToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate passkey session token: %w", err)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate passkey login event ID: %w", err)
	}
	ciphertext, err := m.encryptPasskeyCredential(user.UserID, snapshot.RowID, record.Record)
	if err != nil {
		return nil, err
	}
	now := m.clock.Now().UTC()
	userAgent = boundedUserAgent(userAgent)
	buckets, err := m.passkeyLoginThrottleBuckets(draft.Identifier, source)
	if err != nil {
		return nil, err
	}
	sourceHash, err := m.authenticationThrottleBucketHash(passkeyLoginThrottleAction, loginThrottleBucketSource, strings.TrimSpace(source))
	if err != nil {
		return nil, err
	}
	session := &Session{
		ID: sessionID, UserID: user.UserID, Token: sessionToken, AuthVersion: user.AuthVersion,
		AuthenticationMethod: AuthenticationMethodPasskey, AssuranceLevel: AssuranceLevelPhishingResistant,
		UserAgent: userAgent, AuthenticatedAt: now, LastUsedAt: now,
		IdleExpiresAt: now.Add(sessionIdleLifetime), AbsoluteExpiresAt: now.Add(sessionAbsoluteLifetime),
		StepUpAt: &now, StepUpMethod: AuthenticationMethodPasskey, CreatedAt: now,
	}
	if session.IdleExpiresAt.After(session.AbsoluteExpiresAt) {
		session.IdleExpiresAt = session.AbsoluteExpiresAt
	}
	cloneWarning := false
	retryAt := time.Time{}
	err = m.runSecurityTransition(ctx, SecurityTransitionLoginCompletion, func(tx *sql.Tx) error {
		currentChallenge, currentDraft, err := m.currentPasskeyAssertionDraft(ctx, tx, challengeToken, "", origin, ChallengePurposeLogin, now)
		if err != nil || currentChallenge.ID != challenge.ID || !samePasskeyAssertionDraft(currentDraft, draft) {
			return ErrPasskeyAuthenticationInvalid
		}
		if err := currentPasskeyCredentialMatches(ctx, tx, snapshot); err != nil {
			return err
		}
		var status UserStatus
		var authVersion int64
		var mfaRequired, isAdmin int
		if err := tx.QueryRowContext(ctx, `SELECT status, auth_version, mfa_required, is_admin FROM users WHERE id = ?`, user.UserID).Scan(&status, &authVersion, &mfaRequired, &isAdmin); err != nil || !status.AllowsAuthentication() || authVersion != user.AuthVersion {
			return ErrPasskeyAuthenticationInvalid
		}
		policy := resolveAuthenticationPolicy(authVersion, mfaRequired == 1, isAdmin == 1)
		if !policy.allowsAssurance(session.AssuranceLevel) {
			return ErrAuthenticationPolicyNotSatisfied
		}
		if record.CloneWarning {
			cloneWarning = true
			if err := updatePasskeyCredentialAssertion(ctx, tx, snapshot, record, ciphertext, now, false); err != nil {
				return err
			}
			for _, bucket := range buckets {
				blockedUntil, err := recordLoginThrottleFailure(ctx, tx, bucket, now)
				if err != nil {
					return err
				}
				if blockedUntil.After(retryAt) {
					retryAt = blockedUntil
				}
			}
			if err := consumePasskeyAssertionChallenge(ctx, tx, currentChallenge, challengeToken, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO auth_events (
					id, occurred_at, actor_user_id, subject_user_id, event_type, success,
					reason, user_agent, source_hash, metadata_json
				) VALUES (?, ?, NULL, ?, ?, 0, ?, ?, ?, '{"factor":"passkey","result":"counter_regression"}')`,
				eventID, now, user.UserID, AuthEventLoginFailed, AuthEventReasonPolicyRequired, userAgent, sourceHash,
			); err != nil {
				return fmt.Errorf("record passkey counter regression: %w", err)
			}
			return nil
		}
		if err := updatePasskeyCredentialAssertion(ctx, tx, snapshot, record, ciphertext, now, true); err != nil {
			return err
		}
		if err := consumePasskeyAssertionChallenge(ctx, tx, currentChallenge, challengeToken, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE users SET last_login_at = ?, updated_at = ?
			WHERE id = ? AND status = 'active' AND auth_version = ?`, now, now, user.UserID, user.AuthVersion,
		); err != nil {
			return fmt.Errorf("update passkey login timestamp: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (
				id, user_id, token_hash, auth_version, authentication_method, assurance_level,
				user_agent, authenticated_at, last_used_at, idle_expires_at, absolute_expires_at,
				step_up_at, step_up_method, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			session.ID, session.UserID, hashToken(session.Token), session.AuthVersion,
			session.AuthenticationMethod, session.AssuranceLevel, session.UserAgent,
			session.AuthenticatedAt, session.LastUsedAt, session.IdleExpiresAt, session.AbsoluteExpiresAt,
			session.StepUpAt, session.StepUpMethod, session.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert passkey session: %w", err)
		}
		if draft.Identifier != "" {
			identifierHash, err := m.authenticationThrottleBucketHash(passkeyLoginThrottleAction, loginThrottleBucketIdentifier, draft.Identifier)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM auth_throttle WHERE bucket_hash = ? AND action = ?`, identifierHash, passkeyLoginThrottleAction); err != nil {
				return fmt.Errorf("clear passkey login throttle: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
				success, reason, user_agent, source_hash, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, '{"factor":"passkey","flow":"login"}')`,
			eventID, now, user.UserID, user.UserID, session.ID, AuthEventLoginSucceeded,
			AuthEventReasonChallengeVerified, userAgent, sourceHash,
		); err != nil {
			return fmt.Errorf("record successful passkey login: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if cloneWarning {
		if retryAt.After(now) {
			return nil, m.loginThrottleError(newLoginThrottleDecision(now, retryAt))
		}
		return nil, ErrPasskeyCloneWarning
	}
	return session, nil
}

func (m *Manager) completePasskeyStepUp(ctx context.Context, challenge *PreAuthChallenge, draft *passkeyAssertionDraft, challengeToken, sessionToken, origin, source, userAgent string, user *passkeyAuthenticationUser, snapshot *passkeyCredentialSnapshot, record *passkeyCredentialRecord) error {
	eventID, err := m.tokens.ID()
	if err != nil {
		return fmt.Errorf("generate passkey step-up event ID: %w", err)
	}
	ciphertext, err := m.encryptPasskeyCredential(user.UserID, snapshot.RowID, record.Record)
	if err != nil {
		return err
	}
	now := m.clock.Now().UTC()
	userAgent = boundedUserAgent(userAgent)
	buckets, err := m.passkeyStepUpThrottleBuckets(user.UserID, source)
	if err != nil {
		return err
	}
	sourceHash, err := m.authenticationThrottleBucketHash(passkeyStepUpThrottleAction, loginThrottleBucketSource, strings.TrimSpace(source))
	if err != nil {
		return err
	}
	cloneWarning := false
	retryAt := time.Time{}
	err = m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		currentChallenge, currentDraft, err := m.currentPasskeyAssertionDraft(ctx, tx, challengeToken, sessionToken, origin, ChallengePurposeStepUp, now)
		if err != nil || currentChallenge.ID != challenge.ID || !samePasskeyAssertionDraft(currentDraft, draft) {
			return ErrPasskeyAuthenticationInvalid
		}
		currentSession, err := currentSecuritySession(ctx, tx, sessionToken, now, false)
		if err != nil || currentSession.ID != draft.SessionID || currentSession.UserID != user.UserID || currentSession.AuthVersion != user.AuthVersion {
			return ErrSecuritySessionInvalid
		}
		if err := currentPasskeyCredentialMatches(ctx, tx, snapshot); err != nil {
			return err
		}
		if record.CloneWarning {
			cloneWarning = true
			if err := updatePasskeyCredentialAssertion(ctx, tx, snapshot, record, ciphertext, now, false); err != nil {
				return err
			}
			for _, bucket := range buckets {
				blockedUntil, err := recordLoginThrottleFailure(ctx, tx, bucket, now)
				if err != nil {
					return err
				}
				if blockedUntil.After(retryAt) {
					retryAt = blockedUntil
				}
			}
			if err := consumePasskeyAssertionChallenge(ctx, tx, currentChallenge, challengeToken, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO auth_events (
					id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
					success, reason, user_agent, source_hash, metadata_json
				) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, '{"factor":"passkey","result":"counter_regression","scope":"security_settings"}')`,
				eventID, now, user.UserID, user.UserID, currentSession.ID, AuthEventStepUpFailed,
				AuthEventReasonPolicyRequired, userAgent, sourceHash,
			); err != nil {
				return fmt.Errorf("record passkey step-up counter regression: %w", err)
			}
			return nil
		}
		if err := updatePasskeyCredentialAssertion(ctx, tx, snapshot, record, ciphertext, now, true); err != nil {
			return err
		}
		if err := consumePasskeyAssertionChallenge(ctx, tx, currentChallenge, challengeToken, now); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE sessions SET step_up_at = ?, step_up_method = ?
			WHERE id = ? AND user_id = ? AND token_hash = ? AND revoked_at IS NULL`,
			now, AuthenticationMethodPasskey, currentSession.ID, currentSession.UserID, hashToken(sessionToken),
		)
		if err != nil {
			return fmt.Errorf("record passkey step-up state: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrSecuritySessionInvalid
		}
		identifierHash, err := m.authenticationThrottleBucketHash(passkeyStepUpThrottleAction, loginThrottleBucketIdentifier, user.UserID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM auth_throttle WHERE bucket_hash = ? AND action = ?`, identifierHash, passkeyStepUpThrottleAction); err != nil {
			return fmt.Errorf("clear passkey step-up throttle: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
				success, reason, user_agent, source_hash, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, '{"factor":"passkey","scope":"security_settings"}')`,
			eventID, now, user.UserID, user.UserID, currentSession.ID, AuthEventStepUpSucceeded,
			AuthEventReasonChallengeVerified, userAgent, sourceHash,
		); err != nil {
			return fmt.Errorf("record successful passkey step-up: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if cloneWarning {
		if retryAt.After(now) {
			return m.loginThrottleError(newLoginThrottleDecision(now, retryAt))
		}
		return ErrPasskeyCloneWarning
	}
	return nil
}

func (m *Manager) rejectPasskeyLoginStart(ctx context.Context, buckets []loginThrottleBucket) error {
	decision, err := m.recordLoginThrottleBuckets(ctx, buckets)
	if err != nil {
		return fmt.Errorf("record unavailable passkey login: %w", err)
	}
	if decision.Throttled {
		return m.loginThrottleError(decision)
	}
	return ErrPasskeyAuthenticationUnavailable
}

func (m *Manager) rejectPasskeyAssertion(ctx context.Context, challenge *PreAuthChallenge, draft *passkeyAssertionDraft, challengeToken, sessionToken, origin, source, userAgent string, user *passkeyAuthenticationUser, validationErr error) error {
	eventID, err := m.tokens.ID()
	if err != nil {
		return fmt.Errorf("generate rejected passkey assertion event ID: %w", err)
	}
	now := m.clock.Now().UTC()
	userAgent = boundedUserAgent(userAgent)
	userID := draft.UserID
	if user != nil {
		userID = user.UserID
	}
	var buckets []loginThrottleBucket
	var action string
	var eventType AuthEventType
	if challenge.Purpose == ChallengePurposeStepUp {
		buckets, err = m.passkeyStepUpThrottleBuckets(userID, source)
		action = passkeyStepUpThrottleAction
		eventType = AuthEventStepUpFailed
	} else {
		identifier := draft.Identifier
		if identifier == "" && userID != "" {
			identifier = userID
		}
		buckets, err = m.passkeyLoginThrottleBuckets(identifier, source)
		action = passkeyLoginThrottleAction
		eventType = AuthEventLoginFailed
	}
	if err != nil {
		return err
	}
	sourceHash, err := m.authenticationThrottleBucketHash(action, loginThrottleBucketSource, strings.TrimSpace(source))
	if err != nil {
		return err
	}
	retryAt := time.Time{}
	actorUserID := ""
	if challenge.Purpose == ChallengePurposeStepUp {
		actorUserID = userID
	}
	err = m.runSecurityTransition(ctx, SecurityTransitionLoginCompletion, func(tx *sql.Tx) error {
		currentChallenge, currentDraft, err := m.currentPasskeyAssertionDraft(ctx, tx, challengeToken, sessionToken, origin, challenge.Purpose, now)
		if err != nil || currentChallenge.ID != challenge.ID || !samePasskeyAssertionDraft(currentDraft, draft) {
			return ErrPasskeyAuthenticationInvalid
		}
		for _, bucket := range buckets {
			blockedUntil, err := recordLoginThrottleFailure(ctx, tx, bucket, now)
			if err != nil {
				return err
			}
			if blockedUntil.After(retryAt) {
				retryAt = blockedUntil
			}
		}
		if err := consumePasskeyAssertionChallenge(ctx, tx, currentChallenge, challengeToken, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
				success, reason, user_agent, source_hash, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, '{"factor":"passkey","result":"invalid_assertion"}')`,
			eventID, now, nullableIdentifier(actorUserID), nullableIdentifier(userID), nullableIdentifier(draft.SessionID),
			eventType, AuthEventReasonInvalidCredentials, userAgent, sourceHash,
		); err != nil {
			return fmt.Errorf("record rejected passkey assertion: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if retryAt.After(now) {
		return m.loginThrottleError(newLoginThrottleDecision(now, retryAt))
	}
	if errors.Is(validationErr, ErrPasskeyAuthenticationInvalid) {
		return ErrPasskeyAuthenticationInvalid
	}
	return &PasskeyAuthenticationValidationError{}
}

func (m *Manager) findPasskeyLoginUser(ctx context.Context, identifier, rpID string) (string, error) {
	rows, err := m.db.Read().QueryContext(ctx, `
		SELECT u.id
		FROM users u
		WHERE (u.email_normalized = ? OR u.username_normalized = ?)
		  AND u.status = 'active'
		  AND EXISTS (
			SELECT 1 FROM webauthn_users wu
			WHERE wu.user_id = u.id AND wu.rp_id = ?
		  )
		  AND EXISTS (
			SELECT 1 FROM webauthn_credentials wc
			WHERE wc.user_id = u.id AND wc.rp_id = ? AND wc.revoked_at IS NULL
			  AND wc.credential_ciphertext IS NOT NULL AND wc.key_version IS NOT NULL
		  )
		ORDER BY u.id LIMIT 2`, identifier, identifier, rpID, rpID)
	if err != nil {
		return "", fmt.Errorf("lookup passkey login user: %w", err)
	}
	defer rows.Close()
	var users []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return "", fmt.Errorf("scan passkey login user: %w", err)
		}
		users = append(users, userID)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate passkey login users: %w", err)
	}
	if len(users) != 1 {
		return "", nil
	}
	return users[0], nil
}

func (m *Manager) loadDiscoverablePasskeyUser(ctx context.Context, rpID string, credentialID, userHandle []byte) (*passkeyAuthenticationUser, error) {
	if len(credentialID) == 0 || len(userHandle) < 16 || len(userHandle) > 64 {
		return nil, ErrPasskeyAuthenticationUnavailable
	}
	var userID string
	err := m.db.Read().QueryRowContext(ctx, `
		SELECT wu.user_id
		FROM webauthn_users wu
		JOIN users u ON u.id = wu.user_id
		JOIN webauthn_credentials wc ON wc.user_id = wu.user_id
		WHERE wu.rp_id = ? AND wu.user_handle = ?
		  AND wc.credential_id = ? AND wc.rp_id = wu.rp_id
		  AND wc.revoked_at IS NULL AND wc.credential_ciphertext IS NOT NULL
		  AND wc.key_version IS NOT NULL AND u.status = 'active'`, rpID, userHandle, credentialID,
	).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPasskeyAuthenticationUnavailable
	}
	if err != nil {
		return nil, fmt.Errorf("resolve discoverable passkey owner: %w", err)
	}
	user, err := m.loadPasskeyAuthenticationUser(ctx, userID, rpID)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(user.WebAuthn.ID, userHandle) || user.Credentials[string(credentialID)] == nil {
		return nil, ErrPasskeyAuthenticationUnavailable
	}
	return user, nil
}

func (m *Manager) loadPasskeyAuthenticationUser(ctx context.Context, userID, rpID string) (*passkeyAuthenticationUser, error) {
	user, err := m.GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("load passkey authentication user: %w", err)
	}
	if user == nil || !user.Status.AllowsAuthentication() {
		return nil, ErrPasskeyAuthenticationUnavailable
	}
	var userHandle []byte
	if err := m.db.Read().QueryRowContext(ctx, `
		SELECT user_handle FROM webauthn_users WHERE user_id = ? AND rp_id = ?`, user.ID, rpID,
	).Scan(&userHandle); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPasskeyAuthenticationUnavailable
	} else if err != nil {
		return nil, fmt.Errorf("load passkey authentication handle: %w", err)
	}
	rows, err := m.db.Read().QueryContext(ctx, `
		SELECT id, credential_id, public_key, credential_ciphertext, key_version,
		       sign_count, COALESCE(aaguid, x''), transports, attachment,
		       backup_eligible, backup_state, flags, clone_warning
		FROM webauthn_credentials
		WHERE user_id = ? AND rp_id = ? AND revoked_at IS NULL
		  AND credential_ciphertext IS NOT NULL AND key_version IS NOT NULL
		ORDER BY created_at, id`, user.ID, rpID)
	if err != nil {
		return nil, fmt.Errorf("load passkey authentication credentials: %w", err)
	}
	defer rows.Close()
	loaded := &passkeyAuthenticationUser{
		UserID: user.ID, AuthVersion: user.AuthVersion,
		Credentials: map[string]*passkeyCredentialSnapshot{},
	}
	for rows.Next() {
		snapshot := &passkeyCredentialSnapshot{UserID: user.ID, RPID: rpID}
		var backupEligible, backupState, rawFlags, cloneWarning int
		if err := rows.Scan(
			&snapshot.RowID, &snapshot.CredentialID, &snapshot.PublicKey, &snapshot.Ciphertext,
			&snapshot.KeyVersion, &snapshot.SignCount, &snapshot.AAGUID, &snapshot.TransportsJSON,
			&snapshot.Attachment, &backupEligible, &backupState, &rawFlags, &cloneWarning,
		); err != nil {
			return nil, fmt.Errorf("scan passkey authentication credential: %w", err)
		}
		snapshot.BackupEligible = backupEligible == 1
		snapshot.BackupState = backupState == 1
		snapshot.Flags = byte(rawFlags)
		snapshot.CloneWarning = cloneWarning == 1
		credential, err := m.decryptPasskeyCredential(user.ID, snapshot.RowID, snapshot.Ciphertext, snapshot.KeyVersion)
		if err != nil {
			return nil, err
		}
		if err := validatePasskeyCredentialSnapshot(snapshot, credential); err != nil {
			return nil, err
		}
		snapshot.Credential = *credential
		loaded.WebAuthn.Credentials = append(loaded.WebAuthn.Credentials, *credential)
		loaded.Credentials[string(snapshot.CredentialID)] = snapshot
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate passkey authentication credentials: %w", err)
	}
	if len(loaded.WebAuthn.Credentials) == 0 {
		return nil, ErrPasskeyAuthenticationUnavailable
	}
	name := strings.TrimSpace(user.Username)
	if name == "" {
		name = strings.TrimSpace(user.Email)
	}
	if name == "" {
		name = user.ID
	}
	displayName := strings.TrimSpace(user.Name)
	if displayName == "" {
		displayName = name
	}
	loaded.WebAuthn.ID = append([]byte(nil), userHandle...)
	loaded.WebAuthn.Name = name
	loaded.WebAuthn.DisplayName = displayName
	return loaded, nil
}

func validatePasskeyCredentialSnapshot(snapshot *passkeyCredentialSnapshot, credential *webauthn.Credential) error {
	if snapshot == nil || credential == nil ||
		!bytes.Equal(snapshot.CredentialID, credential.ID) ||
		!bytes.Equal(snapshot.PublicKey, credential.PublicKey) ||
		snapshot.SignCount != credential.Authenticator.SignCount ||
		!bytes.Equal(snapshot.AAGUID, credential.Authenticator.AAGUID) ||
		snapshot.Attachment != string(credential.Authenticator.Attachment) ||
		snapshot.BackupEligible != credential.Flags.BackupEligible ||
		snapshot.BackupState != credential.Flags.BackupState ||
		snapshot.Flags != byte(credential.Flags.ProtocolValue()) ||
		snapshot.CloneWarning != credential.Authenticator.CloneWarning {
		return fmt.Errorf("passkey credential record does not match assertion metadata")
	}
	var transports []string
	if err := json.Unmarshal([]byte(snapshot.TransportsJSON), &transports); err != nil {
		return fmt.Errorf("decode passkey assertion transports: %w", err)
	}
	if len(transports) != len(credential.Transport) {
		return fmt.Errorf("passkey credential transports do not match assertion metadata")
	}
	for index := range transports {
		if transports[index] != string(credential.Transport[index]) {
			return fmt.Errorf("passkey credential transports do not match assertion metadata")
		}
	}
	return nil
}

func validatePreparedPasskeyAssertion(user *passkeyAuthenticationUser, record *passkeyCredentialRecord) (*passkeyCredentialSnapshot, error) {
	if user == nil || record == nil {
		return nil, ErrPasskeyAuthenticationInvalid
	}
	snapshot := user.Credentials[string(record.CredentialID)]
	if snapshot == nil || validatePasskeyRegistrationRecord(record) != nil ||
		!bytes.Equal(snapshot.PublicKey, record.PublicKey) ||
		!bytes.Equal(snapshot.AAGUID, record.AAGUID) ||
		snapshot.Attachment != record.Attachment ||
		snapshot.BackupEligible != record.BackupEligible ||
		len(record.Transports) != len(snapshot.Credential.Transport) {
		return nil, ErrPasskeyAuthenticationInvalid
	}
	for index := range record.Transports {
		if record.Transports[index] != string(snapshot.Credential.Transport[index]) {
			return nil, ErrPasskeyAuthenticationInvalid
		}
	}
	if record.CloneWarning {
		if snapshot.SignCount == 0 && record.SignCount == 0 && !snapshot.CloneWarning {
			return nil, ErrPasskeyAuthenticationInvalid
		}
	} else if snapshot.SignCount != 0 || record.SignCount != 0 {
		if record.SignCount <= snapshot.SignCount {
			return nil, ErrPasskeyAuthenticationInvalid
		}
	}
	return snapshot, nil
}

func currentPasskeyCredentialMatches(ctx context.Context, tx *sql.Tx, expected *passkeyCredentialSnapshot) error {
	current := &passkeyCredentialSnapshot{}
	var backupEligible, backupState, rawFlags, cloneWarning int
	err := tx.QueryRowContext(ctx, `
		SELECT id, user_id, rp_id, credential_id, public_key, credential_ciphertext,
		       key_version, sign_count, COALESCE(aaguid, x''), transports, attachment,
		       backup_eligible, backup_state, flags, clone_warning
		FROM webauthn_credentials
		WHERE id = ? AND user_id = ? AND rp_id = ? AND credential_id = ? AND revoked_at IS NULL`,
		expected.RowID, expected.UserID, expected.RPID, expected.CredentialID,
	).Scan(
		&current.RowID, &current.UserID, &current.RPID, &current.CredentialID, &current.PublicKey,
		&current.Ciphertext, &current.KeyVersion, &current.SignCount, &current.AAGUID,
		&current.TransportsJSON, &current.Attachment, &backupEligible, &backupState, &rawFlags, &cloneWarning,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPasskeyAuthenticationInvalid
	}
	if err != nil {
		return fmt.Errorf("reload asserted passkey credential: %w", err)
	}
	current.BackupEligible = backupEligible == 1
	current.BackupState = backupState == 1
	current.Flags = byte(rawFlags)
	current.CloneWarning = cloneWarning == 1
	if current.RowID != expected.RowID || current.UserID != expected.UserID || current.RPID != expected.RPID ||
		!bytes.Equal(current.CredentialID, expected.CredentialID) || !bytes.Equal(current.PublicKey, expected.PublicKey) ||
		!bytes.Equal(current.Ciphertext, expected.Ciphertext) || current.KeyVersion != expected.KeyVersion ||
		current.SignCount != expected.SignCount || !bytes.Equal(current.AAGUID, expected.AAGUID) ||
		current.TransportsJSON != expected.TransportsJSON || current.Attachment != expected.Attachment ||
		current.BackupEligible != expected.BackupEligible || current.BackupState != expected.BackupState ||
		current.Flags != expected.Flags || current.CloneWarning != expected.CloneWarning {
		return ErrPasskeyAuthenticationInvalid
	}
	return nil
}

func updatePasskeyCredentialAssertion(ctx context.Context, tx *sql.Tx, snapshot *passkeyCredentialSnapshot, record *passkeyCredentialRecord, ciphertext []byte, now time.Time, used bool) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE webauthn_credentials
		SET credential_ciphertext = ?, key_version = ?, sign_count = ?,
		    backup_eligible = ?, backup_state = ?, flags = ?, clone_warning = ?,
		    last_used_at = CASE WHEN ? = 1 THEN ? ELSE last_used_at END
		WHERE id = ? AND user_id = ? AND rp_id = ? AND credential_id = ? AND revoked_at IS NULL`,
		ciphertext, passkeyCredentialKeyVersion, record.SignCount,
		boolInt(record.BackupEligible), boolInt(record.BackupState), int(record.Flags), boolInt(record.CloneWarning),
		boolInt(used), now, snapshot.RowID, snapshot.UserID, snapshot.RPID, snapshot.CredentialID,
	)
	if err != nil {
		return fmt.Errorf("update asserted passkey credential: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return ErrPasskeyAuthenticationInvalid
	}
	return nil
}

func consumePasskeyAssertionChallenge(ctx context.Context, tx *sql.Tx, challenge *PreAuthChallenge, token string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE auth_challenges
		SET attempts = max_attempts, consumed_at = ?, payload_ciphertext = NULL
		WHERE id = ? AND challenge_hash = ? AND consumed_at IS NULL
		  AND expires_at > ? AND attempts < max_attempts`,
		now, challenge.ID, hashToken(token), now,
	)
	if err != nil {
		return fmt.Errorf("consume passkey assertion challenge: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return ErrPasskeyAuthenticationInvalid
	}
	return nil
}

func (m *Manager) readPasskeyAssertionDraft(ctx context.Context, challengeToken, sessionToken, origin string, purpose ChallengePurpose) (*PreAuthChallenge, *passkeyAssertionDraft, error) {
	tx, err := m.db.Read().BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin passkey assertion read: %w", err)
	}
	defer tx.Rollback()
	challenge, draft, err := m.currentPasskeyAssertionDraft(ctx, tx, challengeToken, sessionToken, origin, purpose, m.clock.Now().UTC())
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit passkey assertion read: %w", err)
	}
	return challenge, draft, nil
}

func (m *Manager) currentPasskeyAssertionDraft(ctx context.Context, tx *sql.Tx, challengeToken, sessionToken, origin string, purpose ChallengePurpose, now time.Time) (*PreAuthChallenge, *passkeyAssertionDraft, error) {
	if strings.TrimSpace(challengeToken) == "" {
		return nil, nil, ErrPasskeyAuthenticationInvalid
	}
	challenge, err := scanPreAuthChallenge(tx.QueryRowContext(ctx, preAuthChallengeSelect+`
		WHERE challenge_hash = ? AND purpose = ? AND origin = ?
		  AND consumed_at IS NULL AND expires_at > ? AND attempts < max_attempts`,
		hashToken(challengeToken), purpose, origin, now,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrPasskeyAuthenticationInvalid
	}
	if err != nil {
		return nil, nil, fmt.Errorf("load passkey assertion challenge: %w", err)
	}
	draft, err := m.decryptPasskeyAssertionDraft(challenge, challenge.PayloadCiphertext)
	if err != nil || draft.Version != passkeyAssertionDraftVersion || draft.RPID == "" || len(draft.SessionJSON) == 0 {
		return nil, nil, ErrPasskeyAuthenticationInvalid
	}
	if challenge.UserID != draft.UserID || challenge.SessionID != draft.SessionID {
		return nil, nil, ErrPasskeyAuthenticationInvalid
	}
	if purpose == ChallengePurposeStepUp {
		if strings.TrimSpace(sessionToken) == "" || draft.SessionID == "" || draft.UserID == "" {
			return nil, nil, ErrPasskeyAuthenticationInvalid
		}
		session, err := currentSecuritySession(ctx, tx, sessionToken, now, false)
		if err != nil {
			return nil, nil, err
		}
		if session.ID != draft.SessionID || session.UserID != draft.UserID || session.AuthVersion != draft.AuthVersion {
			return nil, nil, ErrSecuritySessionInvalid
		}
	}
	return challenge, draft, nil
}

func (m *Manager) passkeyAssertionDraftAEAD() (cipher.AEAD, error) {
	if len(m.bucketHashKey) < minimumBucketHashKeyBytes {
		return nil, fmt.Errorf("passkey assertion encryption key is unavailable")
	}
	deriver := hmac.New(sha256.New, m.bucketHashKey)
	_, _ = deriver.Write([]byte(passkeyAssertionDraftKey))
	block, err := aes.NewCipher(deriver.Sum(nil))
	if err != nil {
		return nil, fmt.Errorf("create passkey assertion cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func (m *Manager) encryptPasskeyAssertionDraft(challenge *PreAuthChallenge, draft *passkeyAssertionDraft) ([]byte, error) {
	plaintext, err := json.Marshal(draft)
	if err != nil {
		return nil, fmt.Errorf("encode passkey assertion draft: %w", err)
	}
	aead, err := m.passkeyAssertionDraftAEAD()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate passkey assertion nonce: %w", err)
	}
	payload := []byte{passkeyAssertionDraftVersion}
	payload = append(payload, nonce...)
	return aead.Seal(payload, nonce, plaintext, passkeyAssertionDraftAAD(challenge)), nil
}

func (m *Manager) decryptPasskeyAssertionDraft(challenge *PreAuthChallenge, payload []byte) (*passkeyAssertionDraft, error) {
	aead, err := m.passkeyAssertionDraftAEAD()
	if err != nil {
		return nil, err
	}
	if len(payload) < 1+aead.NonceSize()+aead.Overhead() || payload[0] != passkeyAssertionDraftVersion {
		return nil, ErrPasskeyAuthenticationInvalid
	}
	nonce := payload[1 : 1+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, payload[1+aead.NonceSize():], passkeyAssertionDraftAAD(challenge))
	if err != nil {
		return nil, ErrPasskeyAuthenticationInvalid
	}
	var draft passkeyAssertionDraft
	if err := json.Unmarshal(plaintext, &draft); err != nil {
		return nil, ErrPasskeyAuthenticationInvalid
	}
	return &draft, nil
}

func passkeyAssertionDraftAAD(challenge *PreAuthChallenge) []byte {
	return []byte(challenge.ID + "\x00" + challenge.UserID + "\x00" + challenge.SessionID + "\x00" + challenge.Origin + "\x00" + string(challenge.Purpose))
}

func samePasskeyAssertionDraft(left, right *passkeyAssertionDraft) bool {
	return left != nil && right != nil && left.Version == right.Version && left.Mode == right.Mode &&
		left.AuthVersion == right.AuthVersion && left.UserID == right.UserID && left.SessionID == right.SessionID &&
		left.Identifier == right.Identifier && left.RPID == right.RPID && bytes.Equal(left.UserHandle, right.UserHandle) &&
		string(left.SessionJSON) == string(right.SessionJSON)
}

func SetPasskeyLoginChallengeCookie(w http.ResponseWriter, token string, secure bool, lifetime time.Duration) {
	maxAge := int(lifetime / time.Second)
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name: passkeyLoginChallengeCookieName, Value: token, Path: "/login",
		MaxAge: maxAge, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

func ClearPasskeyLoginChallengeCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: passkeyLoginChallengeCookieName, Value: "", Path: "/login",
		MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

func GetPasskeyLoginChallengeToken(r *http.Request) string {
	cookie, err := r.Cookie(passkeyLoginChallengeCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}
