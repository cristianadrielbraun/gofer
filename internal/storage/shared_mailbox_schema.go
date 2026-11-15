package storage

import (
	"database/sql"
	"fmt"
)

// ensureSharedMailboxSchema adds what shared mailboxes need: the shared
// mailbox a folder belongs to and its role there, and the shared mailboxes
// each account can open. Each step checks before it changes anything, so it
// runs on every start, after the numbered migrations, and on a new database
// as on an upgraded one.
func ensureSharedMailboxSchema(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin shared mailbox schema: %w", err)
	}
	defer tx.Rollback()

	// A database that holds no mail, such as an older one being upgraded
	// in parts, has nothing to add to yet.
	var mailTables int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name IN ('folders', 'accounts', 'account_identities')`).Scan(&mailTables); err != nil {
		return fmt.Errorf("check mail tables: %w", err)
	}
	if mailTables < 3 {
		return nil
	}

	columns := []struct{ table, column, definition string }{
		// The address of the shared mailbox the folder is in; '' for the
		// account's own folders.
		{"folders", "shared_mailbox", "TEXT NOT NULL DEFAULT ''"},
		// The folder's role inside that shared mailbox: a folder role, or
		// 'root' for the mailbox's own folder and 'namespace' for the folder
		// that holds the shared mailboxes. Its role column stays 'custom'.
		{"folders", "shared_role", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, c := range columns {
		exists, err := columnExistsTx(tx, c.table, c.column)
		if err != nil {
			return fmt.Errorf("check %s.%s: %w", c.table, c.column, err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(`ALTER TABLE ` + c.table + ` ADD COLUMN ` + c.column + ` ` + c.definition); err != nil {
			return fmt.Errorf("add %s.%s: %w", c.table, c.column, err)
		}
	}

	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_folders_shared_mailbox
			ON folders(account_id, shared_mailbox, shared_role) WHERE shared_mailbox != ''`,
		// The shared mailboxes an account can open, as its folder sync last
		// found them. A mailbox no longer listed is kept as 'removed', so what
		// belongs to it (its sender identity) can be put aside, not deleted.
		`CREATE TABLE IF NOT EXISTS shared_mailboxes (
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			email TEXT NOT NULL CHECK (email != ''),
			state TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'removed')),
			discovered_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			last_seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			removed_at DATETIME,
			PRIMARY KEY (account_id, email)
		)`,
		// The sender identity Gofer made for a shared mailbox. The identity
		// itself is an ordinary one in account_identities; this says it was
		// made automatically, whether the user changed its name since, and
		// whether the server accepted the address as a sender. State is
		// 'inactive' while the mailbox is no longer shared with the account,
		// 'disabled' while the user has it turned off, and 'deleted' once the
		// user deleted the identity, so that it is not made again. identity_id has no foreign key, so that the table never
		// depends on account_identities being there (the per-user migration
		// accepts older databases without it).
		`CREATE TABLE IF NOT EXISTS shared_mailbox_identities (
			identity_id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			mailbox_email TEXT NOT NULL CHECK (mailbox_email != ''),
			generated_name TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'inactive', 'disabled', 'deleted')),
			sender_check TEXT NOT NULL DEFAULT '' CHECK (sender_check IN ('', 'accepted', 'rejected', 'unknown')),
			sender_check_message TEXT NOT NULL DEFAULT '',
			sender_checked_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE (account_id, mailbox_email)
		)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("shared mailbox schema: %w", err)
		}
	}
	// Whether the user uses the shared mailbox: one turned off is neither
	// synced nor shown, and its identity is not offered. Added after the
	// table, which older databases already have without it.
	// server_name is the name the server gives the mailbox, as a JMAP
	// server's sender name for it; custom_name the name the user gave it.
	for _, c := range []struct{ column, definition string }{
		{"enabled", "INTEGER NOT NULL DEFAULT 1"},
		{"server_name", "TEXT NOT NULL DEFAULT ''"},
		{"custom_name", "TEXT NOT NULL DEFAULT ''"},
	} {
		exists, err := columnExistsTx(tx, "shared_mailboxes", c.column)
		if err != nil {
			return fmt.Errorf("check shared_mailboxes.%s: %w", c.column, err)
		}
		if !exists {
			if _, err := tx.Exec(`ALTER TABLE shared_mailboxes ADD COLUMN ` + c.column + ` ` + c.definition); err != nil {
				return fmt.Errorf("add shared_mailboxes.%s: %w", c.column, err)
			}
		}
	}
	return tx.Commit()
}

// The per-user storage migration copies every table and column it knows, and
// refuses one it doesn't (user_storage_migration_inventory.go). These are the
// ones shared mailboxes add, kept here with the schema that adds them.
func init() {
	folders := userStorageMigrationTables["folders"]
	folders.columns += ",shared_mailbox,shared_role"
	userStorageMigrationTables["folders"] = folders

	byAccount := `(SELECT a.user_id FROM @accounts a WHERE a.id=r.account_id)`
	userStorageMigrationTables["shared_mailboxes"] = migrationTableRule{migrationLocal, byAccount,
		"account_id,email,state,discovered_at,last_seen_at,removed_at,enabled,server_name,custom_name"}
	userStorageMigrationTables["shared_mailbox_identities"] = migrationTableRule{migrationLocal, byAccount,
		"identity_id,account_id,mailbox_email,generated_name,state,sender_check,sender_check_message,sender_checked_at,created_at,updated_at"}
	userStorageMigrationLinks = append(userStorageMigrationLinks,
		migrationLink{"shared_mailboxes", "accounts", []string{"account_id"}, []string{"id"}},
		migrationLink{"shared_mailbox_identities", "accounts", []string{"account_id"}, []string{"id"}},
	)
}
