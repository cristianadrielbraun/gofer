package storage

import (
	"testing"
	"time"
)

// A sent message is saved with its Message-ID in angle brackets, while IMAP
// sync reads the server's copy without them. Sync must find the saved
// message instead of adding the copy as a second one.
func TestIMAPSyncFindsMessageSavedWithBracketedID(t *testing.T) {
	db := newFolderThreadTestDB(t)
	sent := time.Date(2026, 10, 21, 11, 18, 0, 0, time.UTC)
	if _, err := db.Write().Exec(`INSERT INTO folders(id, account_id, remote_id, name, role) VALUES ('box-sent', 'acct', 'Sent', 'Sent', 'sent')`); err != nil {
		t.Fatal(err)
	}
	local := SyncMessage{AccountID: "acct", FolderID: "box-sent", MessageID: "<f0c1-tea@gofer>",
		Subject: "Tea on Sunday", FromEmail: "reader@example.test", DateSent: sent, IsRead: true}
	if err := db.UpsertSyncMessages(t.Context(), []SyncMessage{local}); err != nil {
		t.Fatal(err)
	}
	synced := local
	synced.MessageID = "f0c1-tea@gofer"
	synced.RemoteUID = 2
	if err := db.UpsertSyncMessages(t.Context(), []SyncMessage{synced}); err != nil {
		t.Fatal(err)
	}
	var messages, states int
	var uid int64
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*), COALESCE(MAX(remote_uid), 0) FROM message_folder_state WHERE folder_id = 'box-sent'`).Scan(&states, &uid); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || states != 1 || uid != 2 {
		t.Fatalf("messages=%d sent states=%d uid=%d; want one message holding UID 2", messages, states, uid)
	}
}
