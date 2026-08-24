package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// BeginGoogleEnrollment exchanges a private, one-time Gofer invitation for a
// short-lived Google OIDC challenge. The invitation secret is verified but is
// never persisted in the challenge; only the enrollment-token row ID is kept
// inside the encrypted, purpose-bound PKCE draft.
func (m *Manager) BeginGoogleEnrollment(ctx context.Context, invitationToken string) (*GoogleLoginStart, error) {
	if !m.HasGoogleLogin() || m.db == nil {
		return nil, fmt.Errorf("Google application login is not configured")
	}
	rawToken := strings.TrimSpace(invitationToken)
	now := m.clock.Now().UTC()
	candidate, err := m.findEnrollmentRedemptionCandidate(ctx, rawToken, now)
	if err != nil {
		return nil, err
	}
	if candidate == nil || candidate.purpose != EnrollmentTokenPurposeEnrollment ||
		candidate.status != UserStatusPending || candidate.userType != UserTypeWebmail {
		return nil, ErrEnrollmentTokenInvalid
	}
	policy, err := queryPendingEnrollmentAuthenticationPolicy(ctx, m.db.Read(), candidate.userID, 0)
	if err != nil {
		return nil, err
	}
	if err := m.requireUserReadyForAuthenticationPolicy(ctx, m.db.Read(), candidate.userID, policy); err != nil {
		return nil, err
	}
	origin, err := canonicalAuthOrigin(m.config.BaseURL)
	if err != nil {
		return nil, err
	}
	id, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate Google enrollment challenge ID: %w", err)
	}
	state, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate Google enrollment state: %w", err)
	}
	nonce, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate Google enrollment nonce: %w", err)
	}
	draft := &googleLoginDraft{
		Version: googleLoginDraftVersion, CodeVerifier: oauth2.GenerateVerifier(),
		EnrollmentTokenID: candidate.tokenID,
	}
	challenge := &PreAuthChallenge{
		ID: id, Token: state, Nonce: nonce, UserID: candidate.userID,
		Purpose: ChallengePurposeFederatedEnrollment, Origin: origin, MaxAttempts: 1,
		CreatedAt: now, ExpiresAt: now.Add(defaultPreAuthLifetime),
	}
	payload, err := m.encryptGoogleLoginDraft(challenge, draft)
	if err != nil {
		return nil, err
	}
	challenge.PayloadCiphertext = payload

	err = m.runSecurityTransition(ctx, SecurityTransitionEnrollment, func(tx *sql.Tx) error {
		current, err := scanEnrollmentRedemptionCandidate(tx.QueryRowContext(ctx, `
			SELECT t.id, t.user_id, t.purpose, u.status,
			       u.username, u.user_type
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
			return fmt.Errorf("recheck Google enrollment invitation: %w", err)
		}
		if !sameEnrollmentRedemptionCandidate(current, candidate) ||
			current.purpose != EnrollmentTokenPurposeEnrollment || current.status != UserStatusPending ||
			current.userType != UserTypeWebmail {
			return ErrEnrollmentTokenInvalid
		}
		currentPolicy, err := queryPendingEnrollmentAuthenticationPolicy(ctx, tx, current.userID, policy.AuthVersion)
		if err != nil || currentPolicy != policy {
			if err != nil {
				return err
			}
			return ErrAuthenticationPolicyNotSatisfied
		}
		if err := m.requireUserReadyForAuthenticationPolicy(ctx, tx, current.userID, currentPolicy); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges
			SET consumed_at = COALESCE(consumed_at, ?), payload_ciphertext = NULL
			WHERE user_id = ? AND purpose = ? AND consumed_at IS NULL`,
			now, current.userID, ChallengePurposeFederatedEnrollment,
		); err != nil {
			return fmt.Errorf("replace Google enrollment challenge: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_challenges (
				id, user_id, session_id, challenge_hash, nonce_hash, purpose, origin,
				attempts, max_attempts, payload_ciphertext, created_at, expires_at
			) VALUES (?, ?, NULL, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
			challenge.ID, current.userID, hashToken(challenge.Token), hashToken(challenge.Nonce),
			challenge.Purpose, challenge.Origin, challenge.MaxAttempts,
			challenge.PayloadCiphertext, challenge.CreatedAt, challenge.ExpiresAt,
		); err != nil {
			return fmt.Errorf("insert Google enrollment challenge: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	authorizationURL := googleApplicationAuthorizationURL(
		m.config.GoogleLoginClient,
		challenge.Token,
		oauth2.S256ChallengeOption(draft.CodeVerifier),
		oidc.Nonce(challenge.Nonce),
	)
	return &GoogleLoginStart{Challenge: challenge, AuthorizationURL: authorizationURL}, nil
}

// CompleteGoogleEnrollment verifies Google only as a Gofer authentication
// identity. It does not persist OAuth access or refresh tokens and does not
// create an account/mailbox row. Identity linking, invitation consumption,
// user activation, and the resulting session or MFA continuation are atomic.
func (m *Manager) CompleteGoogleEnrollment(
	ctx context.Context, challengeToken, code, userAgent string,
) (*PrimaryAuthenticationResult, error) {
	challenge, draft, claims, err := m.verifyGoogleCallback(
		ctx, challengeToken, ChallengePurposeFederatedEnrollment, code,
	)
	if err != nil {
		return nil, err
	}
	now := m.clock.Now().UTC()
	policy, err := queryPendingEnrollmentAuthenticationPolicy(ctx, m.db.Read(), challenge.UserID, 0)
	if err != nil {
		_ = m.TerminatePreAuthChallenge(ctx, challengeToken, ChallengePurposeFederatedEnrollment, m.config.BaseURL)
		return nil, federatedLoginError(FederatedLoginFailurePolicyCompletion)
	}
	if err := m.requireUserReadyForAuthenticationPolicy(ctx, m.db.Read(), challenge.UserID, policy); err != nil {
		_ = m.TerminatePreAuthChallenge(ctx, challengeToken, ChallengePurposeFederatedEnrollment, m.config.BaseURL)
		return nil, err
	}

	identityID, err := m.tokens.ID()
	if err != nil {
		_ = m.TerminatePreAuthChallenge(ctx, challengeToken, ChallengePurposeFederatedEnrollment, m.config.BaseURL)
		return nil, federatedLoginError(FederatedLoginFailureInternal)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		_ = m.TerminatePreAuthChallenge(ctx, challengeToken, ChallengePurposeFederatedEnrollment, m.config.BaseURL)
		return nil, federatedLoginError(FederatedLoginFailureInternal)
	}

	userAgent = boundedUserAgent(userAgent)
	var session *Session
	var mfaChallenge *PreAuthChallenge
	var mfaContinuation *mfaContinuationDraft
	conflict := false
	if policy.RequiresMFA {
		mfaChallenge, mfaContinuation, err = m.preparePrimaryMFAContinuation(
			ctx, m.db.Read(),
			challenge.UserID, policy.AuthVersion, AuthenticationMethodFederatedGoogle,
			policy, m.config.BaseURL, now,
		)
		if err != nil {
			_ = m.TerminatePreAuthChallenge(ctx, challengeToken, ChallengePurposeFederatedEnrollment, m.config.BaseURL)
			return nil, federatedLoginError(FederatedLoginFailureInternal)
		}
	} else {
		sessionID, err := m.tokens.ID()
		if err != nil {
			_ = m.TerminatePreAuthChallenge(ctx, challengeToken, ChallengePurposeFederatedEnrollment, m.config.BaseURL)
			return nil, federatedLoginError(FederatedLoginFailureInternal)
		}
		sessionToken, err := m.tokens.Token(32)
		if err != nil {
			_ = m.TerminatePreAuthChallenge(ctx, challengeToken, ChallengePurposeFederatedEnrollment, m.config.BaseURL)
			return nil, federatedLoginError(FederatedLoginFailureInternal)
		}
		idleExpiresAt := now.Add(sessionIdleLifetime)
		absoluteExpiresAt := now.Add(sessionAbsoluteLifetime)
		if idleExpiresAt.After(absoluteExpiresAt) {
			idleExpiresAt = absoluteExpiresAt
		}
		stepUpAt := now
		session = &Session{
			ID: sessionID, UserID: challenge.UserID, Token: sessionToken,
			AuthVersion: policy.AuthVersion, AuthenticationMethod: AuthenticationMethodFederatedGoogle,
			AssuranceLevel: AssuranceLevelSingleFactor, UserAgent: userAgent,
			AuthenticatedAt: now, LastUsedAt: now, IdleExpiresAt: idleExpiresAt,
			AbsoluteExpiresAt: absoluteExpiresAt, StepUpAt: &stepUpAt,
			StepUpMethod: AuthenticationMethodFederatedGoogle, CreatedAt: now,
		}
	}

	err = m.runSecurityTransition(ctx, SecurityTransitionEnrollment, func(tx *sql.Tx) error {
		currentChallenge, currentDraft, nonceHash, err := m.currentGoogleAuthorizationChallenge(
			ctx, tx, challengeToken, ChallengePurposeFederatedEnrollment,
		)
		if err != nil || currentChallenge.ID != challenge.ID || currentChallenge.UserID != challenge.UserID ||
			!sameGoogleLoginDraft(currentDraft, draft) ||
			subtle.ConstantTimeCompare([]byte(hashToken(claims.Nonce)), []byte(nonceHash)) != 1 {
			return ErrPreAuthChallengeInvalid
		}
		var tokenUserID string
		var tokenPurpose EnrollmentTokenPurpose
		if err := tx.QueryRowContext(ctx, `
			SELECT user_id, purpose FROM user_enrollment_tokens
			WHERE id = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
			currentDraft.EnrollmentTokenID, now,
		).Scan(&tokenUserID, &tokenPurpose); errors.Is(err, sql.ErrNoRows) {
			return ErrEnrollmentTokenInvalid
		} else if err != nil {
			return fmt.Errorf("recheck Google enrollment invitation: %w", err)
		}
		if tokenUserID != currentChallenge.UserID || tokenPurpose != EnrollmentTokenPurposeEnrollment {
			return ErrEnrollmentTokenInvalid
		}
		currentPolicy, err := queryPendingEnrollmentAuthenticationPolicy(
			ctx, tx, currentChallenge.UserID, policy.AuthVersion,
		)
		if err != nil || currentPolicy != policy {
			if err != nil {
				return err
			}
			return ErrAuthenticationPolicyNotSatisfied
		}
		if err := m.requireUserReadyForAuthenticationPolicy(ctx, tx, currentChallenge.UserID, currentPolicy); err != nil {
			return err
		}

		inserted, err := tx.ExecContext(ctx, `
			INSERT INTO auth_identities (
				id, user_id, provider, issuer, subject, email, email_verified,
				created_at, linked_at, last_used_at
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)
			ON CONFLICT(issuer, subject) DO NOTHING`,
			identityID, currentChallenge.UserID, googleIdentityProvider, googleLoginIssuer,
			claims.Subject, strings.TrimSpace(claims.Email), now, now, now,
		)
		if err != nil {
			return fmt.Errorf("insert enrolled Google identity: %w", err)
		}
		insertedCount, err := inserted.RowsAffected()
		if err != nil {
			return fmt.Errorf("count enrolled Google identity insert: %w", err)
		}
		conflict = insertedCount != 1
		if !conflict {
			activated, err := tx.ExecContext(ctx, `
				UPDATE users
				SET status = 'active', disabled_at = NULL, disabled_by = NULL, updated_at = ?
				WHERE id = ? AND status = 'pending' AND auth_version = ?`,
				now, currentChallenge.UserID, currentPolicy.AuthVersion,
			)
			if err != nil {
				return fmt.Errorf("activate Google-enrolled user: %w", err)
			}
			if count, err := activated.RowsAffected(); err != nil || count != 1 {
				return ErrEnrollmentTokenInvalid
			}

			consumedInvitation, err := tx.ExecContext(ctx, `
				UPDATE user_enrollment_tokens SET used_at = ?
				WHERE id = ? AND user_id = ? AND purpose = ?
				  AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
				now, currentDraft.EnrollmentTokenID, currentChallenge.UserID,
				EnrollmentTokenPurposeEnrollment, now,
			)
			if err != nil {
				return fmt.Errorf("consume Google enrollment invitation: %w", err)
			}
			if count, err := consumedInvitation.RowsAffected(); err != nil || count != 1 {
				return ErrEnrollmentTokenInvalid
			}
		}

		consumedChallenge, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges
			SET attempts = attempts + 1, consumed_at = ?, payload_ciphertext = NULL
			WHERE id = ? AND challenge_hash = ? AND nonce_hash = ? AND purpose = ?
			  AND origin = ? AND user_id = ? AND session_id IS NULL
			  AND consumed_at IS NULL AND expires_at > ? AND attempts < max_attempts`,
			now, currentChallenge.ID, hashToken(challengeToken), hashToken(claims.Nonce),
			ChallengePurposeFederatedEnrollment, currentChallenge.Origin,
			currentChallenge.UserID, now,
		)
		if err != nil {
			return fmt.Errorf("consume Google enrollment challenge: %w", err)
		}
		if count, err := consumedChallenge.RowsAffected(); err != nil || count != 1 {
			return ErrPreAuthChallengeInvalid
		}

		if conflict {
			// The verified Google subject belongs to another Gofer user. Consume
			// this OIDC attempt and audit the generic conflict, but leave the
			// invitation and pending target untouched.
		} else if mfaChallenge != nil {
			if err := m.requireMFAContinuationAuthenticatorState(ctx, tx, mfaChallenge.UserID, mfaContinuation); err != nil {
				return err
			}
			if err := m.insertMFAContinuation(ctx, tx, mfaChallenge, now); err != nil {
				return err
			}
		} else {
			if !currentPolicy.allowsAssurance(session.AssuranceLevel) {
				return ErrAuthenticationPolicyNotSatisfied
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO sessions (
					id, user_id, token_hash, auth_version, authentication_method,
					assurance_level, user_agent, authenticated_at, last_used_at,
					idle_expires_at, absolute_expires_at, step_up_at, step_up_method, created_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				session.ID, session.UserID, hashToken(session.Token), session.AuthVersion,
				session.AuthenticationMethod, session.AssuranceLevel, session.UserAgent,
				session.AuthenticatedAt, session.LastUsedAt, session.IdleExpiresAt,
				session.AbsoluteExpiresAt, session.StepUpAt, session.StepUpMethod, session.CreatedAt,
			); err != nil {
				return fmt.Errorf("insert Google enrollment session: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE users SET last_login_at = ?, updated_at = ?
				WHERE id = ? AND status = 'active' AND auth_version = ?`,
				now, now, currentChallenge.UserID, currentPolicy.AuthVersion,
			); err != nil {
				return fmt.Errorf("update Google enrollment login timestamp: %w", err)
			}
		}

		result := "enrolled"
		success := 1
		reason := AuthEventReasonChallengeVerified
		if conflict {
			result = "identity_conflict"
			success = 0
			reason = AuthEventReasonInvalidCredentials
		}
		metadata, err := googleEnrollmentEventJSON(currentDraft.EnrollmentTokenID, result)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, event_type,
				success, reason, user_agent, metadata_json
			) VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?)`,
			eventID, now, currentChallenge.UserID, AuthEventEnrollmentCompleted,
			success, reason, userAgent, metadata,
		); err != nil {
			return fmt.Errorf("record Google enrollment completion: %w", err)
		}
		return nil
	})
	if err != nil {
		_ = m.TerminatePreAuthChallenge(ctx, challengeToken, ChallengePurposeFederatedEnrollment, m.config.BaseURL)
		switch {
		case errors.Is(err, ErrFederatedIdentityConflict):
			return nil, ErrFederatedIdentityConflict
		case errors.Is(err, ErrEnrollmentTokenInvalid), errors.Is(err, ErrPreAuthChallengeInvalid):
			return nil, federatedLoginError(FederatedLoginFailureChallengeInvalid)
		case errors.Is(err, ErrInstanceMFAEnrollmentNeeded):
			return nil, err
		default:
			return nil, federatedLoginError(FederatedLoginFailurePolicyCompletion)
		}
	}
	if conflict {
		return nil, ErrFederatedIdentityConflict
	}
	return &PrimaryAuthenticationResult{
		Session: session, PreAuthChallenge: mfaChallenge,
		MFAEnrollmentRequired: mfaContinuation != nil && mfaContinuation.Enrollment != nil,
	}, nil
}

func queryPendingEnrollmentAuthenticationPolicy(
	ctx context.Context, queryer authenticationPolicyQueryer, userID string, expectedAuthVersion int64,
) (authenticationPolicy, error) {
	var authVersion int64
	var mfaRequired, isAdmin int
	var instanceMFAPolicy InstanceMFAPolicy
	err := queryer.QueryRowContext(ctx, `
		SELECT u.auth_version, u.mfa_required, u.is_admin,
		       COALESCE((SELECT state.mfa_policy FROM auth_system_state state WHERE state.id = 1), 'administrators')
		FROM users u
		WHERE u.id = ? AND u.status = 'pending'
		  AND (? = 0 OR u.auth_version = ?)`,
		userID, expectedAuthVersion, expectedAuthVersion,
	).Scan(&authVersion, &mfaRequired, &isAdmin, &instanceMFAPolicy)
	if errors.Is(err, sql.ErrNoRows) {
		return authenticationPolicy{}, ErrEnrollmentTokenInvalid
	}
	if err != nil {
		return authenticationPolicy{}, fmt.Errorf("load pending enrollment authentication policy: %w", err)
	}
	if !instanceMFAPolicy.Valid() {
		return authenticationPolicy{}, fmt.Errorf("%w: %q", ErrInstanceMFAPolicyInvalid, instanceMFAPolicy)
	}
	return resolveAuthenticationPolicy(
		authVersion, mfaRequired == 1, isAdmin == 1, instanceMFAPolicy.RequiresAllUsers(),
	), nil
}

func (m *Manager) requireUserReadyForAuthenticationPolicy(
	ctx context.Context, queryer instanceSecurityPolicyQueryer, userID string, policy authenticationPolicy,
) error {
	if !policy.RequiresMFA {
		return nil
	}
	if policy.UserMFARequired && !policy.AdministratorMFARequired && !policy.InstanceMFARequired {
		return nil
	}
	_, rpID, err := canonicalWebAuthnRelyingParty(m.config.BaseURL)
	if err != nil {
		return fmt.Errorf("resolve passkey relying party for Google enrollment: %w", err)
	}
	ready, err := userHasStrongAuthenticator(ctx, queryer, userID, rpID)
	if err != nil {
		return err
	}
	if !ready {
		return ErrInstanceMFAEnrollmentNeeded
	}
	return nil
}

func googleEnrollmentEventJSON(tokenID, result string) (string, error) {
	metadata := struct {
		TokenID string                 `json:"token_id"`
		Purpose EnrollmentTokenPurpose `json:"purpose"`
		Method  AuthenticationMethod   `json:"method"`
		Result  string                 `json:"result"`
	}{
		TokenID: tokenID, Purpose: EnrollmentTokenPurposeEnrollment,
		Method: AuthenticationMethodFederatedGoogle, Result: result,
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode Google enrollment event metadata: %w", err)
	}
	return string(encoded), nil
}
