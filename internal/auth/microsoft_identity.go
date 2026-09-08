package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (m *Manager) authenticateMicrosoftIdentity(ctx context.Context, claims *MicrosoftIDTokenClaims, userAgent string, consume ...func(*sql.Tx, time.Time) error) (*User, *PrimaryAuthenticationResult, error) {
	if !validMicrosoftIdentity(claims) {
		return nil, nil, federatedLoginError(FederatedLoginFailureIDTokenInvalid)
	}
	var identityID, userID string
	err := m.db.Read().QueryRowContext(ctx, `
		SELECT identity.id, identity.user_id
		FROM auth_identities identity
		JOIN users ON users.id = identity.user_id
		WHERE identity.provider = ? AND identity.issuer = ? AND identity.subject = ?
		  AND users.status = 'active' AND users.user_type = 'webmail'`,
		microsoftIdentityProvider, claims.Issuer, claims.Subject,
	).Scan(&identityID, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, federatedLoginError(FederatedLoginFailureIdentityUnknown)
	}
	if err != nil {
		return nil, nil, federatedLoginError(FederatedLoginFailureInternal)
	}
	user, err := m.GetUserByID(ctx, userID)
	if err != nil {
		return nil, nil, federatedLoginError(FederatedLoginFailureInternal)
	}
	if user == nil || !user.Status.AllowsAuthentication() {
		return nil, nil, federatedLoginError(FederatedLoginFailureIdentityUnknown)
	}
	result, err := m.completeFederatedPrimaryAuthenticationWithTransition(
		ctx, user.ID, userAgent, AuthenticationMethodFederatedMicrosoft,
		func(tx *sql.Tx, now time.Time) error {
			for _, action := range consume {
				if err := action(tx, now); err != nil {
					return err
				}
			}
			updated, err := tx.ExecContext(ctx, `
				UPDATE auth_identities
				SET email = ?, email_verified = 0, last_used_at = ?
				WHERE id = ? AND user_id = ? AND provider = ? AND issuer = ? AND subject = ?`,
				claims.DisplayEmail(), now, identityID, user.ID,
				microsoftIdentityProvider, claims.Issuer, claims.Subject,
			)
			if err != nil {
				return fmt.Errorf("update Microsoft identity use: %w", err)
			}
			changed, err := updated.RowsAffected()
			if err != nil {
				return fmt.Errorf("count Microsoft identity update: %w", err)
			}
			if changed != 1 {
				return ErrFederatedIdentityUnknown
			}
			return nil
		},
	)
	if errors.Is(err, ErrFederatedIdentityUnknown) || errors.Is(err, ErrUserNotActive) {
		return nil, nil, federatedLoginError(FederatedLoginFailureIdentityUnknown)
	}
	if err != nil {
		return nil, nil, federatedLoginError(FederatedLoginFailurePolicyCompletion)
	}
	return user, result, nil
}

