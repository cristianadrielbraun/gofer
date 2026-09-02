package storage

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// ListAdminWebmailUsers returns every retained webmail owner available to the
// operational admin diagnostics. Management-only identities are intentionally
// excluded because they cannot own mailbox accounts or webmail data.
func (db *DB) ListAdminWebmailUsers(ctx context.Context) ([]models.AdminWebmailUserOption, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT id, username, status
		FROM users
		WHERE user_type = 'webmail' AND COALESCE(deletion_pending, 0) = 0
		ORDER BY username_normalized COLLATE NOCASE, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := make([]models.AdminWebmailUserOption, 0)
	for rows.Next() {
		var user models.AdminWebmailUserOption
		if err := rows.Scan(&user.ID, &user.Username, &user.Status); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}
