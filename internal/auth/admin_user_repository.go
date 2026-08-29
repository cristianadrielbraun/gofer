package auth

import (
	"context"
	"database/sql"
	"fmt"
)

// ListAdministratorUsers returns application-login profile metadata without
// any invitation action references. Call ListAdministratorUsersForSession for
// an interactive administrator page that needs invitation lifecycle actions.
func (m *Manager) ListAdministratorUsers(ctx context.Context, actorUserID string) ([]AdministratorUserSummary, error) {
	return m.listAdministratorUsers(ctx, actorUserID, "")
}

// ListAdministratorUsersForSession also returns bounded invitation lifecycle
// metadata and opaque action references bound to the exact actor session. Raw
// enrollment token values, hashes, and database token IDs are never returned.
func (m *Manager) ListAdministratorUsersForSession(ctx context.Context, actorUserID, actorSessionID string) ([]AdministratorUserSummary, error) {
	return m.listAdministratorUsers(ctx, actorUserID, actorSessionID)
}

func (m *Manager) listAdministratorUsers(ctx context.Context, actorUserID, actorSessionID string) ([]AdministratorUserSummary, error) {
	tx, err := m.db.Read().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin administrator user list: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireActiveManagementAdministrator(ctx, tx, actorUserID); err != nil {
		return nil, err
	}
	now := m.clock.Now().UTC()
	rows, err := tx.QueryContext(ctx, `
		SELECT u.id, u.username, u.status, u.user_type, u.is_admin, u.mfa_required,
		       u.password_reset_requested_at, u.deletion_pending,
		       EXISTS(SELECT 1 FROM auth_system_state state WHERE state.id = 1 AND state.owner_user_id = u.id),
		       invitation.id, invitation.expires_at, invitation.used_at, invitation.revoked_at
		FROM users u
		LEFT JOIN user_enrollment_tokens invitation ON invitation.id = (
			SELECT candidate.id
			FROM user_enrollment_tokens candidate
			WHERE candidate.user_id = u.id AND candidate.purpose = 'enrollment'
			ORDER BY CASE
				WHEN candidate.used_at IS NULL AND candidate.revoked_at IS NULL
				 AND candidate.expires_at > ? THEN 0 ELSE 1
			END, candidate.created_at DESC, candidate.id DESC
			LIMIT 1
		)
		ORDER BY u.username_normalized, u.id`, now)
	if err != nil {
		return nil, fmt.Errorf("list administrator users: %w", err)
	}
	defer rows.Close()
	users := make([]AdministratorUserSummary, 0)
	for rows.Next() {
		var user AdministratorUserSummary
		var isAdmin, mfaRequired, deletionPending, deletionProtected int
		var tokenID sql.NullString
		var passwordResetRequestedAt, expiresAt, usedAt, revokedAt sql.NullTime
		if err := rows.Scan(
			&user.ID, &user.Username, &user.Status, &user.UserType, &isAdmin, &mfaRequired,
			&passwordResetRequestedAt, &deletionPending, &deletionProtected,
			&tokenID, &expiresAt, &usedAt, &revokedAt,
		); err != nil {
			return nil, fmt.Errorf("scan administrator user: %w", err)
		}
		if user.Status != UserStatusPending && user.Status != UserStatusActive && user.Status != UserStatusDisabled {
			return nil, fmt.Errorf("user %q has invalid status %q", user.ID, user.Status)
		}
		if isAdmin != 0 && isAdmin != 1 {
			return nil, fmt.Errorf("user %q has invalid administrator state", user.ID)
		}
		if mfaRequired != 0 && mfaRequired != 1 {
			return nil, fmt.Errorf("user %q has invalid MFA requirement", user.ID)
		}
		if deletionPending != 0 && deletionPending != 1 {
			return nil, fmt.Errorf("user %q has invalid deletion state", user.ID)
		}
		if deletionProtected != 0 && deletionProtected != 1 {
			return nil, fmt.Errorf("user %q has invalid deletion protection state", user.ID)
		}
		if !user.UserType.Valid() {
			return nil, fmt.Errorf("user %q has invalid type %q", user.ID, user.UserType)
		}
		user.IsAdmin = isAdmin == 1
		user.MFARequired = mfaRequired == 1
		user.DeletionPending = deletionPending == 1
		user.DeletionProtected = deletionProtected == 1
		if passwordResetRequestedAt.Valid {
			requestedAt := passwordResetRequestedAt.Time
			user.PasswordResetRequestedAt = &requestedAt
		}
		if user.Status == UserStatusPending && user.UserType == UserTypeWebmail && !user.IsAdmin {
			user.InvitationState = AdministratorUserInvitationNotIssued
			switch {
			case tokenID.Valid && !usedAt.Valid && !revokedAt.Valid && expiresAt.Valid && expiresAt.Time.After(now):
				user.InvitationState = AdministratorUserInvitationActive
				invitationExpiry := expiresAt.Time
				user.InvitationExpiresAt = &invitationExpiry
			case tokenID.Valid && revokedAt.Valid:
				user.InvitationState = AdministratorUserInvitationRevoked
			case tokenID.Valid && !usedAt.Valid && expiresAt.Valid && !expiresAt.Time.After(now):
				user.InvitationState = AdministratorUserInvitationExpired
				invitationExpiry := expiresAt.Time
				user.InvitationExpiresAt = &invitationExpiry
			}
			if actorSessionID != "" {
				user.InvitationActionReference, err = m.administratorUserInvitationActionReference(actorSessionID, user.ID)
				if err != nil {
					return nil, err
				}
			}
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate administrator users: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close administrator user list: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit administrator user list: %w", err)
	}
	return users, nil
}
