package storage

import (
	"database/sql"
	"fmt"
)

func migrateV88ToV89(tx *sql.Tx) error {
	hasHandoffs, err := tableExistsTx(tx, "management_handoffs")
	if err != nil {
		return fmt.Errorf("inspect management handoff table: %w", err)
	}
	if hasHandoffs {
		var incomplete int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM management_handoffs WHERE status != 'completed'`).Scan(&incomplete); err != nil {
			return fmt.Errorf("inspect management handoff completion: %w", err)
		}
		if incomplete != 0 {
			return fmt.Errorf("cannot retire management handoff support while %d handoff(s) are incomplete", incomplete)
		}
		if _, err := tx.Exec(`DROP TABLE management_handoffs`); err != nil {
			return fmt.Errorf("drop retired management handoff table: %w", err)
		}
	}
	hasUsers, err := tableExistsTx(tx, "users")
	if err != nil {
		return fmt.Errorf("inspect users table: %w", err)
	}
	if !hasUsers {
		return markSchemaVersion(tx, 89)
	}

	statements := []string{
		`CREATE TRIGGER IF NOT EXISTS users_management_admin_insert
		 BEFORE INSERT ON users
		 WHEN NEW.user_type = 'management' AND NEW.is_admin != 1
		 BEGIN SELECT RAISE(ABORT, 'management user must be an administrator'); END`,
		`CREATE TRIGGER IF NOT EXISTS users_management_admin_update
		 BEFORE UPDATE OF is_admin, user_type ON users
		 WHEN NEW.user_type = 'management' AND NEW.is_admin != 1
		  AND (OLD.is_admin != NEW.is_admin OR OLD.user_type != NEW.user_type)
		 BEGIN SELECT RAISE(ABORT, 'management user must be an administrator'); END`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("install final management-role invariant: %w", err)
		}
	}
	if err := foreignKeyCheckTx(tx); err != nil {
		return err
	}
	return markSchemaVersion(tx, 89)
}
