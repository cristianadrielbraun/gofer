package storage

import (
	"context"
	"database/sql"
	"errors"
)

// SetFolderVisibility stores a user-local presentation preference.
// It does not change IMAP subscriptions or synchronization.
//
// Visible is the default. Only hidden folders need a stored setting.
func (db *DB) SetFolderVisibility(
	ctx context.Context,
	owner, folderID string,
	visible bool,
) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var exists int
	err = tx.QueryRowContext(ctx, `
		SELECT 1
		FROM folders f
		JOIN accounts a ON a.id = f.account_id
		WHERE f.id = ?
		  AND a.user_id = ?
		  AND COALESCE(a.is_deleting, 0) = 0
		  AND COALESCE(f.discovery_state, 'active') = 'active'
		  AND COALESCE(f.selectable, 1) = 1
                  AND a.provider = 'imap'
                  AND f.role = 'custom'
	`, folderID, owner).Scan(&exists)

	if errors.Is(err, sql.ErrNoRows) {
		return ErrFolderNotManaged
	}
	if err != nil {
		return err
	}

	key := "folder_visible:" + folderID

	if visible {
		_, err = tx.ExecContext(ctx, `
			DELETE FROM app_settings
			WHERE user_id = ? AND key = ?
		`, owner, key)
	} else {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO app_settings
				(user_id, key, value, updated_at)
			VALUES (?, ?, 'false', CURRENT_TIMESTAMP)
			ON CONFLICT(user_id, key)
			DO UPDATE SET
				value = excluded.value,
				updated_at = excluded.updated_at
		`, owner, key)
	}

	if err != nil {
		return err
	}

	return tx.Commit()
}
