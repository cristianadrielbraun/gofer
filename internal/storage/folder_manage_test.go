package storage

import (
	"context"
	"testing"
)

func seedManagedFolders(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'imap', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_trips", AccountID: "acc", RemoteID: "Trips", Name: "Trips", Role: "custom", Selectable: true},
		{ID: "acc_coast", AccountID: "acc", ParentID: "acc_trips", RemoteID: "Trips/Coast", Name: "Coast", Role: "custom", Selectable: true},
		{ID: "acc_dunes", AccountID: "acc", ParentID: "acc_coast", RemoteID: "Trips/Coast/Dunes", Name: "Dunes", Role: "custom", Selectable: true},
		{ID: "acc_tripsx", AccountID: "acc", RemoteID: "TripsX", Name: "TripsX", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatalf("seed folders: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO messages (id, account_id, internet_message_id, subject, from_email)
		VALUES (1, 'acc', '<ferry@example.com>', 'Ferry times', 'harbour@example.com'),
		       (2, 'acc', '<map@example.com>', 'Trail map', 'ranger@example.com'),
		       (3, 'acc', '<hello@example.com>', 'Hello', 'friend@example.com');
		INSERT INTO message_folder_state (message_id, folder_id, remote_uid)
		VALUES (1, 'acc_trips', 1), (2, 'acc_dunes', 1), (3, 'acc_inbox', 1), (3, 'acc_coast', 2);
	`); err != nil {
		t.Fatalf("seed messages: %v", err)
	}
	return db
}

func TestManagedFolderForUserListsFoldersInsideDeepestFirst(t *testing.T) {
	ctx := context.Background()
	db := seedManagedFolders(t)
	folder, err := db.ManagedFolderForUser(ctx, "default", "acc_trips")
	if err != nil {
		t.Fatalf("ManagedFolderForUser() error = %v", err)
	}
	if folder.Messages != 3 || len(folder.Descendants) != 2 || folder.Descendants[0].ID != "acc_dunes" || folder.Descendants[1].ID != "acc_coast" {
		t.Fatalf("folder = %+v, want 3 messages and Dunes before Coast", folder)
	}
	if _, err := db.ManagedFolderForUser(ctx, "someone-else", "acc_trips"); err != ErrFolderNotManaged {
		t.Fatalf("another user's folder error = %v, want ErrFolderNotManaged", err)
	}
}

func TestRenameFolderMovesFoldersInsideAndKeepsIDs(t *testing.T) {
	ctx := context.Background()
	db := seedManagedFolders(t)
	if err := db.RenameFolder(ctx, "acc", "acc_trips", "Trips", "Journeys", "Journeys", '/'); err != nil {
		t.Fatalf("RenameFolder() error = %v", err)
	}
	ids, err := db.IMAPFolderIDs(ctx, "acc")
	if err != nil {
		t.Fatalf("IMAPFolderIDs() error = %v", err)
	}
	want := map[string]string{
		"INBOX": "acc_inbox", "Journeys": "acc_trips", "Journeys/Coast": "acc_coast",
		"Journeys/Coast/Dunes": "acc_dunes", "TripsX": "acc_tripsx",
	}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for name, id := range want {
		if ids[name] != id {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
	var name string
	if err := db.Read().QueryRowContext(ctx, `SELECT name FROM folders WHERE id = 'acc_coast'`).Scan(&name); err != nil || name != "Coast" {
		t.Fatalf("child name = %q, %v; want Coast", name, err)
	}
}

func TestRemoveDeletedFoldersDropsMessagesOnlyThere(t *testing.T) {
	ctx := context.Background()
	db := seedManagedFolders(t)
	if err := db.RemoveDeletedFolders(ctx, "acc", FolderDiscoveryIMAP, []string{"acc_dunes", "acc_coast", "acc_trips"}); err != nil {
		t.Fatalf("RemoveDeletedFolders() error = %v", err)
	}
	var removed int
	_ = db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM folders WHERE discovery_state = 'removed'`).Scan(&removed)
	if removed != 3 {
		t.Fatalf("removed folders = %d, want 3", removed)
	}
	var messages []int
	rows, err := db.Read().QueryContext(ctx, `SELECT id FROM messages ORDER BY id`)
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		_ = rows.Scan(&id)
		messages = append(messages, id)
	}
	if len(messages) != 1 || messages[0] != 3 {
		t.Fatalf("messages left = %v, want only the one also in Inbox", messages)
	}
	if _, err := db.ManagedFolderForUser(ctx, "default", "acc_trips"); err != ErrFolderNotManaged {
		t.Fatalf("deleted folder error = %v, want ErrFolderNotManaged", err)
	}
}

// A folder made with the name a renamed folder had gets an ID of its own,
// and the renamed one keeps its ID through sync.
func TestAddIMAPFolderAfterRenameGetsNewID(t *testing.T) {
	ctx := context.Background()
	db := seedManagedFolders(t)
	original := FolderIDForIdentity("acc", "imap", "Notes")
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: original, AccountID: "acc", RemoteID: "Notes", Name: "Notes", Role: "custom", Selectable: true}}); err != nil {
		t.Fatalf("seed Notes: %v", err)
	}
	if err := db.RenameFolder(ctx, "acc", original, "Notes", "Jottings", "Jottings", '/'); err != nil {
		t.Fatalf("RenameFolder() error = %v", err)
	}
	id, err := db.AddIMAPFolder(ctx, "acc", "", "Notes", "Notes", 200)
	if err != nil {
		t.Fatalf("AddIMAPFolder() error = %v", err)
	}
	if id == "" || id == original {
		t.Fatalf("new Notes ID = %q, want one other than %q", id, original)
	}
	ids, err := db.IMAPFolderIDs(ctx, "acc")
	if err != nil || ids["Jottings"] != original || ids["Notes"] != id {
		t.Fatalf("ids = %v, %v", ids, err)
	}
	if again, err := db.AddIMAPFolder(ctx, "acc", "", "Notes", "Notes", 200); err != nil || again != id {
		t.Fatalf("second AddIMAPFolder() = %q, %v; want %q", again, err, id)
	}
}

