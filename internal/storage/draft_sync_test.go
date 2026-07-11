package storage

import (
	"bytes"
	"context"
	"database/sql"
	"testing"
	"time"
)

func seedIMAPDraftSyncTest(t *testing.T) (*DB, int64, IMAPDraftState) {
	t.Helper()
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'imap', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "drafts", AccountID: "acc", RemoteID: "Drafts", Name: "Drafts", Role: "drafts", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	messageID, err := db.SaveDraftMessage(ctx, DraftMessageInput{
		AccountID: "acc", FolderID: "drafts", InternetMessageID: "<draft@example.com>",
		Subject: "Local draft", FromEmail: "user@example.com", Date: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("SaveDraftMessage() error = %v", err)
	}
	return db, messageID, IMAPDraftState{
		AccountID: "acc", DraftKey: "<draft@example.com>", LocalMessageID: messageID,
		FolderID: "drafts", FolderRemoteName: "Drafts",
	}
}

func queueDraftRevision(t *testing.T, db *DB, state IMAPDraftState, revision string, raw []byte) IMAPDraftOperation {
	t.Helper()
	op, err := db.QueueIMAPDraftUpsert(context.Background(), QueueIMAPDraftUpsertInput{
		State: state, RevisionToken: revision, MIMEData: raw, MessageDate: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("QueueIMAPDraftUpsert() error = %v", err)
	}
	return op
}

func TestIMAPDraftQueueCoalescesPendingAutosaves(t *testing.T) {
	db, _, state := seedIMAPDraftSyncTest(t)
	first := queueDraftRevision(t, db, state, "revision-1", []byte("first"))
	second := queueDraftRevision(t, db, state, "revision-2", []byte("second"))
	if second.ID != first.ID || second.RevisionToken != "revision-2" || !bytes.Equal(second.MIMEData, []byte("second")) {
		t.Fatalf("coalesced operation first=%#v second=%#v", first, second)
	}
	var count int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM imap_draft_operations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("operation count = %d, %v", count, err)
	}
}

func TestIMAPDraftQueueKeepsNewRevisionBehindActiveRevision(t *testing.T) {
	db, _, state := seedIMAPDraftSyncTest(t)
	first := queueDraftRevision(t, db, state, "revision-1", []byte("first"))
	claimed, err := db.ClaimDueIMAPDraftOperations(context.Background(), time.Now(), 1)
	if err != nil || len(claimed) != 1 || claimed[0].ID != first.ID {
		t.Fatalf("first claim = %#v, %v", claimed, err)
	}
	second := queueDraftRevision(t, db, state, "revision-2", []byte("second"))
	if second.ID == first.ID {
		t.Fatal("new autosave replaced an operation that was already syncing")
	}
	if blocked, err := db.ClaimDueIMAPDraftOperations(context.Background(), time.Now(), 1); err != nil || len(blocked) != 0 {
		t.Fatalf("new revision bypassed active revision: %#v, %v", blocked, err)
	}
	if _, err := db.CompleteIMAPDraftUpsert(context.Background(), first.ID, 10, 100); err != nil {
		t.Fatalf("CompleteIMAPDraftUpsert(first) error = %v", err)
	}
	next, err := db.ClaimDueIMAPDraftOperations(context.Background(), time.Now(), 1)
	if err != nil || len(next) != 1 || next[0].ID != second.ID || next[0].State.RemoteUID != 10 || next[0].State.UIDValidity != 100 {
		t.Fatalf("second claim = %#v, %v", next, err)
	}
}

func TestInterruptedIMAPDraftOperationKeepsRevisionForRecovery(t *testing.T) {
	db, _, state := seedIMAPDraftSyncTest(t)
	queued := queueDraftRevision(t, db, state, "revision-1", []byte("first"))
	if operations, err := db.ClaimDueIMAPDraftOperations(context.Background(), time.Now(), 1); err != nil || len(operations) != 1 {
		t.Fatalf("ClaimDueIMAPDraftOperations() = %#v, %v", operations, err)
	}
	if count, err := db.MarkInterruptedIMAPDraftOperationsAmbiguous(context.Background(), "interrupted"); err != nil || count != 1 {
		t.Fatalf("MarkInterruptedIMAPDraftOperationsAmbiguous() = %d, %v", count, err)
	}
	recovered, err := db.ClaimDueIMAPDraftOperations(context.Background(), time.Now(), 1)
	if err != nil || len(recovered) != 1 || recovered[0].ID != queued.ID || recovered[0].Status != IMAPDraftStatusAmbiguous || recovered[0].RevisionToken != "revision-1" {
		t.Fatalf("recovered operation = %#v, %v", recovered, err)
	}
}

func TestAmbiguousIMAPDraftRevisionBlocksNewerAutosave(t *testing.T) {
	db, _, state := seedIMAPDraftSyncTest(t)
	first := queueDraftRevision(t, db, state, "revision-1", []byte("first"))
	if operations, err := db.ClaimDueIMAPDraftOperations(context.Background(), time.Now(), 1); err != nil || len(operations) != 1 {
		t.Fatalf("ClaimDueIMAPDraftOperations() = %#v, %v", operations, err)
	}
	if _, err := db.MarkInterruptedIMAPDraftOperationsAmbiguous(context.Background(), "interrupted"); err != nil {
		t.Fatalf("MarkInterruptedIMAPDraftOperationsAmbiguous() error = %v", err)
	}
	second := queueDraftRevision(t, db, state, "revision-2", []byte("second"))
	if second.ID == first.ID {
		t.Fatal("new autosave replaced an ambiguous revision")
	}
	claimed, err := db.ClaimDueIMAPDraftOperations(context.Background(), time.Now(), 10)
	if err != nil || len(claimed) != 1 || claimed[0].ID != first.ID {
		t.Fatalf("claim with queued autosave = %#v, %v", claimed, err)
	}
}

func TestIMAPDraftDeleteReplacesPendingUpsertAndSurvivesLocalDeletion(t *testing.T) {
	db, _, state := seedIMAPDraftSyncTest(t)
	upsert := queueDraftRevision(t, db, state, "revision-1", []byte("first"))
	deleted, err := db.QueueIMAPDraftDelete(context.Background(), state)
	if err != nil {
		t.Fatalf("QueueIMAPDraftDelete() error = %v", err)
	}
	if deleted.ID != upsert.ID || deleted.Kind != IMAPDraftOperationDelete || len(deleted.MIMEData) != 0 {
		t.Fatalf("delete operation = %#v", deleted)
	}
	if _, err := db.DeleteDraftMessage(context.Background(), state.AccountID, state.DraftKey); err != nil {
		t.Fatalf("DeleteDraftMessage() error = %v", err)
	}
	operation, err := db.GetIMAPDraftOperation(context.Background(), deleted.ID)
	if err != nil || operation.State.LocalMessageID != 0 {
		t.Fatalf("operation after local deletion = %#v, %v", operation, err)
	}
	if operations, err := db.ClaimDueIMAPDraftOperations(context.Background(), time.Now(), 1); err != nil || len(operations) != 1 {
		t.Fatalf("claim delete = %#v, %v", operations, err)
	}
	if err := db.CompleteIMAPDraftDelete(context.Background(), deleted.ID); err != nil {
		t.Fatalf("CompleteIMAPDraftDelete() error = %v", err)
	}
	if state, err := db.GetIMAPDraftState(context.Background(), state.AccountID, state.DraftKey); err != nil || state != nil {
		t.Fatalf("state after remote deletion = %#v, %v", state, err)
	}
}

func TestIMAPDraftSyncPreservesLocalDraftWithPendingRevision(t *testing.T) {
	db, messageID, state := seedIMAPDraftSyncTest(t)
	queueDraftRevision(t, db, state, "revision-1", []byte("local"))
	if err := db.UpsertSyncMessages(context.Background(), []SyncMessage{{
		AccountID: "acc", FolderID: "drafts", RemoteUID: 7, MessageID: state.DraftKey,
		Subject: "Stale remote draft", FromEmail: "user@example.com", DateSent: time.Now(), IsRead: true, IsDraft: true,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	var subject string
	var remoteUID sql.NullInt64
	var isDraft int
	if err := db.Read().QueryRow(`
		SELECT m.subject, mfs.remote_uid, mfs.is_draft
		FROM messages m JOIN message_folder_state mfs ON mfs.message_id = m.id
		WHERE m.id = ? AND mfs.folder_id = 'drafts'`, messageID).Scan(&subject, &remoteUID, &isDraft); err != nil {
		t.Fatalf("query preserved local draft: %v", err)
	}
	if subject != "Local draft" || !remoteUID.Valid || remoteUID.Int64 != 7 || isDraft != 1 {
		t.Fatalf("draft subject=%q remoteUID=%v isDraft=%d", subject, remoteUID, isDraft)
	}
}
