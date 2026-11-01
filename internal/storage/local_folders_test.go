package storage

import (
	"path/filepath"
	"testing"
)

func newLocalFoldersTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized, name) VALUES ('robin', 'robin', 'robin', 'Robin');
		INSERT INTO users (id, username, username_normalized, name) VALUES ('sam', 'sam', 'sam', 'Sam');
		INSERT INTO accounts (id, user_id, provider, email_address, sort_order) VALUES ('imap-1', 'robin', 'imap', 'robin@example.test', 0);`); err != nil {
		t.Fatal(err)
	}
	return db
}

// Each user gets one Local folders account, after their other accounts, with
// Archive and Trash; asking again changes nothing.
func TestEnsureLocalFoldersAccount(t *testing.T) {
	db := newLocalFoldersTestDB(t)
	ctx := t.Context()
	id, err := db.EnsureLocalFoldersAccount(ctx, "robin")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := db.EnsureLocalFoldersAccount(ctx, "robin"); err != nil || again != id {
		t.Fatalf("second ensure = %q, %v; want %q", again, err, id)
	}
	other, err := db.EnsureLocalFoldersAccount(ctx, "sam")
	if err != nil || other == id {
		t.Fatalf("sam's account = %q, %v; robin's is %q", other, err, id)
	}

	var provider, name string
	var order, hidden int
	if err := db.Read().QueryRow(`SELECT provider, display_name, sort_order, hidden FROM accounts WHERE id = ?`, id).Scan(&provider, &name, &order, &hidden); err != nil {
		t.Fatal(err)
	}
	if provider != "local" || name != LocalFoldersName || order != 1 || hidden != 0 {
		t.Fatalf("account = %s %q order %d hidden %d", provider, name, order, hidden)
	}
	var folders int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM folders WHERE account_id = ?`, id).Scan(&folders); err != nil || folders != 2 {
		t.Fatalf("folders = %d, %v", folders, err)
	}
	for _, role := range []string{"archive", "trash"} {
		if folderID, _, err := db.GetFolderIDByRole(ctx, id, role); err != nil || folderID != LocalFolderID(id, role) {
			t.Fatalf("%s folder = %q, %v", role, folderID, err)
		}
	}
}

// Local folders have no mail server, so no sync ever picks them up.
func TestLocalFoldersAreNotSynced(t *testing.T) {
	db := newLocalFoldersTestDB(t)
	ctx := t.Context()
	id, err := db.EnsureLocalFoldersAccount(ctx, "robin")
	if err != nil {
		t.Fatal(err)
	}
	if db.IsEmailSyncEnabled(ctx, id) {
		t.Fatal("local folders sync is enabled")
	}
	for name, list := range map[string]func() ([]string, error){
		"user": func() ([]string, error) { return db.GetEmailSyncAccountIDs(ctx, "robin") },
		"all":  func() ([]string, error) { return db.GetAllEmailSyncAccountIDs(ctx) },
	} {
		ids, err := list()
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 || ids[0] != "imap-1" {
			t.Fatalf("%s sync accounts = %v", name, ids)
		}
	}
}

// Upgrading adds accounts.hidden, off for every account.
func TestMigrateV133ToV134AddsHidden(t *testing.T) {
	db := newLocalFoldersTestDB(t)
	if _, err := db.Write().Exec(`ALTER TABLE accounts DROP COLUMN hidden; DELETE FROM schema_version WHERE version >= 134`); err != nil {
		t.Fatal(err)
	}
	if err := migrateV133ToV134(db.Write()); err != nil {
		t.Fatal(err)
	}
	var hidden, version int
	if err := db.Read().QueryRow(`SELECT hidden FROM accounts WHERE id = 'imap-1'`).Scan(&hidden); err != nil || hidden != 0 {
		t.Fatalf("hidden = %d, %v", hidden, err)
	}
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 134 {
		t.Fatalf("version = %d, %v", version, err)
	}
	if err := migrateV133ToV134(db.Write()); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

// addLocalTestMessage files an invented message in a Local folder.
func addLocalTestMessage(t *testing.T, db *DB, accountID, folderID, subject string) int64 {
	t.Helper()
	result, err := db.Write().Exec(`INSERT INTO messages (account_id, subject, from_email) VALUES (?, ?, 'sender@example.test')`, accountID, subject)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO message_folder_state (message_id, folder_id, is_read) VALUES (?, ?, 0)`, id, folderID); err != nil {
		t.Fatal(err)
	}
	return id
}

func localFolderMessages(t *testing.T, db *DB, folderID string) []int64 {
	t.Helper()
	rows, err := db.Read().Query(`SELECT message_id FROM message_folder_state WHERE folder_id = ? AND is_deleted = 0 ORDER BY message_id`, folderID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// Moving, flagging and deleting mail in Local folders changes it here at
// once, queues nothing for a server, and a deleted message is gone for good.
func TestLocalFoldersChangesAreMadeHere(t *testing.T) {
	db := newLocalFoldersTestDB(t)
	ctx := t.Context()
	account, err := db.EnsureLocalFoldersAccount(ctx, "robin")
	if err != nil {
		t.Fatal(err)
	}
	archive, trash := LocalFolderID(account, "archive"), LocalFolderID(account, "trash")
	first := addLocalTestMessage(t, db, account, archive, "Garden plans")
	second := addLocalTestMessage(t, db, account, archive, "Bike repair")

	if err := db.SetMessagesReadAndQueueForUser(ctx, []int64{first}, true, "robin"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMessagesStarredAndQueueForUser(ctx, []int64{first}, true, "robin"); err != nil {
		t.Fatal(err)
	}
	if err := db.MoveMessagesAndQueueForUser(ctx, []int64{first}, archive, trash, "robin"); err != nil {
		t.Fatal(err)
	}
	if got := localFolderMessages(t, db, archive); len(got) != 1 || got[0] != second {
		t.Fatalf("archive = %v", got)
	}
	if got := localFolderMessages(t, db, trash); len(got) != 1 || got[0] != first {
		t.Fatalf("trash = %v", got)
	}
	var read, starred int
	if err := db.Read().QueryRow(`SELECT is_read, is_starred FROM message_folder_state WHERE message_id = ? AND folder_id = ?`, first, trash).Scan(&read, &starred); err != nil || read != 1 || starred != 1 {
		t.Fatalf("moved state read=%d starred=%d %v", read, starred, err)
	}
	var leftBehind int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_folder_state WHERE message_id = ? AND folder_id = ?`, first, archive).Scan(&leftBehind); err != nil || leftBehind != 0 {
		t.Fatalf("source copy kept: %d %v", leftBehind, err)
	}

	if err := db.PermanentlyDeleteMessagesAndQueueForUser(ctx, []int64{first}, trash, "robin"); err != nil {
		t.Fatal(err)
	}
	var messages, queued int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM messages WHERE id = ?`, first).Scan(&messages); err != nil || messages != 0 {
		t.Fatalf("deleted message kept: %d %v", messages, err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM messages WHERE id = ?`, second).Scan(&messages); err != nil || messages != 1 {
		t.Fatalf("other message gone: %d %v", messages, err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("queued %d changes, %v", queued, err)
	}
}
