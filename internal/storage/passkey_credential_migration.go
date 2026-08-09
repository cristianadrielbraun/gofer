package storage

import (
	"database/sql"
	"fmt"
)

func migrateV80ToV81(tx *sql.Tx) error {
	hasCredentials, err := tableExistsTx(tx, "webauthn_credentials")
	if err != nil {
		return err
	}
	if !hasCredentials {
		return markSchemaVersion(tx, 81)
	}

	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "credential_ciphertext", definition: `BLOB CHECK (credential_ciphertext IS NULL OR length(credential_ciphertext) > 0)`},
		{name: "key_version", definition: `INTEGER CHECK (key_version IS NULL OR key_version > 0)`},
		{name: "rp_id", definition: `TEXT CHECK (rp_id IS NULL OR rp_id <> '')`},
		{name: "flags", definition: `INTEGER NOT NULL DEFAULT 0 CHECK (flags BETWEEN 0 AND 255)`},
		{name: "clone_warning", definition: `INTEGER NOT NULL DEFAULT 0 CHECK (clone_warning IN (0, 1))`},
	} {
		exists, err := columnExistsTx(tx, "webauthn_credentials", column.name)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(`ALTER TABLE webauthn_credentials ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
			return fmt.Errorf("add WebAuthn credential column %q: %w", column.name, err)
		}
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS webauthn_users (
		user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		rp_id TEXT NOT NULL CHECK (rp_id <> ''),
		user_handle BLOB NOT NULL CHECK (length(user_handle) BETWEEN 16 AND 64),
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (rp_id, user_id),
		UNIQUE (rp_id, user_handle)
	)`); err != nil {
		return fmt.Errorf("create WebAuthn user handles: %w", err)
	}

	if err := foreignKeyCheckTx(tx); err != nil {
		return err
	}
	return markSchemaVersion(tx, 81)
}
