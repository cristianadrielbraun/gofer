package storage

import (
	"testing"
	"time"
)

func TestGetSenderAvatarUserIDsUsesActiveAccountOwnership(t *testing.T) {
	ctx := t.Context()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users (id, email, name) VALUES ('user-b', 'b@example.com', 'User B');
		INSERT INTO accounts (id, user_id, email_address) VALUES ('account-a', 'default', 'a@example.com');
		INSERT INTO accounts (id, user_id, email_address, is_deleting) VALUES ('account-b', 'user-b', 'b@example.com', 1);
	`); err != nil {
		t.Fatalf("seed users and accounts: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "inbox-a", AccountID: "account-a", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "inbox-b", AccountID: "account-b", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	for _, message := range []SyncMessage{
		{AccountID: "account-a", FolderID: "inbox-a", RemoteUID: 1, MessageID: "<a@example.com>", FromEmail: "Sender@Example.com", DateSent: time.Now()},
		{AccountID: "account-b", FolderID: "inbox-b", RemoteUID: 1, MessageID: "<b@example.com>", FromEmail: "sender@example.com", DateSent: time.Now()},
	} {
		if err := db.UpsertSyncMessages(ctx, []SyncMessage{message}); err != nil {
			t.Fatalf("UpsertSyncMessages() error = %v", err)
		}
	}
	userIDs, err := db.GetSenderAvatarUserIDs(ctx, " sender@example.com ")
	if err != nil {
		t.Fatalf("GetSenderAvatarUserIDs() error = %v", err)
	}
	if len(userIDs) != 1 || userIDs[0] != "default" {
		t.Fatalf("GetSenderAvatarUserIDs() = %#v, want only active owner", userIDs)
	}
}
