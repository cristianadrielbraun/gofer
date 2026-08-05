package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type AuthenticationMethod string

const (
	AuthenticationMethodLegacy             AuthenticationMethod = "legacy"
	AuthenticationMethodPassword           AuthenticationMethod = "password"
	AuthenticationMethodPasskey            AuthenticationMethod = "passkey"
	AuthenticationMethodTOTP               AuthenticationMethod = "totp"
	AuthenticationMethodRecoveryCode       AuthenticationMethod = "recovery_code"
	AuthenticationMethodFederatedGoogle    AuthenticationMethod = "federated_google"
	AuthenticationMethodFederatedMicrosoft AuthenticationMethod = "federated_microsoft"
	AuthenticationMethodFederatedOIDC      AuthenticationMethod = "federated_oidc"
)

type AssuranceLevel string

const (
	AssuranceLevelLegacy            AssuranceLevel = "legacy"
	AssuranceLevelSingleFactor      AssuranceLevel = "single_factor"
	AssuranceLevelMultiFactor       AssuranceLevel = "multi_factor"
	AssuranceLevelPhishingResistant AssuranceLevel = "phishing_resistant"
)

type ChallengePurpose string

const (
	ChallengePurposeLogin          ChallengePurpose = "login"
	ChallengePurposeMFA            ChallengePurpose = "mfa"
	ChallengePurposeEnrollment     ChallengePurpose = "enrollment"
	ChallengePurposeRecovery       ChallengePurpose = "recovery"
	ChallengePurposeStepUp         ChallengePurpose = "step_up"
	ChallengePurposeFederatedLogin ChallengePurpose = "federated_login"
)

type SessionRevocationReason string

const (
	SessionRevocationLogout            SessionRevocationReason = "logout"
	SessionRevocationUserDisabled      SessionRevocationReason = "user_disabled"
	SessionRevocationUserStatusChanged SessionRevocationReason = "user_status_changed"
	SessionRevocationCredentialReset   SessionRevocationReason = "credential_reset"
	SessionRevocationAdminAction       SessionRevocationReason = "admin_action"
	SessionRevocationExpired           SessionRevocationReason = "expired"
	SessionRevocationRotation          SessionRevocationReason = "rotation"
	SessionRevocationRoleChanged       SessionRevocationReason = "role_changed"
)

func (method AuthenticationMethod) Valid() bool {
	switch method {
	case AuthenticationMethodLegacy,
		AuthenticationMethodPassword,
		AuthenticationMethodPasskey,
		AuthenticationMethodTOTP,
		AuthenticationMethodRecoveryCode,
		AuthenticationMethodFederatedGoogle,
		AuthenticationMethodFederatedMicrosoft,
		AuthenticationMethodFederatedOIDC:
		return true
	default:
		return false
	}
}

func (level AssuranceLevel) Valid() bool {
	switch level {
	case AssuranceLevelLegacy,
		AssuranceLevelSingleFactor,
		AssuranceLevelMultiFactor,
		AssuranceLevelPhishingResistant:
		return true
	default:
		return false
	}
}

func (purpose ChallengePurpose) Valid() bool {
	switch purpose {
	case ChallengePurposeLogin,
		ChallengePurposeMFA,
		ChallengePurposeEnrollment,
		ChallengePurposeRecovery,
		ChallengePurposeStepUp,
		ChallengePurposeFederatedLogin:
		return true
	default:
		return false
	}
}

func (reason SessionRevocationReason) Valid() bool {
	switch reason {
	case SessionRevocationLogout,
		SessionRevocationUserDisabled,
		SessionRevocationUserStatusChanged,
		SessionRevocationCredentialReset,
		SessionRevocationAdminAction,
		SessionRevocationExpired,
		SessionRevocationRotation,
		SessionRevocationRoleChanged:
		return true
	default:
		return false
	}
}

type AuthEventType string

const (
	AuthEventLoginSucceeded    AuthEventType = "login_succeeded"
	AuthEventLoginFailed       AuthEventType = "login_failed"
	AuthEventSessionRevoked    AuthEventType = "session_revoked"
	AuthEventUserDisabled      AuthEventType = "user_disabled"
	AuthEventCredentialChanged AuthEventType = "credential_changed"
	AuthEventRecoveryUsed      AuthEventType = "recovery_used"
	AuthEventEnrollmentIssued  AuthEventType = "enrollment_token_issued"
	AuthEventEnrollmentRevoked AuthEventType = "enrollment_token_revoked"
)

type AuthEventReason string

const (
	AuthEventReasonInvalidCredentials  AuthEventReason = "invalid_credentials"
	AuthEventReasonUserInactive        AuthEventReason = "user_inactive"
	AuthEventReasonChallengeExpired    AuthEventReason = "challenge_expired"
	AuthEventReasonChallengeConsumed   AuthEventReason = "challenge_consumed"
	AuthEventReasonThrottled           AuthEventReason = "throttled"
	AuthEventReasonPolicyRequired      AuthEventReason = "policy_required"
	AuthEventReasonAdministratorAction AuthEventReason = "administrator_action"
)

type SecurityTransition string

const (
	SecurityTransitionSetup            SecurityTransition = "setup"
	SecurityTransitionLoginCompletion  SecurityTransition = "login_completion"
	SecurityTransitionLoginThrottle    SecurityTransition = "login_throttle"
	SecurityTransitionCredentialChange SecurityTransition = "credential_change"
	SecurityTransitionEnrollment       SecurityTransition = "enrollment"
	SecurityTransitionRecovery         SecurityTransition = "recovery"
	SecurityTransitionSessionRotation  SecurityTransition = "session_rotation"
	SecurityTransitionUserStatus       SecurityTransition = "user_status"
	SecurityTransitionRoleChange       SecurityTransition = "role_change"
)

type Clock interface {
	Now() time.Time
	NewTicker(time.Duration) Ticker
}

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type TokenGenerator interface {
	Token(byteCount int) (string, error)
	ID() (string, error)
}

type Dependencies struct {
	Clock         Clock
	Tokens        TokenGenerator
	BucketHashKey []byte
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
func (systemClock) NewTicker(interval time.Duration) Ticker {
	return systemTicker{time.NewTicker(interval)}
}

type systemTicker struct{ ticker *time.Ticker }

func (ticker systemTicker) C() <-chan time.Time { return ticker.ticker.C }
func (ticker systemTicker) Stop()               { ticker.ticker.Stop() }

type secureTokenGenerator struct{}

func (secureTokenGenerator) Token(byteCount int) (string, error) {
	if byteCount <= 0 {
		return "", fmt.Errorf("token byte count must be positive")
	}
	value := make([]byte, byteCount)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func (secureTokenGenerator) ID() (string, error) {
	value, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("generate random ID: %w", err)
	}
	return value.String(), nil
}

func hashToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

func (m *Manager) runSecurityTransition(ctx context.Context, boundary SecurityTransition, action func(*sql.Tx) error) error {
	if boundary == "" {
		return fmt.Errorf("security transition boundary is required")
	}
	tx, err := m.db.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s transition: %w", boundary, err)
	}
	defer tx.Rollback()
	if err := action(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s transition: %w", boundary, err)
	}
	return nil
}
