package storage

import (
	"testing"
	"time"

	mailmessage "github.com/cristianadrielbraun/gofer/internal/mail/message"
)

func messageRecipientKinds(t *testing.T, db *DB, id int64) map[string]string {
	t.Helper()
	rows, err := db.Read().Query(`SELECT kind,email FROM message_recipients WHERE message_id=? ORDER BY kind`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var kind, email string
		if err := rows.Scan(&kind, &email); err != nil {
			t.Fatal(err)
		}
		got[kind] += email
	}
	return got
}

func TestIMAPHeaderSyncKeepsBcc(t *testing.T) {
	db, id := seedMessageMutationTest(t, "imap", []UpsertFolderInput{{ID: "drafts", AccountID: "acc", RemoteID: "Drafts", Name: "Drafts", Role: "drafts", Selectable: true}})
	if err := db.UpsertSyncMessages(t.Context(), []SyncMessage{{
		AccountID: "acc", FolderID: "drafts", RemoteUID: 1, MessageID: "<mutation@example.com>", Subject: "Mutation", FromEmail: "user@example.com",
		DateSent: time.Now(), IsDraft: true,
		ToRecipients:  []Recipient{{Email: "ana@example.com"}},
		BCCRecipients: []Recipient{{Email: "bruno@example.com"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if got := messageRecipientKinds(t, db, id); got["to"] != "ana@example.com" || got["bcc"] != "bruno@example.com" {
		t.Fatalf("recipients = %v", got)
	}
}

func TestBodyDownloadKeepsBcc(t *testing.T) {
	db, id := seedMessageMutationTest(t, "gmail", []UpsertFolderInput{{ID: "drafts", AccountID: "acc", RemoteID: "DRAFT", Name: "Drafts", Role: "drafts", Selectable: true}})
	if _, err := db.Write().Exec(`UPDATE accounts SET auth_method='oauth2',provider_account_id='subject' WHERE id='acc'; UPDATE message_folder_state SET remote_message_id='m1' WHERE message_id=?; INSERT INTO message_recipients(message_id,kind,name,email) VALUES(?,'bcc','','bruno@example.com')`, id, id); err != nil {
		t.Fatal(err)
	}
	cache := MessageBodyCache{ProviderMessageID: "m1", ProviderAccountID: "subject", ProviderType: "gmail", Parsed: &mailmessage.ParsedMessage{Subject: "Mutation", FromEmail: "user@example.com", To: []mailmessage.Recipient{{Email: "ana@example.com"}}}}
	// A copy without Bcc leaves the saved Bcc alone.
	if err := db.SaveMessageBodyCache(t.Context(), id, "acc", cache); err != nil {
		t.Fatal(err)
	}
	if got := messageRecipientKinds(t, db, id); got["to"] != "ana@example.com" || got["bcc"] != "bruno@example.com" {
		t.Fatalf("recipients without Bcc in the copy = %v", got)
	}
	// A copy with Bcc replaces it.
	cache.Parsed.BCC = []mailmessage.Recipient{{Email: "cara@example.com"}}
	if err := db.SaveMessageBodyCache(t.Context(), id, "acc", cache); err != nil {
		t.Fatal(err)
	}
	if got := messageRecipientKinds(t, db, id); got["to"] != "ana@example.com" || got["bcc"] != "cara@example.com" {
		t.Fatalf("recipients with Bcc in the copy = %v", got)
	}
}
