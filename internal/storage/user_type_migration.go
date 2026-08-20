package storage

import (
	"database/sql"
	"fmt"
)

const managementHandoffsV87Table = `CREATE TABLE IF NOT EXISTS management_handoffs (
	id TEXT PRIMARY KEY,
	source_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	target_user_id TEXT NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
	status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'completed', 'canceled')),
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	completed_at DATETIME,
	canceled_at DATETIME,
	CHECK (source_user_id != target_user_id),
	CHECK ((status = 'completed') = (completed_at IS NOT NULL)),
	CHECK ((status = 'canceled') = (canceled_at IS NOT NULL))
)`

func migrateV86ToV87(tx *sql.Tx) error {
	hasUsers, err := tableExistsTx(tx, "users")
	if err != nil {
		return err
	}
	if !hasUsers {
		return markSchemaVersion(tx, 87)
	}
	hasAdmin, err := columnExistsTx(tx, "users", "is_admin")
	if err != nil {
		return err
	}
	if !hasAdmin {
		if _, err := tx.Exec(`ALTER TABLE users ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add administrator marker before user separation: %w", err)
		}
	}
	hasType, err := columnExistsTx(tx, "users", "user_type")
	if err != nil {
		return err
	}
	if !hasType {
		if _, err := tx.Exec(`ALTER TABLE users ADD COLUMN user_type TEXT NOT NULL DEFAULT 'webmail' CHECK (user_type IN ('webmail', 'management'))`); err != nil {
			return fmt.Errorf("add separated user type: %w", err)
		}
	}
	hasAccounts, err := tableExistsTx(tx, "accounts")
	if err != nil {
		return err
	}
	hasAccountOwner := false
	if hasAccounts {
		hasAccountOwner, err = columnExistsTx(tx, "accounts", "user_id")
		if err != nil {
			return err
		}
	}
	hasAuthIdentities, err := tableExistsTx(tx, "auth_identities")
	if err != nil {
		return err
	}
	if hasAccountOwner {
		classification := `
			UPDATE users
			SET user_type = 'management'
			WHERE is_admin = 1
			  AND NOT EXISTS (SELECT 1 FROM accounts WHERE accounts.user_id = users.id)`
		if hasAuthIdentities {
			classification += `
			  AND NOT EXISTS (SELECT 1 FROM auth_identities WHERE auth_identities.user_id = users.id)`
		}
		if _, err := tx.Exec(classification); err != nil {
			return fmt.Errorf("classify mailbox-free administrators: %w", err)
		}
	} else if hasAuthIdentities {
		if _, err := tx.Exec(`
			UPDATE users
			SET user_type = 'management'
			WHERE is_admin = 1
			  AND NOT EXISTS (SELECT 1 FROM auth_identities WHERE auth_identities.user_id = users.id)`); err != nil {
			return fmt.Errorf("classify provider-identity-free administrators: %w", err)
		}
	} else if _, err := tx.Exec(`UPDATE users SET user_type = 'management' WHERE is_admin = 1`); err != nil {
		return fmt.Errorf("classify administrators: %w", err)
	}
	if _, err := tx.Exec(managementHandoffsV87Table); err != nil {
		return fmt.Errorf("create management handoff table: %w", err)
	}
	statements := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_management_handoffs_source_pending
		 ON management_handoffs(source_user_id) WHERE status = 'pending'`,
		`CREATE TRIGGER IF NOT EXISTS users_management_type_insert
		 BEFORE INSERT ON users
		 WHEN NEW.is_admin = 1 AND NEW.user_type != 'management'
		 BEGIN SELECT RAISE(ABORT, 'administrator must be a management user'); END`,
		`CREATE TRIGGER IF NOT EXISTS users_management_type_update
		 BEFORE UPDATE OF is_admin, user_type ON users
		 WHEN NEW.is_admin = 1 AND NEW.user_type != 'management'
		  AND (OLD.is_admin != NEW.is_admin OR OLD.user_type != NEW.user_type)
		 BEGIN SELECT RAISE(ABORT, 'administrator must be a management user'); END`,
		`CREATE TRIGGER IF NOT EXISTS users_management_mailbox_update
		 BEFORE UPDATE OF user_type ON users
		 WHEN NEW.user_type = 'management'
		  AND EXISTS (SELECT 1 FROM accounts WHERE user_id = NEW.id)
		 BEGIN SELECT RAISE(ABORT, 'management user cannot own a mailbox'); END`,
	}
	if hasAccountOwner {
		statements = append(statements,
			`CREATE TRIGGER IF NOT EXISTS accounts_management_owner_insert
			 BEFORE INSERT ON accounts
			 WHEN NEW.user_id IS NOT NULL
			  AND EXISTS (SELECT 1 FROM users WHERE id = NEW.user_id AND user_type != 'webmail')
			 BEGIN SELECT RAISE(ABORT, 'management user cannot own a mailbox'); END`,
			`CREATE TRIGGER IF NOT EXISTS accounts_management_owner_update
			 BEFORE UPDATE OF user_id ON accounts
			 WHEN NEW.user_id IS NOT NULL
			  AND EXISTS (SELECT 1 FROM users WHERE id = NEW.user_id AND user_type != 'webmail')
			 BEGIN SELECT RAISE(ABORT, 'management user cannot own a mailbox'); END`,
		)
	}
	if hasAuthIdentities {
		statements = append(statements,
			`CREATE TRIGGER IF NOT EXISTS users_management_identity_update
			 BEFORE UPDATE OF user_type ON users
			 WHEN NEW.user_type = 'management'
			  AND EXISTS (SELECT 1 FROM auth_identities WHERE user_id = NEW.id)
			 BEGIN SELECT RAISE(ABORT, 'management user cannot own an application sign-in identity'); END`,
			`CREATE TRIGGER IF NOT EXISTS auth_identities_management_owner_insert
			 BEFORE INSERT ON auth_identities
			 WHEN EXISTS (SELECT 1 FROM users WHERE id = NEW.user_id AND user_type != 'webmail')
			 BEGIN SELECT RAISE(ABORT, 'management user cannot own an application sign-in identity'); END`,
			`CREATE TRIGGER IF NOT EXISTS auth_identities_management_owner_update
			 BEFORE UPDATE OF user_id ON auth_identities
			 WHEN EXISTS (SELECT 1 FROM users WHERE id = NEW.user_id AND user_type != 'webmail')
			 BEGIN SELECT RAISE(ABORT, 'management user cannot own an application sign-in identity'); END`,
		)
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("install separated user invariant: %w", err)
		}
	}
	if err := foreignKeyCheckTx(tx); err != nil {
		return err
	}
	return markSchemaVersion(tx, 87)
}
