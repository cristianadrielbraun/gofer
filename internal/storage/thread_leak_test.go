package storage

import (
	"testing"
	"time"
)

func threadRowCount(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func messageThreadID(t *testing.T, db *DB, internetID string) string {
	t.Helper()
	var id string
	if err := db.Read().QueryRow(`SELECT thread_id FROM messages WHERE internet_message_id = ?`, internetID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Syncing a conversation again keeps its thread instead of starting a new
// one each time and leaving the old one empty.
func TestResyncKeepsConversationThread(t *testing.T) {
	db := newFolderThreadTestDB(t)
	sent := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	root := SyncMessage{AccountID: "acct", FolderID: "box-inbox", RemoteUID: 1, MessageID: "<garden-1@example.test>",
		Subject: "Garden club", FromEmail: "lena@example.test", DateSent: sent}
	reply := SyncMessage{AccountID: "acct", FolderID: "box-inbox", RemoteUID: 2, MessageID: "<garden-2@example.test>",
		InReplyTo: "<garden-1@example.test>", References: "<garden-1@example.test>",
		Subject: "Re: Garden club", FromEmail: "omar@example.test", DateSent: sent.Add(time.Hour)}
	if err := db.UpsertSyncMessages(t.Context(), []SyncMessage{root, reply}); err != nil {
		t.Fatal(err)
	}
	thread := messageThreadID(t, db, root.MessageID)
	if got := messageThreadID(t, db, reply.MessageID); got != thread {
		t.Fatalf("reply thread %q, root thread %q", got, thread)
	}

	for range 3 {
		root.IsRead = !root.IsRead
		if err := db.UpsertSyncMessages(t.Context(), []SyncMessage{root}); err != nil {
			t.Fatal(err)
		}
	}
	if got := messageThreadID(t, db, root.MessageID); got != thread {
		t.Fatalf("root moved from thread %q to %q", thread, got)
	}
	if got := messageThreadID(t, db, reply.MessageID); got != thread {
		t.Fatalf("reply moved from thread %q to %q", thread, got)
	}
	if n := threadRowCount(t, db); n != 1 {
		t.Fatalf("%d thread rows, want 1", n)
	}
}

// A thread goes once no message is in it, whether its messages moved to
// another conversation or were removed.
func TestThreadRemovedWithItsLastMessage(t *testing.T) {
	db := newFolderThreadTestDB(t)
	sent := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	if err := db.UpsertSyncMessages(t.Context(), []SyncMessage{
		{AccountID: "acct", FolderID: "box-inbox", RemoteUID: 1, MessageID: "<choir-1@example.test>",
			Subject: "Choir rota", FromEmail: "ines@example.test", DateSent: sent},
		{AccountID: "acct", FolderID: "box-inbox", RemoteUID: 2, MessageID: "<bikes-1@example.test>",
			Subject: "Bike repair", FromEmail: "tomas@example.test", DateSent: sent},
	}); err != nil {
		t.Fatal(err)
	}
	if n := threadRowCount(t, db); n != 2 {
		t.Fatalf("%d thread rows, want 2", n)
	}
	// The retention pass removes a message that is in no folder.
	if _, err := db.Write().Exec(`
		DELETE FROM message_folder_state WHERE message_id = (SELECT id FROM messages WHERE internet_message_id = '<choir-1@example.test>');
		UPDATE messages SET created_at = '2026-09-01 09:00:00';`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	result, err := db.PruneDurableMailJobs(t.Context(), now, RetentionBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages != 1 || result.Threads != 0 {
		t.Fatalf("pruned %d messages and %d threads, want 1 and 0", result.Messages, result.Threads)
	}
	var left string
	if err := db.Read().QueryRow(`SELECT normalized_subject FROM threads`).Scan(&left); err != nil || left != "bike repair" {
		t.Fatalf("thread left = %q, %v; want the bike repair thread only", left, err)
	}
}

// Threads earlier versions left empty are cleared by the retention pass.
func TestPruneDurableMailJobsRemovesEmptyThreads(t *testing.T) {
	db := newFolderThreadTestDB(t)
	if err := db.UpsertSyncMessages(t.Context(), []SyncMessage{{AccountID: "acct", FolderID: "box-inbox", RemoteUID: 1,
		MessageID: "<quilt-1@example.test>", Subject: "Quilt fair", FromEmail: "ana@example.test", DateSent: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO threads(id, account_id, message_count) VALUES ('empty-1', 'acct', 1), ('empty-2', 'acct', 3)`); err != nil {
		t.Fatal(err)
	}
	result, err := db.PruneDurableMailJobs(t.Context(), time.Now(), RetentionBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	if result.Threads != 2 {
		t.Fatalf("pruned %d threads, want 2", result.Threads)
	}
	if n := threadRowCount(t, db); n != 1 {
		t.Fatalf("%d thread rows, want 1", n)
	}
}
