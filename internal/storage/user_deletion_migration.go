package storage

import (
	"database/sql"
	"fmt"
)

func migrateV90ToV91(tx *sql.Tx) error {
	hasUsers, err := tableExistsTx(tx, "users")
	if err != nil {
		return fmt.Errorf("inspect users table: %w", err)
	}
	if !hasUsers {
		return markSchemaVersion(tx, 91)
	}

	columns := []struct {
		name       string
		definition string
	}{
		{name: "deletion_pending", definition: "INTEGER NOT NULL DEFAULT 0 CHECK (deletion_pending IN (0, 1))"},
		{name: "deletion_started_at", definition: "DATETIME"},
		{name: "deletion_started_by", definition: "TEXT REFERENCES users(id) ON DELETE SET NULL"},
	}
	for _, column := range columns {
		exists, err := columnExistsTx(tx, "users", column.name)
		if err != nil {
			return fmt.Errorf("inspect user deletion field %s: %w", column.name, err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(`ALTER TABLE users ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
			return fmt.Errorf("add user deletion field %s: %w", column.name, err)
		}
	}
	if _, err := tx.Exec(`
		CREATE TRIGGER IF NOT EXISTS users_deletion_state_update
		BEFORE UPDATE OF status, user_type, is_admin, deletion_pending ON users
		WHEN NEW.deletion_pending = 1
		 AND (NEW.status != 'disabled' OR NEW.user_type != 'webmail' OR NEW.is_admin != 0)
		BEGIN
			SELECT RAISE(ABORT, 'pending deletion requires a disabled webmail user');
		END`); err != nil {
		return fmt.Errorf("create user deletion state trigger: %w", err)
	}
	if err := foreignKeyCheckTx(tx); err != nil {
		return err
	}
	return markSchemaVersion(tx, 91)
}
