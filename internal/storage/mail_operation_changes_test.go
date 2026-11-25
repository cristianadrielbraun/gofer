package storage

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

func TestMailOperationChangesTellTheAccountOwnerOnceCommitted(t *testing.T) {
	ctx := context.Background()
	db := seedUndoTest(t, "imap")
	if _, err := db.Write().Exec(`INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('other-acc', 'other', 'imap', 'robin@example.com')`); err != nil {
		t.Fatal(err)
	}
	var recorder contactChangeRecorder
	db.SetMailOperationsChangeHook(recorder.record)

	// Empty folder or a rule queues many changes at once: one notice.
	move := WithUserMailAction(ctx, MailActionMove)
	if err := db.MoveMessagesAndQueueForUser(move, []int64{1}, "projects", "archive", "default"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMessagesReadAndQueueForUser(ctx, []int64{1, 2}, true, "default"); err != nil {
		t.Fatal(err)
	}
	if owners := recorder.told(); !slices.Equal(owners, []string{"default"}) {
		t.Fatalf("after queuing, told %v; want [default] once", owners)
	}

	// A worker failing a change, with no page involved, tells the owner.
	if _, err := db.Write().Exec(`UPDATE message_mutations SET status = 'failed', attempt_count = 1, last_error = 'timeout' WHERE kind = 'move'`); err != nil {
		t.Fatal(err)
	}
	if owners := recorder.told(); !slices.Equal(owners, []string{"default"}) {
		t.Fatalf("after a failure, told %v; want [default]", owners)
	}

	// A change that is rolled back tells no one.
	tx, err := db.Write().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DELETE FROM message_mutations`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if owners := recorder.told(); len(owners) != 0 {
		t.Fatalf("after a rollback, told %v; want no one", owners)
	}

	// Each kind of work is watched, and each owner hears about their own.
	for _, statement := range []string{
		`INSERT INTO label_mutation_queue (account_id, message_id, folder_id, provider_type, label_name, operation) VALUES ('other-acc', 2, 'inbox', 'imap', 'Travel', 'add')`,
		`INSERT INTO outgoing_sends (id, account_id, transport, envelope_from, send_after) VALUES ('send-1', 'other-acc', 'smtp', 'robin@example.com', CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
		if owners := recorder.told(); !slices.Equal(owners, []string{"other"}) {
			t.Fatalf("after %s, told %v; want [other]", statement, owners)
		}
	}

	// Retry and Undo are changes too.
	if _, err := db.RetryMailActionForUser(ctx, "default", MailActionFrom(move).ID, "acc", nil); err != nil {
		t.Fatal(err)
	}
	if owners := recorder.told(); !slices.Equal(owners, []string{"default"}) {
		t.Fatalf("after Retry, told %v; want [default]", owners)
	}

	// Other mail writes aren't background work.
	if _, err := db.Write().Exec(`UPDATE messages SET subject = 'Lunch on Saturday?' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if owners := recorder.told(); len(owners) != 0 {
		t.Fatalf("after editing a message, told %v; want no one", owners)
	}
}

func TestOwnedStoreTellsTheSystemHookAboutMailOperations(t *testing.T) {
	system, err := New(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer system.Close()
	owned := seedUndoTest(t, "imap")
	owned.contactHookSource = system
	var recorder contactChangeRecorder
	system.SetMailOperationsChangeHook(recorder.record)
	if err := owned.SetMessagesStarredAndQueueForUser(context.Background(), []int64{2}, true, "default"); err != nil {
		t.Fatal(err)
	}
	if owners := recorder.told(); !slices.Equal(owners, []string{"default"}) {
		t.Fatalf("told %v; want [default]", owners)
	}
}
