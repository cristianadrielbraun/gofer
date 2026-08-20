package auth

import (
	"context"
	"fmt"
)

// ListAdministratorUsers returns only application-login profile metadata for
// the active administrator user list. It deliberately excludes mailbox,
// message, contact, credential, factor, identity, session, and token data.
func (m *Manager) ListAdministratorUsers(ctx context.Context, actorUserID string) ([]AdministratorUserSummary, error) {
	tx, err := m.db.Read().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin administrator user list: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireActiveAdministrator(ctx, tx, actorUserID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, COALESCE(username, ''), email, status, is_admin
		FROM users
		ORDER BY COALESCE(
			NULLIF(trim(username_normalized), ''),
			NULLIF(trim(email_normalized), ''),
			lower(trim(email)),
			id
		), id`)
	if err != nil {
		return nil, fmt.Errorf("list administrator users: %w", err)
	}
	defer rows.Close()
	users := make([]AdministratorUserSummary, 0)
	for rows.Next() {
		var user AdministratorUserSummary
		var isAdmin int
		if err := rows.Scan(&user.ID, &user.Username, &user.Email, &user.Status, &isAdmin); err != nil {
			return nil, fmt.Errorf("scan administrator user: %w", err)
		}
		if user.Status != UserStatusPending && user.Status != UserStatusActive && user.Status != UserStatusDisabled {
			return nil, fmt.Errorf("user %q has invalid status %q", user.ID, user.Status)
		}
		if isAdmin != 0 && isAdmin != 1 {
			return nil, fmt.Errorf("user %q has invalid administrator state", user.ID)
		}
		user.IsAdmin = isAdmin == 1
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
