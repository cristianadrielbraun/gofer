package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type InstanceMFAPolicy string

const (
	InstanceMFAPolicyAdministrators InstanceMFAPolicy = "administrators"
	InstanceMFAPolicyAllUsers       InstanceMFAPolicy = "all_users"
)

var (
	ErrInstanceMFAPolicyInvalid     = errors.New("instance MFA policy is invalid")
	ErrInstanceMFAPolicyUnavailable = errors.New("instance MFA policy is unavailable before setup completes")
	ErrInstanceMFAEnrollmentNeeded  = errors.New("strong authenticator enrollment is required before instance-wide MFA activation")
)

func (policy InstanceMFAPolicy) Valid() bool {
	return policy == InstanceMFAPolicyAdministrators || policy == InstanceMFAPolicyAllUsers
}

func (policy InstanceMFAPolicy) RequiresAllUsers() bool {
	return policy == InstanceMFAPolicyAllUsers
}

type InstanceSecurityPolicy struct {
	MFA       InstanceMFAPolicy
	UpdatedAt *time.Time
	UpdatedBy string
}

type InstanceMFAPolicyEnrollmentError struct {
	ActiveUsersWithoutFactor int
}

func (err *InstanceMFAPolicyEnrollmentError) Error() string {
	return fmt.Sprintf("%s: %d active user(s) are not ready", ErrInstanceMFAEnrollmentNeeded, err.ActiveUsersWithoutFactor)
}

func (err *InstanceMFAPolicyEnrollmentError) Unwrap() error {
	return ErrInstanceMFAEnrollmentNeeded
}

type SetInstanceMFAPolicyOptions struct {
	ActorUserID    string
	ActorSessionID string
	Policy         InstanceMFAPolicy
}

type SetInstanceMFAPolicyResult struct {
	Policy          InstanceSecurityPolicy
	Changed         bool
	RevokedSessions int64
}

type instanceSecurityPolicyQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readInstanceSecurityPolicy(ctx context.Context, queryer instanceSecurityPolicyQueryer) (InstanceSecurityPolicy, error) {
	var rawPolicy string
	var updatedAt sql.NullTime
	var updatedBy sql.NullString
	err := queryer.QueryRowContext(ctx, `
		SELECT mfa_policy, security_policy_updated_at, security_policy_updated_by
		FROM auth_system_state WHERE id = 1`,
	).Scan(&rawPolicy, &updatedAt, &updatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return InstanceSecurityPolicy{MFA: InstanceMFAPolicyAdministrators}, nil
	}
	if err != nil {
		return InstanceSecurityPolicy{}, fmt.Errorf("read instance security policy: %w", err)
	}
	policy := InstanceMFAPolicy(rawPolicy)
	if !policy.Valid() {
		return InstanceSecurityPolicy{}, fmt.Errorf("%w: %q", ErrInstanceMFAPolicyInvalid, rawPolicy)
	}
	result := InstanceSecurityPolicy{MFA: policy, UpdatedBy: updatedBy.String}
	if updatedAt.Valid {
		value := updatedAt.Time
		result.UpdatedAt = &value
	}
	return result, nil
}

// InstanceSecurityPolicy returns the effective persisted instance policy. An
// unprovisioned instance uses the safe compatibility default: MFA for
// administrators, without implying that setup has completed.
func (m *Manager) InstanceSecurityPolicy(ctx context.Context) (InstanceSecurityPolicy, error) {
	return readInstanceSecurityPolicy(ctx, m.db.Read())
}

func userHasStrongAuthenticator(ctx context.Context, queryer instanceSecurityPolicyQueryer, userID, rpID string) (bool, error) {
	var ready int
	if err := queryer.QueryRowContext(ctx, `
		SELECT
			EXISTS (
				SELECT 1 FROM totp_credentials
				WHERE user_id = ? AND enabled = 1 AND revoked_at IS NULL
			)
			OR EXISTS (
				SELECT 1 FROM webauthn_credentials
				WHERE user_id = ? AND rp_id = ? AND revoked_at IS NULL
				  AND credential_ciphertext IS NOT NULL AND key_version IS NOT NULL
			)`,
		userID, userID, rpID,
	).Scan(&ready); err != nil {
		return false, fmt.Errorf("check strong authenticator enrollment: %w", err)
	}
	return ready == 1, nil
}

