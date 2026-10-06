package storage

import (
	"testing"
	"time"

	mailmessage "github.com/cristianadrielbraun/gofer/internal/mail/message"
)

func TestGmailQueueDeadlineCombinesOwnedQueuesAndTimeFormats(t *testing.T) {
	db, id := seedMessageMutationTest(t, "gmail", []UpsertFolderInput{{ID: "inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true}})
	if next, err := db.NextGmailQueueAttempt(t.Context(), "acc"); err != nil || !next.IsZero() {
		t.Fatalf("empty queue: %v %v", next, err)
	}
	if _, err := db.NextGmailQueueAttempt(t.Context(), ""); err == nil {
		t.Fatal("empty account permitted global deadlines")
	}
	if _, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,provider,email_address) VALUES('other','default','gmail','other@mail.test')`); err != nil {
		t.Fatal(err)
	}
	if err := db.EnqueueGmailMessageFetch(t.Context(), "acc", "m1", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.EnqueueGmailMessageFetch(t.Context(), "other", "foreign", "", nil); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	if _, err := db.Write().Exec(`UPDATE gmail_message_fetch_queue SET next_attempt_at=? WHERE account_id='acc'`, at.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`UPDATE gmail_message_fetch_queue SET next_attempt_at=datetime('now','-1 minute') WHERE account_id='other'`); err != nil {
		t.Fatal(err)
	}
	labelAt := at.Add(-10 * time.Second)
	if _, err := db.Write().Exec(`INSERT INTO label_mutation_queue(account_id,message_id,provider_type,operation,label_name,next_attempt_at) VALUES('acc',?,'gmail','add','Work',?)`, id, labelAt.Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	if next, err := db.NextGmailQueueAttempt(t.Context(), "acc"); err != nil || !next.Equal(labelAt) {
		t.Fatalf("mixed-format minimum: %v %v", next, err)
	}
	if _, err := db.Write().Exec(`UPDATE label_mutation_queue SET next_attempt_at=?`, formatDBTime(at.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if next, err := db.NextGmailQueueAttempt(t.Context(), "acc"); err != nil || !next.Equal(at) {
		t.Fatalf("owned metadata minimum: %v %v", next, err)
	}
}

func TestGmailBodyPublicationValidatesIdentityAndRollsBackAllRows(t *testing.T) {
	db, id := seedMessageMutationTest(t, "gmail", []UpsertFolderInput{{ID: "inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true}})
	if _, err := db.Write().Exec(`UPDATE accounts SET auth_method='oauth2',provider_account_id='subject' WHERE id='acc'; UPDATE messages SET remote_message_id='m1' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	cache := MessageBodyCache{ProviderMessageID: "m1", ProviderAccountID: "subject", TextPath: "candidate-text", Parsed: &mailmessage.ParsedMessage{Subject: "published body", FromEmail: "sender@mail.test", RawPath: "candidate-raw", Attachments: []mailmessage.AttachmentMeta{{Filename: "report.txt", BlobPath: "candidate-attachment"}}, To: []mailmessage.Recipient{{Email: "recipient@mail.test"}}}}
	assertUnpublished := func() {
		t.Helper()
		var count int
		if err := db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM messages WHERE id=? AND (body_text_path IS NOT NULL OR raw_path IS NOT NULL))+(SELECT COUNT(*) FROM attachments WHERE message_id=?)+(SELECT COUNT(*) FROM message_recipients WHERE message_id=?)`, id, id, id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial publication: %d %v", count, err)
		}
	}
	for _, change := range []string{"provider-message", "provider-subject", "provider", "method", "deleted"} {
		t.Run(change, func(t *testing.T) {
			candidate := cache
			switch change {
			case "provider-message":
				candidate.ProviderMessageID = "other"
			case "provider-subject":
				candidate.ProviderAccountID = "other"
			case "provider":
				if _, err := db.Write().Exec(`UPDATE accounts SET provider='outlook' WHERE id='acc'`); err != nil {
					t.Fatal(err)
				}
			case "method":
				if _, err := db.Write().Exec(`UPDATE accounts SET auth_method='plain' WHERE id='acc'`); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if _, err := db.Write().Exec(`UPDATE message_folder_state SET is_deleted=1 WHERE message_id=?`, id); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.SaveMessageBodyCache(t.Context(), id, "acc", candidate); err == nil {
				t.Fatal("stale provider body accepted")
			}
			assertUnpublished()
			if _, err := db.Write().Exec(`UPDATE accounts SET provider='gmail',auth_method='oauth2' WHERE id='acc'; UPDATE message_folder_state SET is_deleted=0 WHERE message_id=?`, id); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := db.Write().Exec(`CREATE TRIGGER fail_gmail_attachment BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT,'injected attachment failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMessageBodyCache(t.Context(), id, "acc", cache); err == nil {
		t.Fatal("attachment failure did not abort body publication")
	}
	assertUnpublished()
	if _, err := db.Write().Exec(`DROP TRIGGER fail_gmail_attachment`); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMessageBodyCache(t.Context(), id, "acc", cache); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_search WHERE message_search MATCH 'published'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("successful body not searchable: %d %v", count, err)
	}
}
