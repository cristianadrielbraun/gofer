package storage

import (
	"database/sql"
	"slices"
	"testing"
	"time"
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

// The retention pass removes folder states marked deleted over 30 days ago,
// except ones something may still read.
func TestPruneDurableMailJobsRemovesOldDeletedFolderStates(t *testing.T) {
	db := newFolderThreadTestDB(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	old := now.Add(-DeletedFolderStateRetentionPeriod - time.Hour).Format("2006-01-02 15:04:05")
	recent := now.Add(-DeletedFolderStateRetentionPeriod + time.Hour).Format("2006-01-02 15:04:05")
	if _, err := db.Write().Exec(`
		INSERT INTO messages(id, account_id) VALUES (1, 'acct'), (2, 'acct'), (3, 'acct'), (4, 'acct'), (5, 'acct'), (6, 'acct'), (7, 'acct');
		INSERT INTO message_folder_state(message_id, folder_id, is_deleted, remote_uid) VALUES
			(1, 'box-inbox', 1, NULL), (2, 'box-inbox', 1, NULL), (3, 'box-inbox', 1, 42),
			(4, 'box-inbox', 1, NULL), (5, 'box-inbox', 1, NULL), (6, 'box-inbox', 1, NULL),
			(7, 'box-inbox', 0, NULL);
		INSERT INTO message_mutations(id, account_id, message_id, provider_type, kind, target_value, status)
			VALUES ('mut-4', 'acct', 4, 'imap', 'read', 1, 'applied');
		INSERT INTO label_mutation_queue(account_id, message_id, provider_type, operation, label_name)
			VALUES ('acct', 5, 'gmail', 'add', 'Receipts');
		INSERT INTO imap_draft_states(account_id, draft_key, local_message_id, folder_remote_name)
			VALUES ('acct', 'draft-6', 6, 'Drafts');`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`UPDATE message_folder_state SET deleted_at = CASE WHEN message_id = 2 THEN ? ELSE ? END WHERE is_deleted = 1`, recent, old); err != nil {
		t.Fatal(err)
	}

	result, err := db.PruneDurableMailJobs(t.Context(), now, RetentionBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	if result.FolderStates != 1 {
		t.Fatalf("pruned %d folder states, want 1", result.FolderStates)
	}
	rows, err := db.Read().Query(`SELECT message_id FROM message_folder_state ORDER BY message_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kept []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, id)
	}
	if want := []int64{2, 3, 4, 5, 6, 7}; !slices.Equal(kept, want) {
		t.Fatalf("kept folder states of messages %v, want %v", kept, want)
	}
	// The message row itself stays; only its folder state goes.
	var messages int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM messages WHERE id = 1`).Scan(&messages); err != nil || messages != 1 {
		t.Fatalf("message 1 rows = %d, %v", messages, err)
	}
}
