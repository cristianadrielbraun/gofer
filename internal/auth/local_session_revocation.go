package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrLocalSessionRevocationTargetInvalid = errors.New("user is not eligible for local session revocation")

type LocalSessionRevocationResult struct {
	UserID          string
	RevokedSessions int64
}

// RevokeUserSessionsLocally signs the target out everywhere without changing
// credentials, reset tokens, lifecycle status, or auth version. The caller
// must enforce exact operator confirmation and hold Gofer's exclusive runtime
// lock for the complete transition.
func (m *Manager) RevokeUserSessionsLocally(ctx context.Context, userID string) (*LocalSessionRevocationResult, error) {
	if userID == "" {
		return nil, ErrLocalSessionRevocationTargetInvalid
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate local session revocation event ID: %w", err)
	}

	now := m.clock.Now().UTC()
	result := &LocalSessionRevocationResult{UserID: userID}
	err = m.runSecurityTransition(ctx, SecurityTransitionSessionRevocation, func(tx *sql.Tx) error {
		var status UserStatus
		if err := tx.QueryRowContext(ctx, `SELECT status FROM users WHERE id = ?`, userID).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrLocalSessionRevocationTargetInvalid
			}
			return fmt.Errorf("read local session revocation target: %w", err)
		}
		if status != UserStatusActive && status != UserStatusDisabled {
			return ErrLocalSessionRevocationTargetInvalid
		}

		revoked, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET revoked_at = ?, revoked_by = NULL, revocation_reason = ?
			WHERE user_id = ? AND revoked_at IS NULL`,
			now, SessionRevocationAdminAction, userID,
		)
		if err != nil {
			return fmt.Errorf("revoke local operator sessions: %w", err)
		}
		result.RevokedSessions, err = revoked.RowsAffected()
		if err != nil {
			return fmt.Errorf("count local operator sessions: %w", err)
		}

		metadata, err := localSessionRevocationEventJSON(result.RevokedSessions)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, subject_user_id, event_type, success, reason, metadata_json
			) VALUES (?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, userID, AuthEventSessionRevoked,
			AuthEventReasonLocalOperator, metadata,
		); err != nil {
			return fmt.Errorf("record local session revocation event: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func localSessionRevocationEventJSON(revokedSessions int64) (string, error) {
	metadata := struct {
		RevokedSessions int64 `json:"revoked_sessions"`
	}{RevokedSessions: revokedSessions}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode local session revocation event metadata: %w", err)
	}
	return string(encoded), nil
}
