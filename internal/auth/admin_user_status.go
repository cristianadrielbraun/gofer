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
	ErrAdministratorUserStatusInvalid       = errors.New("administrator user status is invalid")
	ErrAdministratorUserStatusTargetInvalid = errors.New("administrator user status target is invalid")
)

type SetAdministratorUserStatusOptions struct {
	ActorUserID    string
	ActorSessionID string
	TargetUserID   string
	Status         UserStatus
}

type SetAdministratorUserStatusResult struct {
	Status          UserStatus
	Changed         bool
	RevokedSessions int64
}

// SetAdministratorUserStatus disables or enables an existing non-current
// application user. Disabling invalidates every active session immediately;
// enabling never restores those sessions or changes any credential.
func (m *Manager) SetAdministratorUserStatus(
	ctx context.Context, options SetAdministratorUserStatusOptions,
) (*SetAdministratorUserStatusResult, error) {
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
		return nil, ErrAdministratorUserStatusTargetInvalid
	}
	if options.Status != UserStatusActive && options.Status != UserStatusDisabled {
		return nil, ErrAdministratorUserStatusInvalid
	}

	now := m.clock.Now().UTC()
	result := &SetAdministratorUserStatusResult{Status: options.Status}
	err := m.runSecurityTransition(ctx, SecurityTransitionUserStatus, func(tx *sql.Tx) error {
		if err := requireActiveManagementAdministrator(ctx, tx, actorUserID); err != nil {
			return err
		}
		if err := requireRecentAdministratorStepUp(ctx, tx, actorUserID, actorSessionID, now); err != nil {
			return err
		}
		if targetUserID == actorUserID {
			return ErrAdministratorUserStatusTargetInvalid
		}

		var currentStatus UserStatus
		var deletionPending int
		if err := tx.QueryRowContext(ctx, `SELECT status, deletion_pending FROM users WHERE id = ?`, targetUserID).Scan(&currentStatus, &deletionPending); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrAdministratorUserStatusTargetInvalid
			}
			return fmt.Errorf("load administrator user status target: %w", err)
		}
		if currentStatus != UserStatusActive && currentStatus != UserStatusDisabled {
			return ErrAdministratorUserStatusTargetInvalid
		}
		if deletionPending != 0 {
			return ErrAdministratorUserStatusTargetInvalid
		}
		if currentStatus == options.Status {
			return nil
		}

		eventID, err := m.tokens.ID()
		if err != nil {
			return fmt.Errorf("generate user status event ID: %w", err)
		}
		transition, err := m.setUserStatusTx(ctx, tx, targetUserID, options.Status, actorUserID, now)
		if err != nil {
			return err
		}
		metadata, err := json.Marshal(struct {
			From            UserStatus `json:"from"`
			To              UserStatus `json:"to"`
			RevokedSessions int64      `json:"revoked_sessions"`
		}{
			From: transition.From, To: transition.To,
			RevokedSessions: transition.RevokedSessions,
		})
		if err != nil {
			return fmt.Errorf("encode user status event: %w", err)
		}
		eventType := AuthEventUserDisabled
		if options.Status == UserStatusActive {
			eventType = AuthEventUserEnabled
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
				success, reason, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, actorUserID, targetUserID, actorSessionID,
			eventType, AuthEventReasonAdministratorAction, string(metadata),
		); err != nil {
			return fmt.Errorf("record user status change: %w", err)
		}
		result.Changed = transition.Changed
		result.RevokedSessions = transition.RevokedSessions
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
