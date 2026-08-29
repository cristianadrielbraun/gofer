package storage

import (
	"database/sql"
	"fmt"
)

func migrateV89ToV90(tx *sql.Tx) error {
	hasUsers, err := tableExistsTx(tx, "users")
	if err != nil {
		return fmt.Errorf("inspect users table: %w", err)
	}
	if !hasUsers {
		return markSchemaVersion(tx, 90)
	}
	hasRequestedAt, err := columnExistsTx(tx, "users", "password_reset_requested_at")
	if err != nil {
		return fmt.Errorf("inspect password reset request field: %w", err)
	}
	if !hasRequestedAt {
		if _, err := tx.Exec(`ALTER TABLE users ADD COLUMN password_reset_requested_at DATETIME`); err != nil {
			return fmt.Errorf("add password reset request field: %w", err)
		}
	}
	if err := foreignKeyCheckTx(tx); err != nil {
		return err
	}
	return markSchemaVersion(tx, 90)
}
