package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LocalProfileUserID is the single profile of open and personal installations.
const LocalProfileUserID = "default"

// localProfilePasswordLinkLifetime gives the profile a day to set its first
// password after a converted open installation starts in managed mode.
const localProfilePasswordLinkLifetime = 24 * time.Hour

// LocalProfile describes the open or personal profile of a converted
// installation while managed setup has not created an administrator yet.
type LocalProfile struct {
	Username       string
	HasCredentials bool
}

// AdoptLocalProfileForManagedSetup turns a converted personal installation,
// whose local profile still owns the instance, into one awaiting managed
// setup: the profile becomes a regular user that keeps its credentials, and
// first-run setup creates a separate administrator. Open installations never
// completed setup and need no change. It only matches an instance whose sole
// user is the local profile, which managed mode cannot otherwise produce, so
// running it on every managed start is safe and finishes an interrupted run.
func (m *Manager) AdoptLocalProfileForManagedSetup(ctx context.Context) (bool, error) {
	if m.config.AuthenticationMode() != ModeManaged {
		return false, nil
	}
	adopted := false
	err := m.runSecurityTransition(ctx, SecurityTransitionSetup, func(tx *sql.Tx) error {
		var users, owned int
		if err := tx.QueryRowContext(ctx, `SELECT
		 (SELECT COUNT(*) FROM users),
		 (SELECT COUNT(*) FROM auth_system_state state JOIN users u ON u.id = state.owner_user_id
		  WHERE state.id = 1 AND state.initialized = 1 AND u.id = ? AND u.user_type = 'webmail' AND u.is_admin = 0)`,
			LocalProfileUserID).Scan(&users, &owned); err != nil {
			return fmt.Errorf("read local profile ownership: %w", err)
		}
		if users != 1 || owned != 1 {
			return nil
		}
		now := m.clock.Now().UTC()
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_system_state
			SET initialized = 0, owner_user_id = NULL, initialized_at = NULL,
			    setup_token_hash = NULL, setup_expires_at = NULL, setup_attempts = 0, setup_rotated_at = NULL
			WHERE id = 1 AND initialized = 1 AND owner_user_id = ?`, LocalProfileUserID); err != nil {
			return fmt.Errorf("reopen managed setup: %w", err)
		}
		// The profile is no longer the instance owner; existing sessions
		// were issued under personal mode and must sign in again.
		if _, err := tx.ExecContext(ctx, `UPDATE users SET auth_version = auth_version + 1, updated_at = ? WHERE id = ?`, now, LocalProfileUserID); err != nil {
			return fmt.Errorf("invalidate local profile sessions: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions SET revoked_at = ?, revoked_by = NULL, revocation_reason = ?
			WHERE user_id = ? AND revoked_at IS NULL`, now, SessionRevocationRoleChanged, LocalProfileUserID); err != nil {
			return fmt.Errorf("revoke local profile sessions: %w", err)
		}
		adopted = true
		return nil
	})
	return adopted, err
}

// PendingLocalProfile reports the local profile of a managed instance that
// has not completed setup, or nil when there is none.
func (m *Manager) PendingLocalProfile(ctx context.Context) (*LocalProfile, error) {
	if m.config.AuthenticationMode() != ModeManaged {
		return nil, nil
	}
	state, err := m.SetupState(ctx)
	if err != nil || state.Initialized {
		return nil, err
	}
	var profile LocalProfile
	err = m.db.Read().QueryRowContext(ctx, `
		SELECT username,
		       EXISTS(SELECT 1 FROM password_credentials WHERE user_id = users.id)
		       OR EXISTS(SELECT 1 FROM webauthn_credentials WHERE user_id = users.id AND revoked_at IS NULL)
		       OR EXISTS(SELECT 1 FROM auth_identities WHERE user_id = users.id)
		FROM users
		WHERE id = ? AND user_type = 'webmail' AND is_admin = 0 AND status = 'active' AND deletion_pending = 0`,
		LocalProfileUserID).Scan(&profile.Username, &profile.HasCredentials)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read local profile: %w", err)
	}
	return &profile, nil
}

// IssueLocalProfilePasswordLink gives a profile without credentials a token
// to set its first password, replacing any earlier unused one. The raw token
// is returned only here. The caller holds Gofer's exclusive runtime lock.
func (m *Manager) IssueLocalProfilePasswordLink(ctx context.Context) (*EnrollmentToken, error) {
	result, err := m.recoverUserLocally(ctx, LocalProfileUserID, localProfilePasswordLinkLifetime)
	if err != nil {
		return nil, err
	}
	return &result.Token, nil
}
