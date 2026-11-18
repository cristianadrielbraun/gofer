package storage

import (
	"database/sql"
	"fmt"
)

// sharedMailboxTablesSQL holds what shared mailboxes need besides the two
// folder columns: the shared mailboxes each account can open, and the sender
// identities Gofer made for them. schema.sql has the same.
const sharedMailboxTablesSQL = `
CREATE TABLE IF NOT EXISTS shared_mailboxes (
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	email TEXT NOT NULL CHECK (email != ''),
	state TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'removed')),
	enabled INTEGER NOT NULL DEFAULT 1,
	server_name TEXT NOT NULL DEFAULT '',
	custom_name TEXT NOT NULL DEFAULT '',
	discovered_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	last_seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	removed_at DATETIME,
	PRIMARY KEY (account_id, email)
);
CREATE TABLE IF NOT EXISTS shared_mailbox_identities (
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
);`

// migrateV150ToV151 adds shared mailboxes: the shared mailbox a folder is in
// and its role there, and the shared_mailboxes and shared_mailbox_identities
// tables. Both tables start empty; the next folder sync fills them.
func migrateV150ToV151(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 151 {
		return nil
	}
	// An older database being upgraded in parts may have no folders yet.
	folders, err := tableExistsTx(tx, "folders")
	if err != nil {
		return fmt.Errorf("check folders: %w", err)
	}
	for _, column := range []string{"shared_mailbox", "shared_role"} {
		if !folders {
			break
		}
		exists, err := columnExistsTx(tx, "folders", column)
		if err != nil {
			return fmt.Errorf("check folders.%s: %w", column, err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(`ALTER TABLE folders ADD COLUMN ` + column + ` TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add folders.%s: %w", column, err)
		}
	}
	// Some old test databases hold a folders table without accounts.
	indexable := false
	if folders {
		if indexable, err = columnExistsTx(tx, "folders", "account_id"); err != nil {
			return fmt.Errorf("check folders.account_id: %w", err)
		}
	}
	if indexable {
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_folders_shared_mailbox
			ON folders(account_id, shared_mailbox, shared_role) WHERE shared_mailbox != ''`); err != nil {
			return fmt.Errorf("index folders by shared mailbox: %w", err)
		}
	}
	if _, err := tx.Exec(sharedMailboxTablesSQL); err != nil {
		return fmt.Errorf("create shared mailbox tables: %w", err)
	}
	if err := markSchemaVersion(tx, 151); err != nil {
		return err
	}
	return tx.Commit()
}
