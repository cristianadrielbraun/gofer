package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"fmt"
)

type PersonalSetupInput struct {
	Token, Origin, UserAgent                       string
	Name, Username, Password, PasswordConfirmation string
}

// CompletePersonalSetup protects the explicit local profile. It never creates a
// management identity or transfers mailbox ownership. Setup state, credentials,
// session revocation, first session and audit event commit together.
func (m *Manager) CompletePersonalSetup(ctx context.Context, input PersonalSetupInput) (*SetupCompletionResult, error) {
	if !m.IsPersonal() {
		return nil, ErrSetupAccessInvalid
	}
	access, err := m.GetActiveSetupAccess(ctx, input.Token, input.Origin)
	if err != nil {
		return nil, err
	}
	if access == nil {
		return nil, ErrSetupAccessInvalid
	}
	draft, err := prepareSetupOwnerDraft(SetupOwnerDraftInput{Mode: SetupOwnerModeCreate, Name: input.Name, Username: input.Username})
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(input.Password), []byte(input.PasswordConfirmation)) != 1 {
		return nil, &SetupPasswordValidationError{Fields: map[string]string{"confirmation": "The password confirmation does not match."}}
	}
	password, err := PrepareNewPassword(input.Password, PasswordPolicyContext{Username: draft.Username})
	if err != nil {
		return nil, &SetupPasswordValidationError{Fields: map[string]string{"password": err.Error()}}
	}
	passwordHash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	sessionID, err := m.tokens.ID()
	if err != nil {
		return nil, err
	}
	sessionToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, err
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, err
	}
	now := m.clock.Now().UTC()
	result := &SetupCompletionResult{OwnerUserID: "default"}
	err = m.runSecurityTransition(ctx, SecurityTransitionSetup, func(tx *sql.Tx) error {
		challenge, err := activeSetupAccessInTransaction(ctx, tx, input.Token, input.Origin, now)
		if err != nil {
			return err
		}
		var incompatible, existing, credentials, badOwners int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN id != 'default' OR user_type != 'webmail' OR is_admin != 0 THEN 1 ELSE 0 END),0) FROM users`).Scan(&existing, &incompatible); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM password_credentials) + (SELECT COUNT(*) FROM webauthn_credentials) + (SELECT COUNT(*) FROM totp_credentials) + (SELECT COUNT(*) FROM auth_identities)`).Scan(&credentials); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE user_id IS NULL OR user_id != 'default'`).Scan(&badOwners); err != nil {
			return err
		}
		if incompatible != 0 || existing > 1 || credentials != 0 || badOwners != 0 {
			return ErrPersonalSetupBlocked
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, username, username_normalized, name, status, auth_version, user_type, is_admin, mfa_required, created_at, updated_at) VALUES ('default', ?, ?, ?, 'active', 1, 'webmail', 0, 0, ?, ?) ON CONFLICT(id) DO UPDATE SET username=excluded.username, username_normalized=excluded.username_normalized, name=excluded.name, status='active', auth_version=users.auth_version+1, mfa_required=0, updated_at=excluded.updated_at`, draft.Username, draft.UsernameNormalized, draft.Name, now, now); err != nil {
			return fmt.Errorf("save personal profile: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO password_credentials (user_id,password_hash,must_change,created_at,changed_at) VALUES ('default',?,0,?,?)`, passwordHash, now, now); err != nil {
			return err
		}
		revoked, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=?, revoked_by='default', revocation_reason=? WHERE revoked_at IS NULL`, now, SessionRevocationAdminAction)
		if err != nil {
			return err
		}
		result.RevokedSessions, err = revoked.RowsAffected()
		if err != nil {
			return err
		}
		state, err := tx.ExecContext(ctx, `UPDATE auth_system_state SET initialized=1, owner_user_id='default', initialized_at=?, setup_token_hash=NULL, setup_expires_at=NULL, setup_attempts=0, cutover_version=?, mfa_policy='administrators' WHERE id=1 AND initialized=0 AND setup_token_hash IS NOT NULL`, now, setupCutoverVersion)
		if err != nil {
			return err
		}
		changed, err := state.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrSetupAlreadyInitialized
		}
		if _, err := tx.ExecContext(ctx, `UPDATE auth_challenges SET consumed_at=?, payload_ciphertext=NULL WHERE id=?`, now, challenge.ID); err != nil {
			return err
		}
		var version int64
		if err := tx.QueryRowContext(ctx, `SELECT auth_version FROM users WHERE id='default'`).Scan(&version); err != nil {
			return err
		}
		if _, err := m.requireAuthenticationAssurance(ctx, tx, "default", version, AssuranceLevelSingleFactor); err != nil {
			return err
		}
		idle, absolute := now.Add(sessionIdleLifetime), now.Add(sessionAbsoluteLifetime)
		if idle.After(absolute) {
			idle = absolute
		}
		agent := boundedUserAgent(input.UserAgent)
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id,user_id,token_hash,auth_version,authentication_method,assurance_level,user_agent,authenticated_at,last_used_at,idle_expires_at,absolute_expires_at,step_up_at,step_up_method,created_at) VALUES (?,'default',?,?,?,?,?,?,?,?,?,?,?,?)`, sessionID, hashToken(sessionToken), version, AuthenticationMethodPassword, AssuranceLevelSingleFactor, agent, now, now, idle, absolute, now, AuthenticationMethodPassword, now); err != nil {
			return err
		}
		topology := SetupOwnerTopology{Kind: SetupOwnerTopologyFresh}
		if existing == 1 {
			topology.Kind = SetupOwnerTopologyLegacyDefault
			draft.Mode = SetupOwnerModeExisting
		}
		metadata, err := setupCompletionEventJSON(topology, draft, result.RevokedSessions)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_events (id,occurred_at,actor_user_id,subject_user_id,session_id,event_type,success,reason,user_agent,metadata_json) VALUES (?,?,'default','default',?,?,1,?,?,?)`, eventID, now, sessionID, AuthEventSetupCompleted, AuthEventReasonSystemInitialization, agent, metadata); err != nil {
			return err
		}
		result.Session = &Session{ID: sessionID, UserID: "default", Token: sessionToken, AuthVersion: version, AuthenticationMethod: AuthenticationMethodPassword, AssuranceLevel: AssuranceLevelSingleFactor, UserAgent: agent, AuthenticatedAt: now, LastUsedAt: now, IdleExpiresAt: idle, AbsoluteExpiresAt: absolute, StepUpAt: &now, StepUpMethod: AuthenticationMethodPassword, CreatedAt: now}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
