package storage

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// seedUndoTest makes an account with Inbox, Archive and Projects, and two
// messages: one in Projects and one in Inbox.
func seedUndoTest(t *testing.T, provider string) *DB {
	t.Helper()
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'alex@example.com')`, provider); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO users (id, username, username_normalized, name) VALUES ('other', 'other', 'other', 'Other')`); err != nil {
		t.Fatalf("insert other user: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "archive", AccountID: "acc", RemoteID: "Archive", Name: "Archive", Role: "archive", Selectable: true},
		{ID: "projects", AccountID: "acc", RemoteID: "Projects", Name: "Projects", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatalf("seed folders: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO messages (id, account_id, internet_message_id, subject, from_email) VALUES
			(1, 'acc', '<lunch@example.com>', 'Lunch on Friday?', 'sam@example.com'),
			(2, 'acc', '<plans@example.com>', 'Weekend plans', 'sam@example.com');
		INSERT INTO message_folder_state (message_id, folder_id, remote_uid, remote_message_id) VALUES
			(1, 'projects', 11, 'p11'),
			(2, 'inbox', 21, 'i21');`); err != nil {
		t.Fatalf("seed messages: %v", err)
	}
	return db
}

// removeProjectsFolder has folder sync miss Projects long enough to treat it
// as deleted on the server, which parks the work that needs it.
func removeProjectsFolder(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	first := time.Date(2026, time.November, 2, 8, 0, 0, 0, time.UTC)
	for i, seen := range [][]string{{"INBOX", "Archive", "Projects"}, {"INBOX", "Archive"}, {"INBOX", "Archive"}} {
		at := first.Add([]time.Duration{0, time.Hour, 25 * time.Hour}[i])
		if _, err := db.ReconcileDiscoveredFolders(ctx, "acc", FolderDiscoveryIMAP, seen, at); err != nil {
			t.Fatalf("reconcile folders: %v", err)
		}
	}
}

// parkOperation parks a queued change the way folder cleanup does.
func parkOperation(t *testing.T, db *DB, table, id string) {
	t.Helper()
	status := ""
	if table == "message_mutations" {
		status = ", status = 'failed'"
	}
	if _, err := db.Write().Exec(`UPDATE `+table+` SET next_attempt_at = ?, last_error = 'folder removed remotely (imap discovery)'`+status+` WHERE id = ?`, parkedRetryAt, id); err != nil {
		t.Fatalf("park %s %s: %v", table, id, err)
	}
}

func onlyMutationID(t *testing.T, db *DB, messageID int64, kind string) string {
	t.Helper()
	var id string
	if err := db.Read().QueryRow(`SELECT id FROM message_mutations WHERE message_id = ? AND kind = ?`, messageID, kind).Scan(&id); err != nil {
		t.Fatalf("find %s mutation for message %d: %v", kind, messageID, err)
	}
	return id
}

func folderVisibility(t *testing.T, db *DB, messageID int64) map[string]bool {
	t.Helper()
	rows, err := db.Read().Query(`SELECT folder_id, is_deleted FROM message_folder_state WHERE message_id = ?`, messageID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	visible := map[string]bool{}
	for rows.Next() {
		var folder string
		var deleted int
		if err := rows.Scan(&folder, &deleted); err != nil {
			t.Fatal(err)
		}
		visible[folder] = deleted == 0
	}
	return visible
}

func countRows(t *testing.T, db *DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Read().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestParkedMoveOutOfRemovedFolderIsUndoneInGofer(t *testing.T) {
	ctx := context.Background()
	db := seedUndoTest(t, "imap")
	if err := db.MoveMessagesAndQueueForUser(ctx, []int64{1}, "projects", "archive", "default"); err != nil {
		t.Fatalf("move: %v", err)
	}
	mutation := onlyMutationID(t, db, 1, MessageMutationMove)
	removeProjectsFolder(t, db)

	operation, err := db.GetMailOperationForUser(ctx, "default", "message_mutation:"+mutation)
	if err != nil {
		t.Fatal(err)
	}
	if !operation.Parked || operation.ParkedReason != models.MailOperationParkedFolderRemoved || !operation.CanUndo || operation.CanRetry || !operation.NeedsAttention() {
		t.Fatalf("parked move = %#v", operation)
	}
	if _, err := db.RetryMailOperationForUser(ctx, "default", operation.ID); !errors.Is(err, ErrMailOperationNotRetryable) {
		t.Fatalf("retry parked move error = %v, want not retryable", err)
	}
	if _, err := db.UndoMailOperationForUser(ctx, "other", operation.ID, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("another user's undo error = %v, want sql.ErrNoRows", err)
	}

	if _, err := db.UndoMailOperationForUser(ctx, "default", operation.ID, nil); err != nil {
		t.Fatalf("undo: %v", err)
	}
	// Its folder is gone on the server, and the move never happened there:
	// Gofer has nowhere to show it until a sync finds it.
	if n := countRows(t, db, `SELECT COUNT(*) FROM message_mutations WHERE id = ?`, mutation); n != 0 {
		t.Fatalf("move still queued")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM messages WHERE id = 1`); n != 0 {
		t.Fatalf("message left behind in no folder")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM messages WHERE id = 2`); n != 1 {
		t.Fatalf("another message was removed")
	}
	if _, err := db.UndoMailOperationForUser(ctx, "default", operation.ID, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second undo error = %v, want sql.ErrNoRows", err)
	}
}

func TestParkedMoveIntoRemovedFolderPutsMessageBack(t *testing.T) {
	ctx := context.Background()
	db := seedUndoTest(t, "imap")
	if err := db.MoveMessagesAndQueueForUser(ctx, []int64{2}, "inbox", "projects", "default"); err != nil {
		t.Fatalf("move: %v", err)
	}
	mutation := onlyMutationID(t, db, 2, MessageMutationMove)
	removeProjectsFolder(t, db)
	if visible := folderVisibility(t, db, 2); visible["inbox"] {
		t.Fatalf("before undo the message shows in Inbox: %#v", visible)
	}

	if _, err := db.UndoMailOperationForUser(ctx, "default", "message_mutation:"+mutation, nil); err != nil {
		t.Fatalf("undo: %v", err)
	}
	visible := folderVisibility(t, db, 2)
	if !visible["inbox"] || len(visible) != 1 {
		t.Fatalf("after undo = %#v, want only Inbox", visible)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM message_mutations WHERE message_id = 2`); n != 0 {
		t.Fatalf("move still queued")
	}
}

func TestParkedMoveBetweenExistingFoldersPutsMessageBack(t *testing.T) {
	ctx := context.Background()
	db := seedUndoTest(t, "imap")
	if err := db.MoveMessagesAndQueueForUser(ctx, []int64{2}, "inbox", "archive", "default"); err != nil {
		t.Fatalf("move: %v", err)
	}
	mutation := onlyMutationID(t, db, 2, MessageMutationMove)
	parkOperation(t, db, "message_mutations", mutation)
	if _, err := db.UndoMailOperationForUser(ctx, "default", "message_mutation:"+mutation, nil); err != nil {
		t.Fatalf("undo: %v", err)
	}
	visible := folderVisibility(t, db, 2)
	if !visible["inbox"] || len(visible) != 1 {
		t.Fatalf("after undo = %#v, want only Inbox", visible)
	}
}

func TestParkedDeleteShowsMessageAgain(t *testing.T) {
	for _, provider := range []string{"imap", "gmail"} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			db := seedUndoTest(t, provider)
			if provider == "gmail" {
				// The same Gmail message under a second label.
				if _, err := db.Write().Exec(`INSERT INTO message_folder_state (message_id, folder_id, remote_message_id) VALUES (2, 'archive', 'i21')`); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.PermanentlyDeleteMessagesAndQueueForUser(ctx, []int64{2}, "inbox", "default"); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if visible := folderVisibility(t, db, 2); visible["inbox"] || visible["archive"] {
				t.Fatalf("before undo = %#v, want hidden", visible)
			}
			mutation := onlyMutationID(t, db, 2, MessageMutationDelete)
			parkOperation(t, db, "message_mutations", mutation)
			if _, err := db.UndoMailOperationForUser(ctx, "default", "message_mutation:"+mutation, nil); err != nil {
				t.Fatalf("undo: %v", err)
			}
			visible := folderVisibility(t, db, 2)
			if !visible["inbox"] || (provider == "gmail" && !visible["archive"]) {
				t.Fatalf("after undo = %#v, want shown again", visible)
			}
		})
	}
}