func TestFolderNameTakenLooksAtSiblingsOnly(t *testing.T) {
	ctx := context.Background()
	db := seedManagedFolders(t)
	for _, tt := range []struct {
		parent, name, except string
		want                 bool
	}{
		{"", "trips", "", true},
		{"", "Trips", "acc_trips", false},
		{"acc_trips", "coast", "", true},
		{"acc_trips", "Dunes", "", false},
	} {
		got, err := db.FolderNameTaken(ctx, "acc", tt.parent, tt.name, tt.except)
		if err != nil || got != tt.want {
			t.Fatalf("FolderNameTaken(%q, %q, %q) = %v, %v; want %v", tt.parent, tt.name, tt.except, got, err, tt.want)
		}
	}
}

func TestDeleteProviderLabelsDropsQueuedChanges(t *testing.T) {
	ctx := context.Background()
	db := seedManagedFolders(t)
	if err := db.UpsertLabels(ctx, []LabelInput{
		{AccountID: "acc", Name: "Receipts", ProviderID: "Label_1", ProviderType: LabelProviderGmail},
		{AccountID: "acc", Name: "Recipes", ProviderID: "Label_2", ProviderType: LabelProviderGmail},
	}); err != nil {
		t.Fatalf("seed labels: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO label_mutation_queue (account_id, message_id, folder_id, provider_type, operation, label_name)
		VALUES ('acc', 1, 'acc_trips', ?, 'add', 'receipts'), ('acc', 1, 'acc_trips', ?, 'add', 'Recipes')`, LabelProviderGmail, LabelProviderGmail); err != nil {
		t.Fatalf("seed queue: %v", err)
	}
	if err := db.DeleteProviderLabels(ctx, "acc", LabelProviderGmail, []string{"Label_1"}); err != nil {
		t.Fatalf("DeleteProviderLabels() error = %v", err)
	}
	labels, err := db.ProviderLabels(ctx, "acc", LabelProviderGmail)
	if err != nil || len(labels) != 1 || labels[0].Name != "Recipes" {
		t.Fatalf("labels = %+v, %v; want only Recipes", labels, err)
	}
	var queued int
	_ = db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM label_mutation_queue`).Scan(&queued)
	if queued != 1 {
		t.Fatalf("queued label changes = %d, want 1", queued)
	}
	if err := db.RenameProviderLabels(ctx, "acc", LabelProviderGmail, map[string]string{"Label_2": "Cooking"}); err != nil {
		t.Fatalf("RenameProviderLabels() error = %v", err)
	}
	if labels, _ := db.ProviderLabels(ctx, "acc", LabelProviderGmail); len(labels) != 1 || labels[0].Name != "Cooking" {
		t.Fatalf("labels after rename = %+v", labels)
	}
}

// A folder moved into Trash knows it's there, with the folders inside it,
// and Trash lists it.
func TestMoveFolderIntoTrash(t *testing.T) {
	ctx := context.Background()
	db := seedManagedFolders(t)
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "acc_trash", AccountID: "acc", RemoteID: "Trash", Name: "Trash", Role: "trash", Selectable: true}}); err != nil {
		t.Fatalf("seed Trash: %v", err)
	}
	trash, err := db.TrashFolder(ctx, "default", "acc")
	if err != nil || trash.ID != "acc_trash" {
		t.Fatalf("TrashFolder() = %+v, %v", trash, err)
	}
	if err := db.MoveFolder(ctx, "acc", "acc_trips", "acc_trash", "Trips", "Trash/Trips", "Trips", '/'); err != nil {
		t.Fatalf("MoveFolder() error = %v", err)
	}
	for _, id := range []string{"acc_trips", "acc_dunes"} {
		folder, err := db.ManagedFolderForUser(ctx, "default", id)
		if err != nil || !folder.InTrash {
			t.Fatalf("%s = %+v, %v; want it in Trash", id, folder, err)
		}
	}
	if ids, _ := db.IMAPFolderIDs(ctx, "acc"); ids["Trash/Trips/Coast/Dunes"] != "acc_dunes" {
		t.Fatalf("ids = %v", ids)
	}
	for _, trashID := range []string{"acc_trash", "trash"} {
		inTrash, err := db.FoldersInTrash(ctx, "default", trashID)
		if err != nil || len(inTrash) != 1 || inTrash[0] != "acc_trips" {
			t.Fatalf("FoldersInTrash(%q) = %v, %v", trashID, inTrash, err)
		}
	}
	if folder, _ := db.ManagedFolderForUser(ctx, "default", "acc_tripsx"); folder.InTrash {
		t.Fatal("a folder outside Trash says it's in Trash")
	}
	if err := db.MoveFolder(ctx, "acc", "acc_trips", "", "Trash/Trips", "Trips", "Trips", '/'); err != nil {
		t.Fatalf("MoveFolder(out) error = %v", err)
	}
	if folder, _ := db.ManagedFolderForUser(ctx, "default", "acc_trips"); folder.InTrash || folder.ParentID != "" {
		t.Fatalf("folder moved out = %+v", folder)
	}
}
