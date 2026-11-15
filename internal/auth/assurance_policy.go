package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/policy"
)

var ErrAuthenticationPolicyNotSatisfied = errors.New("authentication assurance does not satisfy policy")

type authenticationPolicy struct {
	AuthVersion         int64
	PasskeyTOTPRequired bool
	// PasskeyCountsAsSecondFactor is an administrator's choice for one
	// person's individual requirement: a verified passkey satisfies it on its
	// own. Administrator and instance-wide requirements always take the app.
	PasskeyCountsAsSecondFactor bool
	RequiresMFA                 bool
	MFAEnrollmentRequired       bool
	HasEnrolledMFA              bool
	UserMFARequired             bool
	AdministratorMFARequired    bool
	InstanceMFARequired         bool
	// PolicyMFARequired: a named policy requires it of the person
	// (internal/policy). Like their individual requirement, it applies at
	// their next sign-in, and their sessions until then stay.
	// PolicyPasskeyCounts: that policy lets a passkey be the second step.
	PolicyMFARequired   bool
	PolicyPasskeyCounts bool
}

type authenticationPolicyQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func resolveAuthenticationPolicy(authVersion int64, mfaRequired, isAdmin, instanceMFARequired bool) authenticationPolicy {
	return authenticationPolicy{
		AuthVersion:              authVersion,
		PasskeyTOTPRequired:      true,
		RequiresMFA:              mfaRequired || isAdmin || instanceMFARequired,
		MFAEnrollmentRequired:    mfaRequired || isAdmin || instanceMFARequired,
		UserMFARequired:          mfaRequired,
		AdministratorMFARequired: isAdmin,
		InstanceMFARequired:      instanceMFARequired,
	}
}

// policyTwoFactor reports whether a named policy requires two-factor
// sign-in of the person, and whether that policy lets a passkey be the
// second step, read with queryer: inside the caller's transaction when it
// has one, so a policy change can't come between the check and what it
// guards.
func policyTwoFactor(ctx context.Context, queryer authenticationPolicyQueryer, userID string) (required, passkeyCounts bool, err error) {
	if err := queryer.QueryRowContext(ctx, policy.TwoFactorForUserSQL(), userID, userID).Scan(&required, &passkeyCounts); err != nil {
		return false, false, fmt.Errorf("check whether a policy requires two-factor sign-in: %w", err)
	}
	return required, passkeyCounts, nil
}

// withPolicyMFA adds a named policy's requirement.
func (p authenticationPolicy) withPolicyMFA(required, passkeyCounts bool) authenticationPolicy {
	if required {
		p.PolicyMFARequired, p.RequiresMFA, p.MFAEnrollmentRequired = true, true, true
		p.PolicyPasskeyCounts = passkeyCounts
	}
	return p
}

func (policy authenticationPolicy) allowsAssurance(assurance AssuranceLevel) bool {
	if !assurance.Valid() {
		return false
	}
	if !policy.RequiresMFA {
		return true
	}
	// Verified passkeys already provide multiple factors. A separate TOTP
	// challenge is optional for webmail users, while passwords and federated
	// sign-ins still obtain multi_factor assurance through TOTP. A required
	// second step accepts a passkey only when the administrator chose that.
	return assurance == AssuranceLevelMultiFactor ||
		(assurance == AssuranceLevelPhishingResistant && !policy.PasskeyTOTPRequired &&
			(!policy.MFAEnrollmentRequired || policy.PasskeyCountsAsSecondFactor))
}

// appMustStay says whether the authenticator app has to stay: a required
// second step needs it, unless that requirement also accepts one of the
// person's passkeys.
func (policy authenticationPolicy) appMustStay(passkeyCount int) bool {
	return policy.MFAEnrollmentRequired && !(policy.PasskeyCountsAsSecondFactor && passkeyCount > 0)
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
	var mfaRequired, isAdmin, passkeyTOTPRequired, mfaPasskeyCounts int
	var userType UserType
	var instanceMFAPolicy InstanceMFAPolicy
	err := queryer.QueryRowContext(ctx, `
		SELECT u.auth_version, u.mfa_required, u.is_admin, u.user_type,
		       u.passkey_totp_required, u.mfa_passkey_counts,
		       COALESCE((SELECT state.mfa_policy FROM auth_system_state state WHERE state.id = 1), 'administrators')
		FROM users u
		WHERE u.id = ? AND u.status = 'active'
		  AND (? = 0 OR u.auth_version = ?)`, userID, expectedAuthVersion, expectedAuthVersion,
	).Scan(&authVersion, &mfaRequired, &isAdmin, &userType, &passkeyTOTPRequired, &mfaPasskeyCounts, &instanceMFAPolicy)
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
	resolved := resolveAuthenticationPolicy(authVersion, mfaRequired == 1, isAdmin == 1, instanceMFAPolicy.RequiresAllUsers())
	// Policies reach webmail users only.
	if userType == UserTypeWebmail && isAdmin != 1 {
		required, passkeyCounts, err := policyTwoFactor(ctx, queryer, userID)
		if err != nil {
			return authenticationPolicy{}, err
		}
		resolved = resolved.withPolicyMFA(required, passkeyCounts)
	}
	policy := resolved
	// An individual requirement and a policy's can each accept a passkey on
	// a webmail user; when both apply, both must. Administrators and the
	// requirement for everyone always take the app.
	policy.PasskeyCountsAsSecondFactor = userType == UserTypeWebmail && isAdmin != 1 && !policy.InstanceMFARequired &&
		(policy.UserMFARequired || policy.PolicyMFARequired) &&
		(!policy.UserMFARequired || mfaPasskeyCounts == 1) &&
		(!policy.PolicyMFARequired || policy.PolicyPasskeyCounts)
	// Management identities are outside this feature; never permit them to
	// bypass the separate factor based on a webmail-only preference.
	policy.PasskeyTOTPRequired = userType != UserTypeWebmail || passkeyTOTPRequired == 1 ||
		(policy.MFAEnrollmentRequired && !policy.PasskeyCountsAsSecondFactor)
	return policy, nil
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
	enrolled, err := userHasSecondFactor(ctx, queryer, userID)
	if err != nil {
		return authenticationPolicy{}, err
	}
	policy.HasEnrolledMFA = enrolled
	policy.RequiresMFA = policy.MFAEnrollmentRequired || enrolled
	if policy.PasskeyCountsAsSecondFactor && !enrolled {
		// "Also ask for the app code after a passkey" can only ask for an
		// app the person has; without one, the passkey is the second step.
		policy.PasskeyTOTPRequired = false
	}
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
