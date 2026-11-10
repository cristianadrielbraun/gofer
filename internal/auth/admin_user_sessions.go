package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/authz"
)

// ErrAdministratorUserSignOutTargetInvalid: only someone else, active and
// not being deleted, can be signed out by an administrator.
var ErrAdministratorUserSignOutTargetInvalid = errors.New("administrator user sign-out target is invalid")

type SignOutAdministratorUserOptions struct {
	ActorUserID    string
	ActorSessionID string
	TargetUserID   string
}

type SignOutAdministratorUserResult struct {
	// RevokedSessions is how many sessions ended; none means the person
	// wasn't signed in anywhere, and nothing was recorded.
	RevokedSessions int64
}

// SignOutAdministratorUser ends every session another active person has,
// without changing their credentials, status or auth version: they can sign
// in again straight away. It records the change when it ended any session.
func (m *Manager) SignOutAdministratorUser(ctx context.Context, options SignOutAdministratorUserOptions) (*SignOutAdministratorUserResult, error) {
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
		return nil, ErrAdministratorUserSignOutTargetInvalid
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate administrator sign-out event ID: %w", err)
	}

	now := m.clock.Now().UTC()
	result := &SignOutAdministratorUserResult{}
	err = m.runSecurityTransition(ctx, SecurityTransitionSessionRevocation, func(tx *sql.Tx) error {
		if err := requireAdministratorPermission(ctx, tx, actorUserID, actorSessionID, authz.UsersAccess, now); err != nil {
			return err
		}
		if targetUserID == actorUserID {
			return ErrAdministratorUserSignOutTargetInvalid
		}
		var status UserStatus
		var deletionPending int
		var authVersion int64
		if err := tx.QueryRowContext(ctx, `SELECT status, deletion_pending, auth_version FROM users WHERE id = ?`, targetUserID).
			Scan(&status, &deletionPending, &authVersion); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrAdministratorUserSignOutTargetInvalid
			}
			return fmt.Errorf("load administrator sign-out target: %w", err)
		}
		if status != UserStatusActive || deletionPending != 0 {
			return ErrAdministratorUserSignOutTargetInvalid
		}

		// Sessions that expired, or belong to an older auth version, are
		// over already; only the ones still working count.
		revoked, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
			WHERE user_id = ? AND auth_version = ?
			  AND revoked_at IS NULL AND idle_expires_at > ? AND absolute_expires_at > ?`,
			now, actorUserID, SessionRevocationAdminAction, targetUserID, authVersion, now, now,
		)
		if err != nil {
			return fmt.Errorf("revoke administrator sign-out sessions: %w", err)
		}
		result.RevokedSessions, err = revoked.RowsAffected()
		if err != nil {
			return fmt.Errorf("count administrator sign-out sessions: %w", err)
		}
		if result.RevokedSessions == 0 {
			return nil
		}

		metadata, err := json.Marshal(struct {
			Scope           string `json:"scope"`
			RevokedSessions int64  `json:"revoked_sessions"`
		}{Scope: "all", RevokedSessions: result.RevokedSessions})
		if err != nil {
			return fmt.Errorf("encode administrator sign-out event: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
				success, reason, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, actorUserID, targetUserID, actorSessionID,
			AuthEventSessionRevoked, AuthEventReasonAdministratorAction, string(metadata),
		); err != nil {
			return fmt.Errorf("record administrator sign-out: %w", err)
		}
		return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
			Action:        "user.signed_out",
			Target:        audit.Target{Kind: auditTargetUser, ID: targetUserID},
			Metadata:      map[string]any{"revoked_sessions": result.RevokedSessions},
			CorrelationID: eventID,
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
