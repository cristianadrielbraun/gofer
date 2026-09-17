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
	AuthVersion              int64
	RequiresMFA              bool
	MFAEnrollmentRequired    bool
	HasEnrolledMFA           bool
	UserMFARequired          bool
	AdministratorMFARequired bool
	InstanceMFARequired      bool
}

type authenticationPolicyQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func resolveAuthenticationPolicy(authVersion int64, mfaRequired, isAdmin, instanceMFARequired bool) authenticationPolicy {
	return authenticationPolicy{
		AuthVersion:              authVersion,
		RequiresMFA:              mfaRequired || isAdmin || instanceMFARequired,
		MFAEnrollmentRequired:    mfaRequired || isAdmin || instanceMFARequired,
		UserMFARequired:          mfaRequired,
		AdministratorMFARequired: isAdmin,
		InstanceMFARequired:      instanceMFARequired,
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

// allowsExistingSession preserves sessions that were valid before an
// individual user's MFA policy was strengthened or enrolled MFA began being
// enforced at sign-in. Enrollment mutations explicitly revoke other sessions.
// Administrator and instance-wide MFA policy remain immediate session requirements; individual
// policy is enforced when the user's next authentication creates a session.
func (policy authenticationPolicy) allowsExistingSession(assurance AssuranceLevel) bool {
	if !assurance.Valid() {
		return false
	}
	if policy.allowsAssurance(assurance) {
		return true
	}
	return !policy.AdministratorMFARequired && !policy.InstanceMFARequired
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

func queryAuthenticationPolicy(ctx context.Context, queryer authenticationPolicyQueryer, userID string, expectedAuthVersion int64) (authenticationPolicy, error) {
	var authVersion int64
	var mfaRequired, isAdmin int
	var userType UserType
	var instanceMFAPolicy InstanceMFAPolicy
	err := queryer.QueryRowContext(ctx, `
		SELECT u.auth_version, u.mfa_required, u.is_admin, u.user_type,
		       COALESCE((SELECT state.mfa_policy FROM auth_system_state state WHERE state.id = 1), 'administrators')
		FROM users u
		WHERE u.id = ? AND u.status = 'active'
		  AND (? = 0 OR u.auth_version = ?)`, userID, expectedAuthVersion, expectedAuthVersion,
	).Scan(&authVersion, &mfaRequired, &isAdmin, &userType, &instanceMFAPolicy)
	if errors.Is(err, sql.ErrNoRows) {
		return authenticationPolicy{}, ErrUserNotActive
	}
	if err != nil {
		return authenticationPolicy{}, fmt.Errorf("load authentication policy: %w", err)
	}
	if !instanceMFAPolicy.Valid() {
		return authenticationPolicy{}, fmt.Errorf("%w: %q", ErrInstanceMFAPolicyInvalid, instanceMFAPolicy)
	}
	if userType == UserTypeManagement && isAdmin != 1 {
		return authenticationPolicy{}, ErrUserNotActive
	}
	return resolveAuthenticationPolicy(authVersion, mfaRequired == 1, isAdmin == 1, instanceMFAPolicy.RequiresAllUsers()), nil
}

func (m *Manager) loadAuthenticationPolicy(ctx context.Context, queryer authenticationPolicyQueryer, userID string, expectedAuthVersion int64) (authenticationPolicy, error) {
	if err := m.requirePersonalProfile(ctx, queryer, userID); err != nil {
		return authenticationPolicy{}, err
	}
	policy, err := queryAuthenticationPolicy(ctx, queryer, userID, expectedAuthVersion)
	if err != nil {
		return authenticationPolicy{}, err
	}
	return m.withEnrolledMFAPolicy(ctx, queryer, userID, policy)
}

func (m *Manager) withEnrolledMFAPolicy(ctx context.Context, queryer authenticationPolicyQueryer, userID string, policy authenticationPolicy) (authenticationPolicy, error) {
	_, rpID, err := canonicalWebAuthnRelyingParty(m.config.BaseURL)
	if err != nil {
		return authenticationPolicy{}, err
	}
	enrolled, err := userHasStrongAuthenticator(ctx, queryer, userID, rpID)
	if err != nil {
		return authenticationPolicy{}, err
	}
	policy.HasEnrolledMFA = enrolled
	policy.RequiresMFA = policy.MFAEnrollmentRequired || enrolled
	return policy, nil
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
