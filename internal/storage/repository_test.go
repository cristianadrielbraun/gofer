package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestGetFoldersForAccountSkipsNonSelectableFolders(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{
			ID:         "acc_gmail_sent",
			AccountID:  "acc",
			ParentID:   "acc_gmail",
			RemoteID:   "[Gmail]/Sent Mail",
			Name:       "Sent",
			Icon:       "send",
			Role:       "sent",
			Selectable: true,
			SortOrder:  2,
		},
		{
			ID:         "acc_gmail",
			AccountID:  "acc",
			RemoteID:   "[Gmail]",
			Name:       "[Gmail]",
			Icon:       "folder",
			Role:       "custom",
			Selectable: false,
			SortOrder:  100,
		},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	folders, err := db.GetFoldersForAccount(ctx, "acc")
	if err != nil {
		t.Fatalf("GetFoldersForAccount() error = %v", err)
	}
	if len(folders) != 1 || folders[0].RemoteID != "[Gmail]/Sent Mail" {
		t.Fatalf("GetFoldersForAccount() = %#v, want only selectable child", folders)
	}

	var parentID sql.NullString
	if err := db.Read().QueryRowContext(ctx, `SELECT parent_id FROM folders WHERE id = 'acc_gmail_sent'`).Scan(&parentID); err != nil {
		t.Fatalf("query parent: %v", err)
	}
	if !parentID.Valid || parentID.String != "acc_gmail" {
		t.Fatalf("parent_id = %#v, want acc_gmail", parentID)
	}
}

func TestMigrateV34MarksGmailContainerNonSelectable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB() error = %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (34);
		CREATE TABLE accounts (id TEXT PRIMARY KEY, imap_host TEXT NOT NULL DEFAULT '');
		CREATE TABLE folders (id TEXT PRIMARY KEY, account_id TEXT NOT NULL, remote_id TEXT);
		INSERT INTO accounts (id, imap_host) VALUES ('acc', 'imap.gmail.com');
		INSERT INTO folders (id, account_id, remote_id) VALUES ('acc_gmail', 'acc', '[Gmail]');
	`); err != nil {
		raw.Close()
		t.Fatalf("seed v34 database: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var selectable int
	if err := db.Read().QueryRow(`SELECT selectable FROM folders WHERE id = 'acc_gmail'`).Scan(&selectable); err != nil {
		t.Fatalf("query selectable: %v", err)
	}
	if selectable != 0 {
		t.Fatalf("selectable = %d, want 0", selectable)
	}
}
