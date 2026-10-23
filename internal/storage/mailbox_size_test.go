package storage

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestMailboxSizeAddsUpFoldersAndAccount(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address, display_name) VALUES ('acc', 'default', 'ana@example.com', 'Ana')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_archive", AccountID: "acc", RemoteID: "Archive", Name: "Archive", Role: "archive", Selectable: true},
		{ID: "acc_empty", AccountID: "acc", RemoteID: "Receipts", Name: "Receipts", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{
		{AccountID: "acc", FolderID: "acc_inbox", RemoteUID: 1, MessageID: "<lunch@example.com>", Subject: "Lunch", FromEmail: "rosa@example.com", DateSent: now, SizeBytes: 2 << 10},
		{AccountID: "acc", FolderID: "acc_inbox", RemoteUID: 2, MessageID: "<photos@example.com>", Subject: "Site photos", FromEmail: "rosa@example.com", DateSent: now, SizeBytes: 3 << 20},
		{AccountID: "acc", FolderID: "acc_inbox", RemoteUID: 3, MessageID: "<note@example.com>", Subject: "Old note", FromEmail: "rosa@example.com", DateSent: now},
		{AccountID: "acc", FolderID: "acc_archive", RemoteUID: 1, MessageID: "<gone@example.com>", Subject: "Moved away", FromEmail: "rosa@example.com", DateSent: now, SizeBytes: 5 << 20},
	}); err != nil {
		t.Fatal(err)
	}
	// The photos are also kept in Archive, as a Gmail label would; the other
	// archived message was deleted.
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO message_folder_state (message_id, folder_id, remote_uid)
		SELECT id, 'acc_archive', 2 FROM messages WHERE internet_message_id = '<photos@example.com>'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		UPDATE message_folder_state SET is_deleted = 1
		WHERE message_id = (SELECT id FROM messages WHERE internet_message_id = '<gone@example.com>')`); err != nil {
		t.Fatal(err)
	}

	size, err := db.GetMailboxSize(ctx, "default", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if size.Account.Email != "ana@example.com" || len(size.Account.Folders) != 3 {
		t.Fatalf("account = %+v", size.Account)
	}
	if want := (models.MailboxSizeCount{Messages: 3, Bytes: 2<<10 + 3<<20, Unmeasured: 1}); size.Total != want {
		t.Fatalf("total = %+v, want %+v", size.Total, want)
	}
	if want := (models.MailboxSizeCount{Messages: 3, Bytes: 2<<10 + 3<<20, Unmeasured: 1}); size.Folders["acc_inbox"] != want {
		t.Fatalf("inbox = %+v, want %+v", size.Folders["acc_inbox"], want)
	}
	if want := (models.MailboxSizeCount{Messages: 1, Bytes: 3 << 20}); size.Folders["acc_archive"] != want {
		t.Fatalf("archive = %+v, want %+v", size.Folders["acc_archive"], want)
	}
	if got := size.Folders["acc_empty"]; got != (models.MailboxSizeCount{}) {
		t.Fatalf("empty folder = %+v", got)
	}

	ids := map[string]int64{}
	rows, err := db.Read().QueryContext(ctx, `SELECT internet_message_id, id FROM messages`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var messageID string
		var id int64
		if err := rows.Scan(&messageID, &id); err != nil {
			t.Fatal(err)
		}
		ids[messageID] = id
	}
	rows.Close()
	files := map[int64]int64{
		ids["<lunch@example.com>"]:  100,
		ids["<photos@example.com>"]: 4000,
		ids["<gone@example.com>"]:   300,
		// Left behind by a message no longer in the database.
		99999: 20,
	}
	if err := db.AddLocalMailboxSizes(ctx, &size, files); err != nil {
		t.Fatal(err)
	}
	if !size.HasLocal || size.Total.LocalBytes != 4420 || size.LocalOutsideFolders != 320 {
		t.Fatalf("local total = %d, outside folders = %d", size.Total.LocalBytes, size.LocalOutsideFolders)
	}
	if got := size.Folders["acc_inbox"].LocalBytes; got != 4100 {
		t.Fatalf("inbox local = %d, want 4100", got)
	}
	if got := size.Folders["acc_archive"].LocalBytes; got != 4000 {
		t.Fatalf("archive local = %d, want 4000", got)
	}

	if _, err := db.GetMailboxSize(ctx, "someone-else", "acc"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("another owner's account: err = %v, want sql.ErrNoRows", err)
	}
}
