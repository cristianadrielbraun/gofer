package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrAdministratorUserMFATargetInvalid = errors.New("administrator user MFA target is invalid")
)

type SetAdministratorUserMFAPolicyOptions struct {
	ActorUserID    string
	ActorSessionID string
	TargetUserID   string
	Required       bool
}

type SetAdministratorUserMFAPolicyResult struct {
	Required bool
	Changed  bool
}

// SetAdministratorUserMFAPolicy changes the individual MFA requirement for
// one webmail user. This transition changes only future authentication and
// factor-removal policy: existing sessions and authenticators are preserved.
// A factorless user is directed into restricted MFA enrollment after their
// next successful primary authentication.
func (m *Manager) SetAdministratorUserMFAPolicy(
	ctx context.Context, options SetAdministratorUserMFAPolicyOptions,
) (*SetAdministratorUserMFAPolicyResult, error) {
	actorUserID := strings.TrimSpace(options.ActorUserID)
	actorSessionID := strings.TrimSpace(options.ActorSessionID)
	targetUserID := strings.TrimSpace(options.TargetUserID)
	if actorUserID == "" {
		return nil, ErrAdministratorRequired
	}
	if actorSessionID == "" {
		return nil, ErrRecentStepUpRequired
	}
	if targetUserID == "" {
		return nil, ErrAdministratorUserMFATargetInvalid
	}

	now := m.clock.Now().UTC()
	result := &SetAdministratorUserMFAPolicyResult{Required: options.Required}
	err := m.runSecurityTransition(ctx, SecurityTransitionPolicyChange, func(tx *sql.Tx) error {
		if err := requireActiveManagementAdministrator(ctx, tx, actorUserID); err != nil {
			return err
		}
		if err := requireRecentAdministratorStepUp(ctx, tx, actorUserID, actorSessionID, now); err != nil {
			return err
		}

		var currentRequired, isAdmin, deletionPending int
		var status UserStatus
		var userType UserType
		err := tx.QueryRowContext(ctx, `
			SELECT status, user_type, is_admin, mfa_required, deletion_pending
			FROM users WHERE id = ?`, targetUserID,
		).Scan(&status, &userType, &isAdmin, &currentRequired, &deletionPending)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAdministratorUserMFATargetInvalid
		}
		if err != nil {
			return fmt.Errorf("load administrator user MFA target: %w", err)
		}
		if userType != UserTypeWebmail || isAdmin != 0 || deletionPending != 0 ||
			(status != UserStatusPending && status != UserStatusActive && status != UserStatusDisabled) {
			return ErrAdministratorUserMFATargetInvalid
		}
		current := currentRequired == 1
		if current == options.Required {
			return nil
		}

		eventID, err := m.tokens.ID()
		if err != nil {
			return fmt.Errorf("generate user MFA policy event ID: %w", err)
		}
		updated, err := tx.ExecContext(ctx, `
			UPDATE users SET mfa_required = ?, updated_at = ?
			WHERE id = ? AND user_type = 'webmail' AND is_admin = 0 AND mfa_required = ?`,
			boolInt(options.Required), now, targetUserID, boolInt(current),
		)
		if err != nil {
			return fmt.Errorf("update user MFA policy: %w", err)
		}
		updatedCount, err := updated.RowsAffected()
		if err != nil {
			return fmt.Errorf("count updated user MFA policies: %w", err)
		}
		if updatedCount != 1 {
			return fmt.Errorf("user MFA policy changed concurrently")
		}

		metadata, err := json.Marshal(struct {
			From bool `json:"from"`
			To   bool `json:"to"`
		}{From: current, To: options.Required})
		if err != nil {
			return fmt.Errorf("encode user MFA policy event: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
				success, reason, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, actorUserID, targetUserID, actorSessionID,
			AuthEventSecurityPolicyChanged, AuthEventReasonAdministratorAction, string(metadata),
		); err != nil {
			return fmt.Errorf("record user MFA policy change: %w", err)
		}
		result.Changed = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
