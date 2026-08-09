package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrAuthenticationPolicyNotSatisfied = errors.New("authentication assurance does not satisfy policy")

type authenticationPolicy struct {
	AuthVersion int64
	RequiresMFA bool
}

type authenticationPolicyQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func resolveAuthenticationPolicy(authVersion int64, mfaRequired, isAdmin bool) authenticationPolicy {
	return authenticationPolicy{
		AuthVersion: authVersion,
		RequiresMFA: mfaRequired || isAdmin,
	}
}

func (policy authenticationPolicy) allowsAssurance(assurance AssuranceLevel) bool {
	if !assurance.Valid() {
		return false
	}
	if !policy.RequiresMFA {
		return true
	}
	return assurance == AssuranceLevelMultiFactor || assurance == AssuranceLevelPhishingResistant
}

func (policy authenticationPolicy) allowsStepUpMethod(method AuthenticationMethod) bool {
	if !method.Valid() || method == AuthenticationMethodLegacy {
		return false
	}
	if !policy.RequiresMFA {
		return true
	}
	return method == AuthenticationMethodPasskey || method == AuthenticationMethodTOTP || method == AuthenticationMethodRecoveryCode
}

func (m *Manager) loadAuthenticationPolicy(ctx context.Context, queryer authenticationPolicyQueryer, userID string, expectedAuthVersion int64) (authenticationPolicy, error) {
	var authVersion int64
	var mfaRequired, isAdmin int
	err := queryer.QueryRowContext(ctx, `
		SELECT auth_version, mfa_required, is_admin
		FROM users
		WHERE id = ? AND status = 'active'
		  AND (? = 0 OR auth_version = ?)`, userID, expectedAuthVersion, expectedAuthVersion,
	).Scan(&authVersion, &mfaRequired, &isAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return authenticationPolicy{}, ErrUserNotActive
	}
	if err != nil {
		return authenticationPolicy{}, fmt.Errorf("load authentication policy: %w", err)
	}
	return resolveAuthenticationPolicy(authVersion, mfaRequired == 1, isAdmin == 1), nil
}

func (m *Manager) requireAuthenticationAssurance(ctx context.Context, queryer authenticationPolicyQueryer, userID string, expectedAuthVersion int64, assurance AssuranceLevel) (authenticationPolicy, error) {
	policy, err := m.loadAuthenticationPolicy(ctx, queryer, userID, expectedAuthVersion)
	if err != nil {
		return authenticationPolicy{}, err
	}
	if !policy.allowsAssurance(assurance) {
		return authenticationPolicy{}, ErrAuthenticationPolicyNotSatisfied
	}
	return policy, nil
}

func hasRecentSecurityStepUp(session *Session, policy authenticationPolicy, now time.Time) bool {
	if session == nil || session.StepUpAt == nil || !policy.allowsAssurance(session.AssuranceLevel) ||
		!policy.allowsStepUpMethod(session.StepUpMethod) {
		return false
	}
	stepUpAt := session.StepUpAt.UTC()
	return !stepUpAt.After(now) && !stepUpAt.Before(now.Add(-securityStepUpMaximumAge))
}

func (m *Manager) sessionHasRecentSecurityStepUp(ctx context.Context, session *Session, now time.Time) (bool, error) {
	if session == nil {
		return false, nil
	}
	policy, err := m.loadAuthenticationPolicy(ctx, m.db.Read(), session.UserID, session.AuthVersion)
	if errors.Is(err, ErrUserNotActive) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return hasRecentSecurityStepUp(session, policy, now), nil
}

func (m *Manager) requireRecentSecurityStepUp(ctx context.Context, session *Session, now time.Time) error {
	recent, err := m.sessionHasRecentSecurityStepUp(ctx, session, now)
	if err != nil {
		return err
	}
	if !recent {
		return ErrRecentStepUpRequired
	}
	return nil
}

// RequireRecentSecurityStepUp verifies that the bearer identifies an active
// session whose current assurance policy and most recent verification permit a
// sensitive action. Callers must still enforce ownership, role, and CSRF at
// their own boundary.
func (m *Manager) RequireRecentSecurityStepUp(ctx context.Context, sessionToken string) error {
	session, err := m.GetSessionByToken(ctx, sessionToken)
	if err != nil {
		return err
	}
	if session == nil {
		return ErrSecuritySessionInvalid
	}
	return m.requireRecentSecurityStepUp(ctx, session, m.clock.Now().UTC())
}
