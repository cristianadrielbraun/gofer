package storage

import (
	"database/sql"
	"testing"
)

func folderStateDeletedAt(t *testing.T, db *DB, messageID int64, folderID string) sql.NullString {
	t.Helper()
	var at sql.NullString
	if err := db.Read().QueryRow(`SELECT CAST(deleted_at AS TEXT) FROM message_folder_state WHERE message_id = ? AND folder_id = ?`, messageID, folderID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// Folder states marked deleted before schema 126 count as marked at the
// upgrade; unmarked ones get no time.
func TestFolderStateDeletedAtUpgradeMarksDeletedStates(t *testing.T) {
	db := newFolderThreadTestDB(t)
	if _, err := db.Write().Exec(`
		DROP TRIGGER message_folder_state_deleted_at_insert;
		DROP TRIGGER message_folder_state_deleted_at_update;
		DROP INDEX idx_message_folder_state_deleted_at;
		ALTER TABLE message_folder_state DROP COLUMN deleted_at;
		INSERT INTO messages(id, account_id) VALUES (1, 'acct'), (2, 'acct');
		INSERT INTO message_folder_state(message_id, folder_id, is_deleted) VALUES
			(1, 'box-inbox', 0), (2, 'box-inbox', 1);
		DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES (125);`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV125ToV126(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	if at := folderStateDeletedAt(t, db, 1, "box-inbox"); at.Valid {
		t.Fatalf("unmarked state got deleted_at %q", at.String)
	}
	if at := folderStateDeletedAt(t, db, 2, "box-inbox"); !at.Valid {
		t.Fatal("marked state has no deleted_at")
	}
	var triggers int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'message_folder_state_deleted_at_%'`).Scan(&triggers); err != nil || triggers != 2 {
		t.Fatalf("deleted_at triggers = %d, %v", triggers, err)
	}
}

// deleted_at follows is_deleted however a folder state is written, and
// marking a state again keeps the first time.
func TestFolderStateDeletedAtFollowsIsDeleted(t *testing.T) {
	db := newFolderThreadTestDB(t)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Write().Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO messages(id, account_id) VALUES (1, 'acct'), (2, 'acct')`)
	exec(`INSERT INTO message_folder_state(message_id, folder_id, is_deleted) VALUES (1, 'box-inbox', 1), (2, 'box-inbox', 0)`)
	if !folderStateDeletedAt(t, db, 1, "box-inbox").Valid {
		t.Fatal("state inserted as deleted has no deleted_at")
	}
	if folderStateDeletedAt(t, db, 2, "box-inbox").Valid {
		t.Fatal("state inserted as present has deleted_at")
	}

	exec(`UPDATE message_folder_state SET is_deleted = 1 WHERE message_id = 2`)
	if !folderStateDeletedAt(t, db, 2, "box-inbox").Valid {
		t.Fatal("marking did not set deleted_at")
	}
	exec(`UPDATE message_folder_state SET deleted_at = '2026-01-01 00:00:00' WHERE message_id = 2`)
	exec(`UPDATE message_folder_state SET is_deleted = 1, synced_at = CURRENT_TIMESTAMP WHERE message_id = 2`)
	if at := folderStateDeletedAt(t, db, 2, "box-inbox"); at.String != "2026-01-01 00:00:00" {
		t.Fatalf("marking again moved deleted_at to %q", at.String)
	}

	// Sync writes states back through upserts.
	exec(`INSERT INTO message_folder_state(message_id, folder_id, is_deleted) VALUES (2, 'box-inbox', 0)
		ON CONFLICT(message_id, folder_id) DO UPDATE SET is_deleted = excluded.is_deleted`)
	if at := folderStateDeletedAt(t, db, 2, "box-inbox"); at.Valid {
		t.Fatalf("restored state kept deleted_at %q", at.String)
	}
}
