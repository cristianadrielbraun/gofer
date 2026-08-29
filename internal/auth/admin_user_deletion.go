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
	ErrAdministratorUserDeletionTargetInvalid       = errors.New("administrator user deletion target is invalid")
	ErrAdministratorUserDeletionConfirmationInvalid = errors.New("administrator user deletion confirmation is invalid")
	ErrAdministratorUserDeletionIncomplete          = errors.New("administrator user deletion is incomplete")
)

type PrepareAdministratorUserDeletionOptions struct {
	ActorUserID    string
	ActorSessionID string
	TargetUserID   string
	Confirmation   string
}

type PrepareAdministratorUserDeletionResult struct {
	TargetUserID string
	Username     string
	AccountIDs   []string
	Resumed      bool
}

type userDeletionAuditMetadata struct {
	TargetUserID              string `json:"target_user_id"`
	TargetUsername            string `json:"target_username"`
	RemoteProviderDataChanged bool   `json:"remote_provider_data_changed"`
}

// PrepareAdministratorUserDeletion establishes a resumable deletion boundary.
// The target must already be a disabled, ordinary webmail user. Mailboxes are
// marked as deleting in the same transaction so they cannot resume provider
// work while their local data is being purged.
func (m *Manager) PrepareAdministratorUserDeletion(
	ctx context.Context, options PrepareAdministratorUserDeletionOptions,
) (*PrepareAdministratorUserDeletionResult, error) {
	actorUserID := strings.TrimSpace(options.ActorUserID)
	actorSessionID := strings.TrimSpace(options.ActorSessionID)
	targetUserID := strings.TrimSpace(options.TargetUserID)
	if actorUserID == "" {
		return nil, ErrAdministratorRequired
	}
	if actorSessionID == "" {
		return nil, ErrRecentStepUpRequired
	}
	if targetUserID == "" || targetUserID == actorUserID {
		return nil, ErrAdministratorUserDeletionTargetInvalid
	}

	now := m.clock.Now().UTC()
	result := &PrepareAdministratorUserDeletionResult{TargetUserID: targetUserID}
	err := m.runSecurityTransition(ctx, SecurityTransitionUserDeletion, func(tx *sql.Tx) error {
		if err := requireActiveManagementAdministrator(ctx, tx, actorUserID); err != nil {
			return err
		}
		if err := requireRecentAdministratorStepUp(ctx, tx, actorUserID, actorSessionID, now); err != nil {
			return err
		}

		var status UserStatus
		var userType UserType
		var isAdmin, deletionPending int
		if err := tx.QueryRowContext(ctx, `
			SELECT username, status, user_type, is_admin, deletion_pending
			FROM users WHERE id = ?`, targetUserID,
		).Scan(&result.Username, &status, &userType, &isAdmin, &deletionPending); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrAdministratorUserDeletionTargetInvalid
			}
			return fmt.Errorf("load user deletion target: %w", err)
		}
		if status != UserStatusDisabled || userType != UserTypeWebmail || isAdmin != 0 ||
			(deletionPending != 0 && deletionPending != 1) {
			return ErrAdministratorUserDeletionTargetInvalid
		}
		if options.Confirmation != result.Username {
			return ErrAdministratorUserDeletionConfirmationInvalid
		}
		var isSetupOwner int
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM auth_system_state WHERE id = 1 AND owner_user_id = ?)`, targetUserID,
		).Scan(&isSetupOwner); err != nil {
			return fmt.Errorf("check user deletion owner boundary: %w", err)
		}
		if isSetupOwner != 0 {
			return ErrAdministratorUserDeletionTargetInvalid
		}

		result.Resumed = deletionPending == 1
		if !result.Resumed {
			updated, err := tx.ExecContext(ctx, `
				UPDATE users
				SET deletion_pending = 1, deletion_started_at = ?, deletion_started_by = ?, updated_at = ?
				WHERE id = ? AND status = 'disabled' AND user_type = 'webmail' AND is_admin = 0
				  AND deletion_pending = 0`, now, actorUserID, now, targetUserID,
			)
			if err != nil {
				return fmt.Errorf("mark user deletion pending: %w", err)
			}
			updatedRows, err := updated.RowsAffected()
			if err != nil {
				return fmt.Errorf("count pending user deletions: %w", err)
			}
			if updatedRows != 1 {
				return ErrAdministratorUserDeletionTargetInvalid
			}
			eventID, err := m.tokens.ID()
			if err != nil {
				return fmt.Errorf("generate user deletion event ID: %w", err)
			}
			metadata, err := json.Marshal(userDeletionAuditMetadata{
				TargetUserID: targetUserID, TargetUsername: result.Username,
				RemoteProviderDataChanged: false,
			})
			if err != nil {
				return fmt.Errorf("encode user deletion event: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO auth_events (
					id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
					success, reason, metadata_json
				) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
				eventID, now, actorUserID, targetUserID, actorSessionID,
				AuthEventUserDeletionStarted, AuthEventReasonAdministratorAction, string(metadata),
			); err != nil {
				return fmt.Errorf("record user deletion start: %w", err)
			}
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE accounts SET is_deleting = 1, updated_at = ? WHERE user_id = ?`, now, targetUserID,
		); err != nil {
			return fmt.Errorf("mark user mailboxes deleting: %w", err)
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM accounts WHERE user_id = ? ORDER BY id`, targetUserID)
		if err != nil {
			return fmt.Errorf("list user deletion mailboxes: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var accountID string
			if err := rows.Scan(&accountID); err != nil {
				return fmt.Errorf("scan user deletion mailbox: %w", err)
			}
			result.AccountIDs = append(result.AccountIDs, accountID)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate user deletion mailboxes: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// CompleteAdministratorUserDeletion removes the disabled login identity only
// after every owned mailbox row and user-specific compose blob is gone. It is
// safe to call after a restart; the initiating administrator is persisted on
// the pending target.
func (m *Manager) CompleteAdministratorUserDeletion(ctx context.Context, targetUserID, actorSessionID string) (bool, error) {
	targetUserID = strings.TrimSpace(targetUserID)
	actorSessionID = strings.TrimSpace(actorSessionID)
	if targetUserID == "" {
		return false, ErrAdministratorUserDeletionTargetInvalid
	}

	now := m.clock.Now().UTC()
	deleted := false
	err := m.runSecurityTransition(ctx, SecurityTransitionUserDeletion, func(tx *sql.Tx) error {
		var username string
		var status UserStatus
		var userType UserType
		var actorUserID sql.NullString
		var isAdmin, deletionPending int
		err := tx.QueryRowContext(ctx, `
			SELECT username, status, user_type, is_admin, deletion_pending, deletion_started_by
			FROM users WHERE id = ?`, targetUserID,
		).Scan(&username, &status, &userType, &isAdmin, &deletionPending, &actorUserID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load pending user deletion: %w", err)
		}
		if status != UserStatusDisabled || userType != UserTypeWebmail || isAdmin != 0 ||
			deletionPending != 1 || !actorUserID.Valid || strings.TrimSpace(actorUserID.String) == "" {
			return ErrAdministratorUserDeletionTargetInvalid
		}
		var remainingAccounts int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE user_id = ?`, targetUserID).Scan(&remainingAccounts); err != nil {
			return fmt.Errorf("count remaining user mailboxes: %w", err)
		}
		if remainingAccounts != 0 {
			return ErrAdministratorUserDeletionIncomplete
		}

		var eventSession any
		if actorSessionID != "" {
			var exists int
			if err := tx.QueryRowContext(ctx, `
				SELECT EXISTS(SELECT 1 FROM sessions WHERE id = ? AND user_id = ?)`,
				actorSessionID, actorUserID.String,
			).Scan(&exists); err != nil {
				return fmt.Errorf("check user deletion actor session: %w", err)
			}
			if exists == 1 {
				eventSession = actorSessionID
			}
		}
		eventID, err := m.tokens.ID()
		if err != nil {
			return fmt.Errorf("generate user deleted event ID: %w", err)
		}
		metadata, err := json.Marshal(userDeletionAuditMetadata{
			TargetUserID: targetUserID, TargetUsername: username,
			RemoteProviderDataChanged: false,
		})
		if err != nil {
			return fmt.Errorf("encode user deleted event: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
				success, reason, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, actorUserID.String, targetUserID, eventSession,
			AuthEventUserDeleted, AuthEventReasonAdministratorAction, string(metadata),
		); err != nil {
			return fmt.Errorf("record user deletion completion: %w", err)
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ? AND deletion_pending = 1`, targetUserID)
		if err != nil {
			return fmt.Errorf("delete user: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count deleted users: %w", err)
		}
		if rows != 1 {
			return ErrAdministratorUserDeletionTargetInvalid
		}
		deleted = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

func (m *Manager) ListPendingUserDeletionIDs(ctx context.Context) ([]string, error) {
	rows, err := m.db.Read().QueryContext(ctx, `
		SELECT id FROM users WHERE deletion_pending = 1 ORDER BY deletion_started_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list pending user deletions: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan pending user deletion: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending user deletions: %w", err)
	}
	return ids, nil
}
