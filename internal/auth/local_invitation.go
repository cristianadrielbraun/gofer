package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrLocalInvitationSetupIncomplete = errors.New("authentication setup is not complete")

// CreateUserInvitationLocally creates the same pending user and enrollment
// token as an administrator invitation, issued by a local operator instead of
// a signed-in administrator. The caller must hold Gofer's exclusive runtime
// lock. The raw token is returned only here; SQLite stores only its hash.
func (m *Manager) CreateUserInvitationLocally(ctx context.Context, name, username string, lifetime time.Duration) (*AdministratorUserInvitation, error) {
	invitation, err := m.newUserInvitation(name, username, lifetime, "")
	if err != nil {
		return nil, err
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate local invitation event ID: %w", err)
	}
	result := invitation.result
	err = m.runSecurityTransition(ctx, SecurityTransitionEnrollment, func(tx *sql.Tx) error {
		var initialized int
		if err := tx.QueryRowContext(ctx, `SELECT initialized FROM auth_system_state WHERE id = 1`).Scan(&initialized); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read authentication setup state: %w", err)
		}
		if initialized != 1 {
			return ErrLocalInvitationSetupIncomplete
		}
		if err := insertUserInvitation(ctx, tx, invitation); err != nil {
			return err
		}

		metadata, err := enrollmentTokenEventJSON(result.Token.ID, result.Token.Purpose, 0)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, subject_user_id, event_type, success, reason, metadata_json
			) VALUES (?, ?, ?, ?, 1, ?, ?)`,
			eventID, result.Token.CreatedAt, result.User.ID, AuthEventEnrollmentIssued,
			AuthEventReasonLocalOperator, metadata,
		); err != nil {
			return fmt.Errorf("record local invitation event: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
