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

const googleIdentityProvider = "google"

var (
	ErrFederatedIdentityUnknown  = errors.New("federated identity is not linked")
	ErrFederatedIdentityConflict = errors.New("federated identity belongs to another user")
)

type FederatedIdentitySummary struct {
	ID            string
	Provider      string
	Issuer        string
	Email         string
	EmailVerified bool
	LinkedAt      time.Time
	LastUsedAt    *time.Time
}

func scanFederatedIdentity(row rowScanner) (*FederatedIdentitySummary, error) {
	identity := &FederatedIdentitySummary{}
	var emailVerified int
	var lastUsedAt sql.NullTime
	if err := row.Scan(
		&identity.ID, &identity.Provider, &identity.Issuer, &identity.Email,
		&emailVerified, &identity.LinkedAt, &lastUsedAt,
	); err != nil {
		return nil, err
	}
	identity.EmailVerified = emailVerified == 1
	if lastUsedAt.Valid {
		identity.LastUsedAt = &lastUsedAt.Time
	}
	return identity, nil
}

func (m *Manager) ListFederatedIdentities(ctx context.Context, userID string) ([]FederatedIdentitySummary, error) {
	rows, err := m.db.Read().QueryContext(ctx, `
		SELECT id, provider, issuer, email, email_verified, linked_at, last_used_at
		FROM auth_identities WHERE user_id = ?
		ORDER BY provider, linked_at, id`, strings.TrimSpace(userID))
	if err != nil {
		return nil, fmt.Errorf("list federated identities: %w", err)
	}
	defer rows.Close()
	identities := []FederatedIdentitySummary{}
	for rows.Next() {
		identity, err := scanFederatedIdentity(rows)
		if err != nil {
			return nil, fmt.Errorf("scan federated identity: %w", err)
		}
		identities = append(identities, *identity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list federated identities: %w", err)
	}
	return identities, nil
}

func (m *Manager) authenticateGoogleIdentity(ctx context.Context, claims *GoogleIDTokenClaims, userAgent string) (*User, *PrimaryAuthenticationResult, error) {
	if !validVerifiedGoogleIdentity(claims) {
		return nil, nil, federatedLoginError(FederatedLoginFailureIDTokenInvalid)
	}
	var identityID, userID string
	err := m.db.Read().QueryRowContext(ctx, `
		SELECT identity.id, identity.user_id
		FROM auth_identities identity
		JOIN users ON users.id = identity.user_id
		WHERE identity.provider = ? AND identity.issuer = ? AND identity.subject = ?
		  AND users.status = 'active'`,
		googleIdentityProvider, googleLoginIssuer, claims.Subject,
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
		ctx, user.ID, userAgent, AuthenticationMethodFederatedGoogle,
		func(tx *sql.Tx, now time.Time) error {
			updated, err := tx.ExecContext(ctx, `
				UPDATE auth_identities
				SET email = ?, email_verified = 1, last_used_at = ?
				WHERE id = ? AND user_id = ? AND provider = ? AND issuer = ? AND subject = ?`,
				strings.TrimSpace(claims.Email), now, identityID, user.ID,
				googleIdentityProvider, googleLoginIssuer, claims.Subject,
			)
			if err != nil {
				return fmt.Errorf("update Google identity use: %w", err)
			}
			changed, err := updated.RowsAffected()
			if err != nil {
				return fmt.Errorf("count Google identity update: %w", err)
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

func (m *Manager) CompleteGoogleIdentityLink(
	ctx context.Context,
	challengeToken, sessionToken, code, userAgent string,
) (*FederatedIdentitySummary, error) {
	challenge, draft, claims, err := m.verifyGoogleCallback(
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
		currentChallenge, currentDraft, nonceHash, err := m.currentGoogleAuthorizationChallenge(
			ctx, tx, challengeToken, ChallengePurposeFederatedLink,
		)
		if err != nil || currentChallenge.ID != challenge.ID || !sameGoogleLoginDraft(currentDraft, draft) ||
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

		inserted, err := tx.ExecContext(ctx, `
			INSERT INTO auth_identities (
				id, user_id, provider, issuer, subject, email, email_verified, created_at, linked_at
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)
			ON CONFLICT(issuer, subject) DO NOTHING`,
			identityID, currentSession.UserID, googleIdentityProvider, googleLoginIssuer,
			claims.Subject, strings.TrimSpace(claims.Email), now, now,
		)
		if err != nil {
			return fmt.Errorf("insert Google identity: %w", err)
		}
		insertedCount, err := inserted.RowsAffected()
		if err != nil {
			return fmt.Errorf("count Google identity insert: %w", err)
		}
		result := "linked"
		success := 1
		reason := AuthEventReasonChallengeVerified
		if insertedCount == 0 {
			var existingUserID string
			if err := tx.QueryRowContext(ctx, `
				SELECT id, user_id FROM auth_identities
				WHERE provider = ? AND issuer = ? AND subject = ?`,
				googleIdentityProvider, googleLoginIssuer, claims.Subject,
			).Scan(&storedIdentityID, &existingUserID); err != nil {
				return fmt.Errorf("load conflicting Google identity: %w", err)
			}
			if existingUserID != currentSession.UserID {
				conflict = true
				result = "conflict"
				success = 0
				reason = AuthEventReasonInvalidCredentials
			} else if _, err := tx.ExecContext(ctx, `
				UPDATE auth_identities SET email = ?, email_verified = 1
				WHERE id = ? AND user_id = ?`,
				strings.TrimSpace(claims.Email), storedIdentityID, currentSession.UserID,
			); err != nil {
				return fmt.Errorf("refresh linked Google identity: %w", err)
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
			return fmt.Errorf("consume Google identity-link challenge: %w", err)
		}
		consumedCount, err := consumed.RowsAffected()
		if err != nil || consumedCount != 1 {
			return ErrPreAuthChallengeInvalid
		}
		metadata := `{"provider":"google","result":"` + result + `"}`
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			eventID, now, currentSession.UserID, currentSession.UserID, currentSession.ID,
			AuthEventIdentityLinked, success, reason, userAgent, metadata,
		); err != nil {
			return fmt.Errorf("record Google identity link: %w", err)
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
		return nil, fmt.Errorf("load linked Google identity: %w", err)
	}
	return identity, nil
}

func validVerifiedGoogleIdentity(claims *GoogleIDTokenClaims) bool {
	return boundedGoogleIdentityClaims(claims) && strings.TrimSpace(claims.Subject) != "" &&
		strings.TrimSpace(claims.Email) != "" && claims.EmailVerified
}

func sameGoogleLoginDraft(left, right *googleLoginDraft) bool {
	return validGoogleLoginDraft(left) && validGoogleLoginDraft(right) &&
		left.Version == right.Version && left.CodeVerifier == right.CodeVerifier
}
