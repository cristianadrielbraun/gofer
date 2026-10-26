package storage

import (
	"maps"
	"path/filepath"
	"testing"
	"time"
)

// A message moved into a folder is shown there before the server reports it;
// one only in another folder, or hidden after moving out, isn't.
func TestMessagesShownInFolder(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := t.Context()
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('owner','owner','owner');
 INSERT INTO accounts(id,user_id,provider,email_address) VALUES('acc','owner','imap','ana@example.test')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "work", AccountID: "acc", RemoteID: "Work", Name: "Work", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{
		{AccountID: "acc", FolderID: "inbox", RemoteUID: 1, MessageID: "<moved@example.com>", Subject: "Moved", FromEmail: "ben@example.test", DateSent: now},
		{AccountID: "acc", FolderID: "inbox", RemoteUID: 2, MessageID: "<stays@example.com>", Subject: "Stays", FromEmail: "ben@example.test", DateSent: now},
	}); err != nil {
		t.Fatal(err)
	}
	moved, err := db.GetMessageLocalIDByInternetIDInternal(ctx, "acc", "<moved@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MoveMessagesAndQueue(ctx, []int64{moved}, "inbox", "work"); err != nil {
		t.Fatal(err)
	}
	ids := []string{"<moved@example.com>", "<stays@example.com>", "<new@example.com>"}
	if got, err := db.MessagesShownInFolder(ctx, "acc", "work", ids); err != nil || !maps.Equal(got, map[string]bool{"<moved@example.com>": true}) {
		t.Fatalf("work = %v, %v", got, err)
	}
	if got, err := db.MessagesShownInFolder(ctx, "acc", "inbox", ids); err != nil || !maps.Equal(got, map[string]bool{"<stays@example.com>": true}) {
		t.Fatalf("inbox = %v, %v", got, err)
	}
}