func (m *Manager) requireUserReadyForInstanceMFA(ctx context.Context, queryer instanceSecurityPolicyQueryer, userID string) error {
	policy, err := readInstanceSecurityPolicy(ctx, queryer)
	if err != nil {
		return err
	}
	if !policy.MFA.RequiresAllUsers() {
		return nil
	}
	_, rpID, err := canonicalWebAuthnRelyingParty(m.config.BaseURL)
	if err != nil {
		return fmt.Errorf("resolve passkey relying party for instance MFA activation: %w", err)
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

// SetInstanceMFAPolicy applies an administrator-authorized policy transition.
// Enabling MFA for every user is refused until each active user has an enabled
// TOTP credential or a complete passkey for this instance. The policy update,
// weak-session revocation, and audit event commit atomically.
func (m *Manager) SetInstanceMFAPolicy(ctx context.Context, options SetInstanceMFAPolicyOptions) (*SetInstanceMFAPolicyResult, error) {
	actorUserID := strings.TrimSpace(options.ActorUserID)
	actorSessionID := strings.TrimSpace(options.ActorSessionID)
	if actorUserID == "" {
		return nil, ErrAdministratorRequired
	}
	if actorSessionID == "" {
		return nil, ErrRecentStepUpRequired
	}
	if !options.Policy.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrInstanceMFAPolicyInvalid, options.Policy)
	}

	rpID := ""
	if options.Policy.RequiresAllUsers() {
		_, resolvedRPID, err := canonicalWebAuthnRelyingParty(m.config.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("resolve passkey relying party for instance MFA policy: %w", err)
		}
		rpID = resolvedRPID
	}
	now := m.clock.Now().UTC()
	result := &SetInstanceMFAPolicyResult{}
	err := m.runSecurityTransition(ctx, SecurityTransitionPolicyChange, func(tx *sql.Tx) error {
		if err := requireActiveAdministrator(ctx, tx, actorUserID); err != nil {
			return err
		}
		if err := requireRecentAdministratorStepUp(ctx, tx, actorUserID, actorSessionID, now); err != nil {
			return err
		}

		var initialized int
		var currentRaw string
		err := tx.QueryRowContext(ctx, `
			SELECT initialized, mfa_policy FROM auth_system_state WHERE id = 1`,
		).Scan(&initialized, &currentRaw)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInstanceMFAPolicyUnavailable
		}
		if err != nil {
			return fmt.Errorf("load current instance MFA policy: %w", err)
		}
		if initialized != 1 {
			return ErrInstanceMFAPolicyUnavailable
		}
		current := InstanceMFAPolicy(currentRaw)
		if !current.Valid() {
			return fmt.Errorf("%w: %q", ErrInstanceMFAPolicyInvalid, currentRaw)
		}
		if current == options.Policy {
			policy, err := readInstanceSecurityPolicy(ctx, tx)
			if err != nil {
				return err
			}
			result.Policy = policy
			return nil
		}

		if options.Policy.RequiresAllUsers() {
			var blockers int
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*)
				FROM users u
				WHERE u.status = 'active'
				  AND NOT EXISTS (
					SELECT 1 FROM totp_credentials t
					WHERE t.user_id = u.id AND t.enabled = 1 AND t.revoked_at IS NULL
				  )
				  AND NOT EXISTS (
					SELECT 1 FROM webauthn_credentials w
					WHERE w.user_id = u.id AND w.rp_id = ? AND w.revoked_at IS NULL
					  AND w.credential_ciphertext IS NOT NULL AND w.key_version IS NOT NULL
				  )`, rpID,
			).Scan(&blockers); err != nil {
				return fmt.Errorf("preflight instance MFA enrollment: %w", err)
			}
			if blockers > 0 {
				return &InstanceMFAPolicyEnrollmentError{ActiveUsersWithoutFactor: blockers}
			}
		}
		eventID, err := m.tokens.ID()
		if err != nil {
			return fmt.Errorf("generate instance MFA policy event ID: %w", err)
		}

		updated, err := tx.ExecContext(ctx, `
			UPDATE auth_system_state
			SET mfa_policy = ?, security_policy_updated_at = ?, security_policy_updated_by = ?
			WHERE id = 1 AND initialized = 1 AND mfa_policy = ?`,
			options.Policy, now, actorUserID, current,
		)
		if err != nil {
			return fmt.Errorf("update instance MFA policy: %w", err)
		}
		rowsAffected, err := updated.RowsAffected()
		if err != nil {
			return fmt.Errorf("count updated instance MFA policies: %w", err)
		}
		if rowsAffected != 1 {
			return fmt.Errorf("instance MFA policy changed concurrently")
		}

		if options.Policy.RequiresAllUsers() {
			revoked, err := tx.ExecContext(ctx, `
				UPDATE sessions
				SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
				WHERE revoked_at IS NULL
				  AND assurance_level NOT IN (?, ?)`,
				now, actorUserID, SessionRevocationAdminAction,
				AssuranceLevelMultiFactor, AssuranceLevelPhishingResistant,
			)
			if err != nil {
				return fmt.Errorf("revoke weak sessions for instance MFA policy: %w", err)
			}
			result.RevokedSessions, err = revoked.RowsAffected()
			if err != nil {
				return fmt.Errorf("count weak sessions revoked for instance MFA policy: %w", err)
			}
		}

		metadata, err := json.Marshal(struct {
			From            InstanceMFAPolicy `json:"from"`
			To              InstanceMFAPolicy `json:"to"`
			RevokedSessions int64             `json:"revoked_sessions"`
		}{From: current, To: options.Policy, RevokedSessions: result.RevokedSessions})
		if err != nil {
			return fmt.Errorf("encode instance MFA policy event: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, session_id, event_type,
				success, reason, metadata_json
			) VALUES (?, ?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, actorUserID, actorSessionID, AuthEventSecurityPolicyChanged,
			AuthEventReasonAdministratorAction, string(metadata),
		); err != nil {
			return fmt.Errorf("record instance MFA policy change: %w", err)
		}

		result.Policy = InstanceSecurityPolicy{
			MFA: options.Policy, UpdatedAt: timePointer(now), UpdatedBy: actorUserID,
		}
		result.Changed = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
