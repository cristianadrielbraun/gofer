package storage

import (
	"path/filepath"
	"testing"
)

// A database at version 150 gains the folder columns and both tables, keeps
// its folders, and running the migration again changes nothing.
func TestMigrateV150ToV151AddsSharedMailboxesTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`INSERT INTO accounts (id, email_address) VALUES ('a1', 'alice@example.com')`,
		`INSERT INTO folders (id, account_id, name, role) VALUES ('f1', 'a1', 'INBOX', 'inbox')`,
		`DROP TABLE shared_mailbox_identities`,
		`DROP TABLE shared_mailboxes`,
		`DROP INDEX idx_folders_shared_mailbox`,
		`ALTER TABLE folders DROP COLUMN shared_mailbox`,
		`ALTER TABLE folders DROP COLUMN shared_role`,
		`DELETE FROM schema_version`,
		`INSERT INTO schema_version (version) VALUES (150)`,
	} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	for run := 1; run <= 2; run++ {
		if err := migrateV150ToV151(db.Write()); err != nil {
			t.Fatal("run", run, err)
		}
	}
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 151 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	var sharedMailbox, sharedRole string
	if err := db.Read().QueryRow(`SELECT shared_mailbox, shared_role FROM folders WHERE id = 'f1'`).Scan(&sharedMailbox, &sharedRole); err != nil || sharedMailbox != "" || sharedRole != "" {
		t.Fatalf("kept folder = %q %q, %v", sharedMailbox, sharedRole, err)
	}
	for _, statement := range []string{
		`INSERT INTO shared_mailboxes (account_id, email) VALUES ('a1', 'team@example.com')`,
		`INSERT INTO shared_mailbox_identities (identity_id, account_id, mailbox_email) VALUES ('i1', 'a1', 'team@example.com')`,
	} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatal(statement, err)
		}
	}
}

// A version 151 database is left alone.
func TestMigrateV150ToV151SkipsCurrentDatabase(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrateV150ToV151(db.Write()); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version = 151`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("version rows = %d, %v", rows, err)
	}
}