func TestParkedReadChangeRestoresUnreadUnlessAnotherIsQueued(t *testing.T) {
	ctx := context.Background()
	db := seedUndoTest(t, "imap")
	readState := func() int {
		return countRows(t, db, `SELECT MAX(is_read) FROM message_folder_state WHERE message_id = 2`)
	}
	if err := db.SetMessagesReadAndQueueForUser(ctx, []int64{2}, true, "default"); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	mutation := onlyMutationID(t, db, 2, MessageMutationRead)
	parkOperation(t, db, "message_mutations", mutation)
	if _, err := db.UndoMailOperationForUser(ctx, "default", "message_mutation:"+mutation, nil); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if readState() != 0 {
		t.Fatalf("message still read after undo")
	}

	// Shown in two folders, the change goes once per folder; undoing one
	// leaves the other on its way, so Gofer keeps showing it read.
	if _, err := db.Write().Exec(`INSERT INTO message_folder_state (message_id, folder_id, remote_uid) VALUES (2, 'archive', 31)`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMessagesReadAndQueueForUser(ctx, []int64{2}, true, "default"); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	var inboxMutation string
	if err := db.Read().QueryRow(`SELECT id FROM message_mutations WHERE message_id = 2 AND kind = 'read' AND folder_id = 'inbox'`).Scan(&inboxMutation); err != nil {
		t.Fatal(err)
	}
	parkOperation(t, db, "message_mutations", inboxMutation)
	if _, err := db.UndoMailOperationForUser(ctx, "default", "message_mutation:"+inboxMutation, nil); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if readState() != 1 {
		t.Fatalf("message shown unread while the other folder's change is queued")
	}
}

func TestParkedLabelChangeIsReversedInGofer(t *testing.T) {
	ctx := context.Background()
	db := seedUndoTest(t, "imap")
	info, err := db.GetMessageMutationInfoForUser(ctx, 1, "default")
	if err != nil || info == nil {
		t.Fatalf("mutation info: %v", err)
	}
	if err := db.QueueUserLabels(ctx, "default", []ThreadMessageMutationInfo{{MessageID: 1, MessageMutationInfo: *info}}, "Travel", LabelMutationAdd); err != nil {
		t.Fatalf("queue label: %v", err)
	}
	hasLabel := func() bool {
		return countRows(t, db, `SELECT COUNT(*) FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE ml.message_id = 1 AND l.name = 'Travel'`) > 0
	}
	if !hasLabel() {
		t.Fatalf("label not shown after queueing")
	}
	var labelID string
	if err := db.Read().QueryRow(`SELECT CAST(id AS TEXT) FROM label_mutation_queue WHERE message_id = 1`).Scan(&labelID); err != nil {
		t.Fatal(err)
	}

	queued, err := db.GetMailOperationForUser(ctx, "default", "label_mutation:"+labelID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.CanRetry || queued.CanUndo || queued.NeedsAttention() || queued.State != "pending" {
		t.Fatalf("label change not tried yet = %#v, want plain queued", queued)
	}
	if _, err := db.UndoMailOperationForUser(ctx, "default", queued.ID, nil); !errors.Is(err, ErrMailOperationNotUndoable) {
		t.Fatalf("undo of a queued label change error = %v, want not undoable", err)
	}

	removeProjectsFolder(t, db)
	parked, err := db.GetMailOperationForUser(ctx, "default", queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !parked.Parked || !parked.CanUndo {
		t.Fatalf("label change in removed folder = %#v, want parked", parked)
	}
	if _, err := db.UndoMailOperationForUser(ctx, "default", parked.ID, nil); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if hasLabel() {
		t.Fatalf("label still shown after undo")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM label_mutation_queue`); n != 0 {
		t.Fatalf("label change still queued")
	}
}

func TestFailedLabelChangeCanBeRetried(t *testing.T) {
	ctx := context.Background()
	db := seedUndoTest(t, "imap")
	if _, err := db.Write().Exec(`INSERT INTO label_mutation_queue (id, account_id, message_id, folder_id, provider_type, operation, label_name, attempts, last_error) VALUES (5, 'acc', 2, 'inbox', 'keyword', 'add', 'Travel', 1, 'server busy')`); err != nil {
		t.Fatal(err)
	}
	operation, err := db.GetMailOperationForUser(ctx, "default", "label_mutation:5")
	if err != nil {
		t.Fatal(err)
	}
	if !operation.CanRetry || operation.Parked || operation.CanUndo || operation.State != "failed" {
		t.Fatalf("failed label change = %#v", operation)
	}
}

func TestOrdinaryFailureAndParkedDraftAreNotUndoable(t *testing.T) {
	ctx := context.Background()
	db := seedUndoTest(t, "imap")
	if err := db.MoveMessagesAndQueueForUser(ctx, []int64{2}, "inbox", "archive", "default"); err != nil {
		t.Fatalf("move: %v", err)
	}
	mutation := onlyMutationID(t, db, 2, MessageMutationMove)
	if _, err := db.Write().Exec(`UPDATE message_mutations SET status = 'failed', last_error = 'server busy' WHERE id = ?`, mutation); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UndoMailOperationForUser(ctx, "default", "message_mutation:"+mutation, nil); !errors.Is(err, ErrMailOperationNotUndoable) {
		t.Fatalf("undo of a retryable failure error = %v, want not undoable", err)
	}

	draft, err := db.QueueIMAPDraftUpsert(ctx, QueueIMAPDraftUpsertInput{
		State:         IMAPDraftState{AccountID: "acc", DraftKey: "draft-1", FolderID: "projects", FolderRemoteName: "Projects"},
		RevisionToken: "revision-1", MIMEData: []byte("Subject: Lunch on Friday?\r\n\r\nSee you there."), MessageDate: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("queue draft: %v", err)
	}
	removeProjectsFolder(t, db)
	operation, err := db.GetMailOperationForUser(ctx, "default", "imap_draft:"+draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !operation.Parked || operation.CanUndo || operation.CanRetry {
		t.Fatalf("parked draft = %#v, want parked without undo or retry", operation)
	}
	if _, err := db.UndoMailOperationForUser(ctx, "default", operation.ID, nil); !errors.Is(err, ErrMailOperationNotUndoable) {
		t.Fatalf("undo of a draft error = %v, want not undoable", err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM imap_draft_operations WHERE id = ?`, draft.ID); n != 1 {
		t.Fatalf("draft operation was removed")
	}
}

func TestUndoKeepsMessageOtherQueuedWorkNeeds(t *testing.T) {
	ctx := context.Background()
	db := seedUndoTest(t, "imap")
	if err := db.MoveMessagesAndQueueForUser(ctx, []int64{1}, "projects", "archive", "default"); err != nil {
		t.Fatalf("move: %v", err)
	}
	mutation := onlyMutationID(t, db, 1, MessageMutationMove)
	removeProjectsFolder(t, db)
	if _, err := db.Write().Exec(`INSERT INTO label_mutation_queue (account_id, message_id, folder_id, provider_type, operation, label_name) VALUES ('acc', 1, 'archive', 'keyword', 'add', 'Travel')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UndoMailOperationForUser(ctx, "default", "message_mutation:"+mutation, nil); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM label_mutation_queue WHERE message_id = 1`); n != 1 {
		t.Fatalf("undoing the move removed another queued change of the message")
	}
}
