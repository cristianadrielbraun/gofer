package auth

import (
	"context"
	"database/sql"
	"fmt"
)

const RequiredPasswordChangePath = "/account/change-password"

type RequirePasswordChangeOptions struct {
	UserID         string
	CreatedBy      string
	ActorSessionID string
}

// RequirePasswordChange creates no reset token. It restricts the existing
// credential and ends sessions atomically with the administrator audit event.
func (m *Manager) RequirePasswordChange(ctx context.Context, options RequirePasswordChangeOptions) error {
	now := m.clock.Now().UTC()
	return m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		if err := requireActiveManagementAdministrator(ctx, tx, options.CreatedBy); err != nil {
			return err
		}
		if err := requireRecentAdministratorStepUp(ctx, tx, options.CreatedBy, options.ActorSessionID, now); err != nil {
			return err
		}
		if err := requireEnrollmentTokenTarget(ctx, tx, options.UserID, EnrollmentTokenPurposeCredentialReset); err != nil {
			return err
		}
		var required bool
		if err := tx.QueryRowContext(ctx, `SELECT must_change FROM password_credentials WHERE user_id = ?`, options.UserID).Scan(&required); err != nil {
			if err == sql.ErrNoRows {
				return ErrEnrollmentTokenTargetInvalid
			}
			return fmt.Errorf("read password-change requirement: %w", err)
		}
		if required {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE password_credentials SET must_change = 1 WHERE user_id = ?`, options.UserID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET auth_version = auth_version + 1, updated_at = ? WHERE id = ?`, now, options.UserID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at = ?, revoked_by = ?, revocation_reason = ? WHERE user_id = ? AND revoked_at IS NULL`, now, options.CreatedBy, SessionRevocationCredentialReset, options.UserID); err != nil {
			return err
		}
		return m.appendTransitionEvent(ctx, tx, options.CreatedBy, options.UserID, options.ActorSessionID, AuthEventPasswordChangeRequired, true, AuthEventReasonAdministratorAction, transitionEventMetadata{})
	})
}