func (m *Manager) CompleteMicrosoftIdentityLink(
	ctx context.Context,
	challengeToken, sessionToken, code, userAgent string,
) (*FederatedIdentitySummary, error) {
	challenge, draft, claims, err := m.verifyMicrosoftCallback(
		ctx, challengeToken, ChallengePurposeFederatedLink, code,
	)
	if err != nil {
		return nil, err
	}
	identityID, err := m.tokens.ID()
	if err != nil {
		_ = m.TerminatePreAuthChallenge(ctx, challengeToken, ChallengePurposeFederatedLink, m.config.BaseURL)
		return nil, federatedLoginError(FederatedLoginFailureInternal)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		_ = m.TerminatePreAuthChallenge(ctx, challengeToken, ChallengePurposeFederatedLink, m.config.BaseURL)
		return nil, federatedLoginError(FederatedLoginFailureInternal)
	}
	now := m.clock.Now().UTC()
	userAgent = boundedUserAgent(userAgent)
	conflict := false
	storedIdentityID := identityID
	err = m.runSecurityTransition(ctx, SecurityTransitionIdentityChange, func(tx *sql.Tx) error {
		currentChallenge, currentDraft, nonceHash, err := m.currentMicrosoftAuthorizationChallenge(
			ctx, tx, challengeToken, ChallengePurposeFederatedLink,
		)
		if err != nil || currentChallenge.ID != challenge.ID || !sameMicrosoftLoginDraft(currentDraft, draft) ||
			subtle.ConstantTimeCompare([]byte(hashToken(claims.Nonce)), []byte(nonceHash)) != 1 {
			return ErrPreAuthChallengeInvalid
		}
		currentSession, err := currentSecuritySession(ctx, tx, sessionToken, now, true)
		if err != nil {
			return err
		}
		if currentChallenge.SessionID != currentSession.ID || currentChallenge.UserID != currentSession.UserID {
			return ErrSecuritySessionInvalid
		}
		if err := requireWebmailUser(ctx, tx, currentSession.UserID); err != nil {
			return err
		}

		inserted, err := tx.ExecContext(ctx, `
			INSERT INTO auth_identities (
				id, user_id, provider, issuer, subject, email, email_verified, created_at, linked_at
			) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)
			ON CONFLICT(issuer, subject) DO NOTHING`,
			identityID, currentSession.UserID, microsoftIdentityProvider, claims.Issuer,
			claims.Subject, claims.DisplayEmail(), now, now,
		)
		if err != nil {
			return fmt.Errorf("insert Microsoft identity: %w", err)
		}
		insertedCount, err := inserted.RowsAffected()
		if err != nil {
			return fmt.Errorf("count Microsoft identity insert: %w", err)
		}
		result := "linked"
		success := 1
		reason := AuthEventReasonChallengeVerified
		if insertedCount == 0 {
			var existingUserID string
			if err := tx.QueryRowContext(ctx, `
				SELECT id, user_id FROM auth_identities
				WHERE provider = ? AND issuer = ? AND subject = ?`,
				microsoftIdentityProvider, claims.Issuer, claims.Subject,
			).Scan(&storedIdentityID, &existingUserID); err != nil {
				return fmt.Errorf("load conflicting Microsoft identity: %w", err)
			}
			if existingUserID != currentSession.UserID {
				conflict = true
				result = "conflict"
				success = 0
				reason = AuthEventReasonInvalidCredentials
			} else if _, err := tx.ExecContext(ctx, `
				UPDATE auth_identities SET email = ?, email_verified = 0
				WHERE id = ? AND user_id = ?`,
				claims.DisplayEmail(), storedIdentityID, currentSession.UserID,
			); err != nil {
				return fmt.Errorf("refresh linked Microsoft identity: %w", err)
			} else {
				result = "already_linked"
			}
		}

		consumed, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges
			SET attempts = attempts + 1, consumed_at = ?, payload_ciphertext = NULL
			WHERE id = ? AND challenge_hash = ? AND nonce_hash = ? AND purpose = ? AND origin = ?
			  AND user_id = ? AND session_id = ? AND consumed_at IS NULL
			  AND expires_at > ? AND attempts < max_attempts`,
			now, currentChallenge.ID, hashToken(challengeToken), hashToken(claims.Nonce),
			ChallengePurposeFederatedLink, currentChallenge.Origin, currentSession.UserID,
			currentSession.ID, now,
		)
		if err != nil {
			return fmt.Errorf("consume Microsoft identity-link challenge: %w", err)
		}
		consumedCount, err := consumed.RowsAffected()
		if err != nil || consumedCount != 1 {
			return ErrPreAuthChallengeInvalid
		}
		metadata := `{"provider":"microsoft","result":"` + result + `"}`
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			eventID, now, currentSession.UserID, currentSession.UserID, currentSession.ID,
			AuthEventIdentityLinked, success, reason, userAgent, metadata,
		); err != nil {
			return fmt.Errorf("record Microsoft identity link: %w", err)
		}
		return nil
	})
	if err != nil {
		_ = m.TerminatePreAuthChallenge(ctx, challengeToken, ChallengePurposeFederatedLink, m.config.BaseURL)
		return nil, err
	}
	if conflict {
		return nil, ErrFederatedIdentityConflict
	}
	identity, err := scanFederatedIdentity(m.db.Read().QueryRowContext(ctx, `
		SELECT id, provider, issuer, email, email_verified, linked_at, last_used_at
		FROM auth_identities WHERE id = ?`, storedIdentityID))
	if err != nil {
		return nil, fmt.Errorf("load linked Microsoft identity: %w", err)
	}
	return identity, nil
}

// UnlinkMicrosoftIdentity removes one exact Microsoft application-login
// identity. It does not inspect or mutate Outlook mailbox accounts or Graph
// OAuth credentials.
func (m *Manager) UnlinkMicrosoftIdentity(
	ctx context.Context,
	sessionToken string,
	identityID string,
	userAgent string,
) (*Session, error) {
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
		return nil, fmt.Errorf("validate Microsoft identity-removal relying party: %w", err)
	}
	identityID = strings.TrimSpace(identityID)
	if identityID == "" {
		return nil, ErrFederatedIdentityUnknown
	}
	newSessionID, newSessionToken, eventID, err := m.securityRotationMaterial()
	if err != nil {
		return nil, err
	}
	userAgent = boundedUserAgent(userAgent)
	var rotated *Session
	err = m.runSecurityTransition(ctx, SecurityTransitionIdentityChange, func(tx *sql.Tx) error {
		current, err := currentSecuritySession(ctx, tx, sessionToken, now, true)
		if err != nil || current.ID != session.ID {
			return ErrSecuritySessionInvalid
		}
		var provider string
		if err := tx.QueryRowContext(ctx, `
			SELECT provider FROM auth_identities
			WHERE id = ? AND user_id = ?`, identityID, current.UserID,
		).Scan(&provider); errors.Is(err, sql.ErrNoRows) {
			return ErrFederatedIdentityUnknown
		} else if err != nil {
			return fmt.Errorf("load removable federated identity: %w", err)
		}
		if provider != microsoftIdentityProvider {
			return ErrFederatedIdentityUnknown
		}
		canUnlink, err := canRemoveAuthenticatorInTransaction(
			ctx, tx, current.UserID, rpID, m.configuredFederatedLoginAvailability(),
			authenticatorRemoval{IdentityID: identityID},
		)
		if err != nil {
			return err
		}
		if !canUnlink {
			return ErrLastAuthenticator
		}
		removed, err := tx.ExecContext(ctx, `
			DELETE FROM auth_identities
			WHERE id = ? AND user_id = ? AND provider = ?`,
			identityID, current.UserID, microsoftIdentityProvider,
		)
		if err != nil {
			return fmt.Errorf("unlink Microsoft identity: %w", err)
		}
		changed, err := removed.RowsAffected()
		if err != nil || changed != 1 {
			return ErrFederatedIdentityUnknown
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
			return fmt.Errorf("terminate identity-management challenges: %w", err)
		}
		rotated, err = insertRotatedSecuritySession(
			ctx, tx, current, newAuthVersion, newSessionID, newSessionToken, userAgent,
			current.StepUpMethod, now,
		)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, '{"provider":"microsoft","result":"unlinked"}')`,
			eventID, now, current.UserID, current.UserID, rotated.ID,
			AuthEventIdentityUnlinked, AuthEventReasonChallengeVerified, userAgent,
		); err != nil {
			return fmt.Errorf("record Microsoft identity unlink: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rotated, nil
}

func validMicrosoftIdentity(claims *MicrosoftIDTokenClaims) bool {
	if !boundedMicrosoftIdentityClaims(claims) || strings.TrimSpace(claims.Subject) == "" || strings.TrimSpace(claims.TenantID) == "" {
		return false
	}
	parsedTenant, err := uuid.Parse(strings.TrimSpace(claims.TenantID))
	if err != nil {
		return false
	}
	return claims.Issuer == microsoftTenantIssuer(strings.ToLower(parsedTenant.String()))
}
