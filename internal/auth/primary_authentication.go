package auth

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func isFederatedAuthenticationMethod(method AuthenticationMethod) bool {
	return method == AuthenticationMethodFederatedGoogle ||
		method == AuthenticationMethodFederatedMicrosoft ||
		method == AuthenticationMethodFederatedOIDC
}

func (m *Manager) completeFederatedPrimaryAuthentication(ctx context.Context, userID, userAgent string, method AuthenticationMethod) (*PrimaryAuthenticationResult, error) {
	return m.completeFederatedPrimaryAuthenticationWithTransition(ctx, userID, userAgent, method, nil)
}

func (m *Manager) completeFederatedPrimaryAuthenticationWithTransition(
	ctx context.Context,
	userID, userAgent string,
	method AuthenticationMethod,
	transition func(*sql.Tx, time.Time) error,
	verifiedGoogleEmail ...string,
) (*PrimaryAuthenticationResult, error) {
	if !isFederatedAuthenticationMethod(method) {
		return nil, fmt.Errorf("invalid federated authentication method %q", method)
	}
	policy, err := m.loadAuthenticationPolicy(ctx, m.db.Read(), userID, 0)
	if err != nil {
		return nil, err
	}
	now := m.clock.Now().UTC()
	userAgent = boundedUserAgent(userAgent)
	var session *Session
	var challenge *PreAuthChallenge
	var continuation *mfaContinuationDraft
	if policy.RequiresMFA {
		challenge, continuation, err = m.preparePrimaryMFAContinuation(
			ctx, m.db.Read(), userID, policy.AuthVersion, method, policy, m.config.BaseURL, now,
		)
		if err != nil {
			return nil, err
		}
		if method == AuthenticationMethodFederatedGoogle && len(verifiedGoogleEmail) > 0 {
			continuation.VerifiedEmail = verifiedGoogleEmail[0]
			challenge.PayloadCiphertext, err = m.encryptMFAContinuationDraft(challenge, continuation)
			if err != nil {
				return nil, err
			}
		}
	} else {
		id, err := m.tokens.ID()
		if err != nil {
			return nil, fmt.Errorf("generate federated session ID: %w", err)
		}
		token, err := m.tokens.Token(32)
		if err != nil {
			return nil, fmt.Errorf("generate federated session token: %w", err)
		}
		idleExpiresAt := now.Add(sessionIdleLifetime)
		absoluteExpiresAt := now.Add(sessionAbsoluteLifetime)
		if idleExpiresAt.After(absoluteExpiresAt) {
			idleExpiresAt = absoluteExpiresAt
		}
		stepUpAt := now
		session = &Session{
			ID: id, UserID: userID, Token: token, AuthVersion: policy.AuthVersion,
			AuthenticationMethod: method, AssuranceLevel: AssuranceLevelSingleFactor,
			UserAgent: userAgent, AuthenticatedAt: now, LastUsedAt: now,
			IdleExpiresAt: idleExpiresAt, AbsoluteExpiresAt: absoluteExpiresAt,
			StepUpAt: &stepUpAt, StepUpMethod: method, CreatedAt: now,
		}
	}

	err = m.runSecurityTransition(ctx, SecurityTransitionLoginCompletion, func(tx *sql.Tx) error {
		currentPolicy, err := m.loadAuthenticationPolicy(ctx, tx, userID, policy.AuthVersion)
		if err != nil || currentPolicy != policy {
			if err != nil {
				return err
			}
			return ErrAuthenticationPolicyNotSatisfied
		}
		if transition != nil {
			if err := transition(tx, now); err != nil {
				return err
			}
		}
		if challenge != nil {
			if err := m.requireMFAContinuationAuthenticatorState(ctx, tx, challenge.UserID, continuation); err != nil {
				return ErrAuthenticationPolicyNotSatisfied
			}
			if err := m.insertMFAContinuation(ctx, tx, challenge, now); err != nil {
				return err
			}
			return m.appendPrimaryAuthenticationEvent(ctx, tx, userID, nil, method)
		}
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
			return fmt.Errorf("insert federated session: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE users SET last_login_at = ?, updated_at = ?
			WHERE id = ? AND status = 'active' AND auth_version = ?`,
			now, now, userID, policy.AuthVersion,
		); err != nil {
			return fmt.Errorf("update federated login timestamp: %w", err)
		}
		return m.appendPrimaryAuthenticationEvent(ctx, tx, userID, session, method)
	})
	if err != nil {
		return nil, err
	}
	return &PrimaryAuthenticationResult{
		Session: session, PreAuthChallenge: challenge,
		MFAEnrollmentRequired: continuation != nil && continuation.Enrollment != nil,
	}, nil
}
