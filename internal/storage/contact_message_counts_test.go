package storage

import (
	"context"
	"strings"
	"testing"
	"time"
)

func observedMessageCount(t *testing.T, db *DB, email string) int {
	t.Helper()
	var count int
	if err := db.Read().QueryRow(`SELECT message_count FROM contact_observations WHERE normalized_email = ?`, email).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// waitForMessageCount waits for the recount that follows a commit.
func waitForMessageCount(t *testing.T, db *DB, email string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := observedMessageCount(t, db, email)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s counts %d messages, want %d", email, got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func contactCountTestMessages() []SyncMessage {
	base := time.Date(2024, 5, 1, 9, 0, 0, 0, time.UTC)
	return []SyncMessage{
		{AccountID: "acct", FolderID: "box-inbox", RemoteUID: 1, MessageID: "<count-1@example.test>", Subject: "Garden plans",
			FromName: "Mira Holt", FromEmail: "Mira.Holt@example.test", DateSent: base,
			ToRecipients: []Recipient{{Name: "Reader", Email: "reader@example.test"}}},
		// Mira in To and in Cc is still one message.
		{AccountID: "acct", FolderID: "box-inbox", RemoteUID: 2, MessageID: "<count-2@example.test>", Subject: "Re: Garden plans",
			FromName: "Reader", FromEmail: "reader@example.test", DateSent: base.Add(time.Hour),
			ToRecipients: []Recipient{{Name: "Mira Holt", Email: "mira.holt@example.test"}},
			CCRecipients: []Recipient{{Name: "Mira", Email: "MIRA.HOLT@example.test"}}},
		{AccountID: "acct", FolderID: "box-inbox", RemoteUID: 3, MessageID: "<count-3@example.test>", Subject: "Seeds",
			FromName: "Mira Holt", FromEmail: "mira.holt@example.test", DateSent: base.Add(2 * time.Hour),
			ToRecipients: []Recipient{{Name: "Reader", Email: "reader@example.test"}}},
	}
}

// Syncing the same mail again, as every sync does, leaves the counts as they
// are: each message counts once for each person in it.
func TestContactMessageCountsCountEachMessageOnce(t *testing.T) {
	db := newFolderThreadTestDB(t)
	ctx := context.Background()
	for range 3 {
		if err := db.UpsertSyncMessages(ctx, contactCountTestMessages()); err != nil {
			t.Fatal(err)
		}
	}
	if got := observedMessageCount(t, db, "mira.holt@example.test"); got != 3 {
		t.Fatalf("Mira counts %d messages after three syncs of the same three, want 3", got)
	}
	// A backfill counts the same mail, and only sets the count.
	for range 2 {
		if err := db.BackfillObservedContacts(ctx, "reader"); err != nil {
			t.Fatal(err)
		}
	}
	if got := observedMessageCount(t, db, "mira.holt@example.test"); got != 3 {
		t.Fatalf("Mira counts %d messages after backfills, want 3", got)
	}
	// The recount after the commits doesn't change them either.
	time.Sleep(2 * contactChangeDelay)
	if got := observedMessageCount(t, db, "mira.holt@example.test"); got != 3 {
		t.Fatalf("Mira counts %d messages after the recount, want 3", got)
	}
}

// Deleted mail stops counting.
func TestContactMessageCountsDropWithDeletedMail(t *testing.T) {
	db := newFolderThreadTestDB(t)
	ctx := context.Background()
	if err := db.UpsertSyncMessages(ctx, contactCountTestMessages()); err != nil {
		t.Fatal(err)
	}
	if got := observedMessageCount(t, db, "mira.holt@example.test"); got != 3 {
		t.Fatalf("Mira counts %d messages, want 3", got)
	}
	if _, err := db.RemoveExpungedUIDs(ctx, "box-inbox", []uint32{2, 3}); err != nil {
		t.Fatal(err)
	}
	waitForMessageCount(t, db, "mira.holt@example.test", 1)
	waitForMessageCount(t, db, "reader@example.test", 1)
}

// Schema 140 recounts the counts that had been added up.
func TestMigrateV139ToV140RecountsContactMail(t *testing.T) {
	db := newFolderThreadTestDB(t)
	ctx := context.Background()
	if err := db.UpsertSyncMessages(ctx, contactCountTestMessages()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`UPDATE contact_observations SET message_count = 20502;
		DROP INDEX idx_messages_contact_email; DROP INDEX idx_recipients_contact_email;
		DELETE FROM schema_version WHERE version >= 140`); err != nil {
		t.Fatal(err)
	}
	if err := migrateV139ToV140(db.Write()); err != nil {
		t.Fatal(err)
	}
	if got := observedMessageCount(t, db, "mira.holt@example.test"); got != 3 {
		t.Fatalf("Mira counts %d messages after the migration, want 3", got)
	}
	var indexes, version int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name IN ('idx_messages_contact_email', 'idx_recipients_contact_email')`).Scan(&indexes); err != nil || indexes != 2 {
		t.Fatalf("indexes = %d, %v; want both", indexes, err)
	}
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 140 {
		t.Fatalf("version = %d, %v; want 140", version, err)
	}
}

// Both halves of the count look the address up in the indexes, also when it
// comes from each contact being recounted, instead of scanning all mail.
func TestContactMessageCountUsesIndexes(t *testing.T) {
	db := newFolderThreadTestDB(t)
	rows, err := db.Read().Query(`EXPLAIN QUERY PLAN UPDATE contact_observations SET message_count = ` + contactMessageCountSQL("contact_observations.user_id", "contact_observations.normalized_email"))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	usedMessages, usedRecipients := false, false
	for _, step := range plan {
		if strings.HasPrefix(step, "SCAN m") {
			t.Fatalf("count scans mail: %v", plan)
		}
		usedMessages = usedMessages || strings.HasPrefix(step, "SEARCH m USING COVERING INDEX idx_messages_contact_email (<expr>=?)")
		usedRecipients = usedRecipients || strings.HasPrefix(step, "SEARCH mr USING COVERING INDEX idx_recipients_contact_email (<expr>=?)")
	}
	if !usedMessages || !usedRecipients {
		t.Fatalf("count plan = %v; want both contact mail indexes", plan)
	}
}
