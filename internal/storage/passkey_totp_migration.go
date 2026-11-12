package storage

import "database/sql"

// Retain the current passkey + TOTP behavior for existing users. The new
// option is deliberately opt-out so an upgrade does not weaken sign-in policy.
func migrateV146ToV147(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 147 {
		return nil
	}
	present, err := columnExistsTx(tx, "users", "passkey_totp_required")
	if err != nil {
		return err
	}
	if !present {
		if _, err := tx.Exec(`ALTER TABLE users ADD COLUMN passkey_totp_required INTEGER NOT NULL DEFAULT 1 CHECK (passkey_totp_required IN (0,1))`); err != nil {
			return err
		}
	}
	if err := markSchemaVersion(tx, 147); err != nil {
		return err
	}
	return tx.Commit()
}
