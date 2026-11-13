package storage

import (
	"database/sql"
	"fmt"
)

// migrateV147ToV148 lets an administrator accept a passkey as the second
// step of an individual two-factor requirement. Existing requirements keep
// asking for the authenticator app, so an upgrade never loosens a sign-in.
func migrateV147ToV148(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 148 {
		return nil
	}
	hasUsers, err := tableExistsTx(tx, "users")
	if err != nil {
		return fmt.Errorf("inspect users table: %w", err)
	}
	if hasUsers {
		present, err := columnExistsTx(tx, "users", "mfa_passkey_counts")
		if err != nil {
			return fmt.Errorf("inspect passkey second-step choice: %w", err)
		}
		if !present {
			if _, err := tx.Exec(`ALTER TABLE users ADD COLUMN mfa_passkey_counts INTEGER NOT NULL DEFAULT 0 CHECK (mfa_passkey_counts IN (0, 1))`); err != nil {
				return fmt.Errorf("add passkey second-step choice: %w", err)
			}
		}
	}
	if err := markSchemaVersion(tx, 148); err != nil {
		return err
	}
	return tx.Commit()
}
