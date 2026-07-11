package storage

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func seedMessageMutationTest(t *testing.T, provider string, folders []UpsertFolderInput) (*DB, int64) {
	t.Helper()
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, provider); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, folders); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	for i, folder := range folders {
		if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
			AccountID: "acc", FolderID: folder.ID, RemoteUID: uint32(i + 1),
			MessageID: "<mutation@example.com>", Subject: "Mutation", FromEmail: "sender@example.com",
			DateSent: time.Now(), IsRead: true,
		}}); err != nil {
			t.Fatalf("UpsertSyncMessages(%s) error = %v", folder.ID, err)
		}
	}
	messageID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<mutation@example.com>")
	if err != nil || messageID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", messageID, err)
	}
	return db, messageID
}

func TestIMAPMessageMutationsCoalescePerFolder(t *testing.T) {
	db, messageID := seedMessageMutationTest(t, "imap", []UpsertFolderInput{
		{ID: "inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "archive", AccountID: "acc", RemoteID: "Archive", Name: "Archive", Role: "archive", Selectable: true},
	})
	ctx := context.Background()
	if err := db.SetMessageReadAndQueue(ctx, messageID, false); err != nil {
		t.Fatalf("SetMessageReadAndQueue(false) error = %v", err)
	}
	rows, err := db.Read().Query(`SELECT id, folder_id, target_value FROM message_mutations ORDER BY folder_id`)
	if err != nil {
		t.Fatalf("query mutations: %v", err)
	}
	firstIDs := map[string]string{}
	for rows.Next() {
		var id, folderID string
		var target int
		if err := rows.Scan(&id, &folderID, &target); err != nil {
			t.Fatalf("scan mutation: %v", err)
		}
		if target != 0 {
			t.Fatalf("initial target for %s = %d, want unread", folderID, target)
		}
		firstIDs[folderID] = id
	}
	rows.Close()
	if len(firstIDs) != 2 || firstIDs["inbox"] == "" || firstIDs["archive"] == "" {
		t.Fatalf("IMAP mutation scopes = %#v", firstIDs)
	}

	if err := db.SetMessageReadAndQueue(ctx, messageID, true); err != nil {
		t.Fatalf("SetMessageReadAndQueue(true) error = %v", err)
	}
	rows, err = db.Read().Query(`SELECT id, folder_id, target_value, status, attempt_count FROM message_mutations ORDER BY folder_id`)
	if err != nil {
		t.Fatalf("query coalesced mutations: %v", err)
	}
	count := 0
	for rows.Next() {
		var id, folderID, status string
		var target, attempts int
		if err := rows.Scan(&id, &folderID, &target, &status, &attempts); err != nil {
			t.Fatalf("scan coalesced mutation: %v", err)
		}
		if id != firstIDs[folderID] || target != 1 || status != MessageMutationPending || attempts != 0 {
			t.Fatalf("coalesced mutation id=%q folder=%q target=%d status=%q attempts=%d", id, folderID, target, status, attempts)
		}
		count++
	}
	rows.Close()
	if count != 2 {
		t.Fatalf("coalesced mutation count = %d, want 2", count)
	}
	var unreadStates int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_folder_state WHERE message_id = ? AND is_read = 0`, messageID).Scan(&unreadStates); err != nil || unreadStates != 0 {
		t.Fatalf("local unread states = %d, %v", unreadStates, err)
	}
}

func TestProviderMessageMutationUsesOneGlobalScope(t *testing.T) {
	db, messageID := seedMessageMutationTest(t, MessageMutationProviderGmail, []UpsertFolderInput{
		{ID: "inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "archive", AccountID: "acc", RemoteID: "Archive", Name: "Archive", Role: "archive", Selectable: true},
	})
	if err := db.SetMessageStarredAndQueue(context.Background(), messageID, true); err != nil {
		t.Fatalf("SetMessageStarredAndQueue() error = %v", err)
	}
	var count int
	var folderID, provider string
	if err := db.Read().QueryRow(`SELECT COUNT(*), COALESCE(MAX(folder_id), ''), COALESCE(MAX(provider_type), '') FROM message_mutations`).Scan(&count, &folderID, &provider); err != nil {
		t.Fatalf("query provider mutation: %v", err)
	}
	if count != 1 || folderID != "" || provider != MessageMutationProviderGmail {
		t.Fatalf("provider mutation count=%d folder=%q provider=%q", count, folderID, provider)
	}
}

func seedMoveMutationTest(t *testing.T, provider string) (*DB, int64) {
	t.Helper()
	ctx := t.Context()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, provider); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "archive", AccountID: "acc", RemoteID: "Archive", ProviderRemoteID: "ARCHIVE", Name: "Archive", Role: "archive", Selectable: true},
		{ID: "projects", AccountID: "acc", RemoteID: "Projects", ProviderRemoteID: "Label_Projects", Name: "Projects", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
		AccountID: "acc", FolderID: "inbox", RemoteUID: 42,
		MessageID: "<move@example.com>", Subject: "Move", FromEmail: "sender@example.com",
		DateSent: time.Now(), IsRead: false, IsStarred: true,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	messageID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<move@example.com>")
	if err != nil || messageID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", messageID, err)
	}
	return db, messageID
}

func TestMoveMessageQueuesWithOptimisticFolderStateAtomically(t *testing.T) {
	db, messageID := seedMoveMutationTest(t, "imap")
	ctx := t.Context()
	if err := db.MoveMessageAndQueue(ctx, messageID, "inbox", "archive"); err != nil {
		t.Fatalf("MoveMessageAndQueue() error = %v", err)
	}
	var sourceDeleted, destinationDeleted, destinationRead, destinationStarred int
	if err := db.Read().QueryRow(`SELECT is_deleted FROM message_folder_state WHERE message_id = ? AND folder_id = 'inbox'`, messageID).Scan(&sourceDeleted); err != nil {
		t.Fatalf("query source state: %v", err)
	}
	if err := db.Read().QueryRow(`SELECT is_deleted, is_read, is_starred FROM message_folder_state WHERE message_id = ? AND folder_id = 'archive'`, messageID).
		Scan(&destinationDeleted, &destinationRead, &destinationStarred); err != nil {
		t.Fatalf("query destination state: %v", err)
	}
	if sourceDeleted != 1 || destinationDeleted != 0 || destinationRead != 0 || destinationStarred != 1 {
		t.Fatalf("move states source_deleted=%d destination_deleted=%d read=%d starred=%d", sourceDeleted, destinationDeleted, destinationRead, destinationStarred)
	}
	var mutation MessageMutation
	mutation, err := scanMessageMutation(db.Read().QueryRow(messageMutationSelect+` WHERE message_id = ? AND kind = ?`, messageID, MessageMutationMove))
	if err != nil {
		t.Fatalf("query move mutation: %v", err)
	}
	if mutation.FolderID != "inbox" || mutation.DestinationFolderID != "archive" || mutation.Status != MessageMutationPending {
		t.Fatalf("queued move = %#v", mutation)
	}
}

func TestMoveMessageRollsBackLocalStateWhenQueueFails(t *testing.T) {
	db, messageID := seedMoveMutationTest(t, "imap")
	ctx := t.Context()
	if _, err := db.Write().Exec(`CREATE TRIGGER reject_move_queue BEFORE INSERT ON message_mutations WHEN NEW.kind = 'move' BEGIN SELECT RAISE(ABORT, 'reject move'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if err := db.MoveMessageAndQueue(ctx, messageID, "inbox", "archive"); err == nil {
		t.Fatal("MoveMessageAndQueue() error = nil, want rollback")
	}
	var sourceDeleted, destinationCount int
	if err := db.Read().QueryRow(`SELECT is_deleted FROM message_folder_state WHERE message_id = ? AND folder_id = 'inbox'`, messageID).Scan(&sourceDeleted); err != nil {
		t.Fatalf("query source state: %v", err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_folder_state WHERE message_id = ? AND folder_id = 'archive'`, messageID).Scan(&destinationCount); err != nil {
		t.Fatalf("query destination state: %v", err)
	}
	if sourceDeleted != 0 || destinationCount != 0 {
		t.Fatalf("rolled back move source_deleted=%d destination_count=%d", sourceDeleted, destinationCount)
	}
}

func TestMoveMessageKeepsLatestDestinationWhileProcessing(t *testing.T) {
	db, messageID := seedMoveMutationTest(t, "imap")
	ctx := t.Context()
	if err := db.MoveMessageAndQueue(ctx, messageID, "inbox", "archive"); err != nil {
		t.Fatalf("queue first move: %v", err)
	}
	claimed, err := db.ClaimDueMessageMutations(ctx, time.Now(), 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim first move = %#v, %v", claimed, err)
	}
	if err := db.MoveMessageAndQueue(ctx, messageID, "archive", "projects"); err != nil {
		t.Fatalf("queue newer move: %v", err)
	}
	if err := db.AdvanceMessageMoveMutation(ctx, claimed[0].ID, "archive", "", 84); err != nil {
		t.Fatalf("advance first move: %v", err)
	}
	if err := db.CompleteMessageMutation(ctx, claimed[0].ID); err != nil {
		t.Fatalf("complete old move: %v", err)
	}
	mutation, err := db.GetMessageMutation(ctx, claimed[0].ID)
	if err != nil {
		t.Fatalf("GetMessageMutation() error = %v", err)
	}
	if mutation.FolderID != "archive" || mutation.DestinationFolderID != "projects" || mutation.Status != MessageMutationPending {
		t.Fatalf("newer move = %#v", mutation)
	}
	var archiveDeleted int
	var archiveUID sql.NullInt64
	if err := db.Read().QueryRow(`SELECT is_deleted, remote_uid FROM message_folder_state WHERE message_id = ? AND folder_id = 'archive'`, messageID).Scan(&archiveDeleted, &archiveUID); err != nil {
		t.Fatalf("query intermediate folder: %v", err)
	}
	if archiveDeleted != 1 || !archiveUID.Valid || archiveUID.Int64 != 84 {
		t.Fatalf("intermediate state deleted=%d uid=%v", archiveDeleted, archiveUID)
	}
}

func TestMoveMessageSurvivesRestart(t *testing.T) {
	db, messageID := seedMoveMutationTest(t, "imap")
	ctx := t.Context()
	if err := db.MoveMessageAndQueue(ctx, messageID, "inbox", "archive"); err != nil {
		t.Fatalf("MoveMessageAndQueue() error = %v", err)
	}
	claimed, err := db.ClaimDueMessageMutations(ctx, time.Now(), 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim move = %#v, %v", claimed, err)
	}
	if recovered, err := db.MarkInterruptedMessageMutationsPending(ctx); err != nil || recovered != 1 {
		t.Fatalf("recover interrupted move = %d, %v", recovered, err)
	}
	reclaimed, err := db.ClaimDueMessageMutations(ctx, time.Now(), 1)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].ID != claimed[0].ID || reclaimed[0].AttemptCount != 2 {
		t.Fatalf("reclaimed move = %#v, %v", reclaimed, err)
	}
}

func TestIMAPMoveWaitsForDestinationSyncWithoutRevivingSource(t *testing.T) {
	db, messageID := seedMoveMutationTest(t, "imap")
	ctx := t.Context()
	if err := db.MoveMessageAndQueue(ctx, messageID, "inbox", "archive"); err != nil {
		t.Fatalf("MoveMessageAndQueue() error = %v", err)
	}
	claimed, err := db.ClaimDueMessageMutations(ctx, time.Now(), 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim move = %#v, %v", claimed, err)
	}
	if err := db.AdvanceMessageMoveMutation(ctx, claimed[0].ID, "archive", "", 84); err != nil {
		t.Fatalf("advance move: %v", err)
	}
	if err := db.CompleteMessageMutation(ctx, claimed[0].ID); err != nil {
		t.Fatalf("complete move: %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
		AccountID: "acc", FolderID: "inbox", RemoteUID: 42, MessageID: "<move@example.com>",
		Subject: "Move", FromEmail: "sender@example.com", DateSent: time.Now(),
	}}); err != nil {
		t.Fatalf("stale source sync: %v", err)
	}
	var sourceDeleted, remaining int
	_ = db.Read().QueryRow(`SELECT is_deleted FROM message_folder_state WHERE message_id = ? AND folder_id = 'inbox'`, messageID).Scan(&sourceDeleted)
	_ = db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE id = ?`, claimed[0].ID).Scan(&remaining)
	if sourceDeleted != 1 || remaining != 1 {
		t.Fatalf("stale source deleted=%d remaining=%d", sourceDeleted, remaining)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
		AccountID: "acc", FolderID: "archive", RemoteUID: 84, MessageID: "<move@example.com>",
		Subject: "Move", FromEmail: "sender@example.com", DateSent: time.Now(),
	}}); err != nil {
		t.Fatalf("destination sync: %v", err)
	}
	_ = db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE id = ?`, claimed[0].ID).Scan(&remaining)
	if remaining != 0 {
		t.Fatalf("confirmed move remaining=%d", remaining)
	}
}

func TestProviderMoveIgnoresStaleSourceUntilDestinationIsConfirmed(t *testing.T) {
	db, messageID := seedMoveMutationTest(t, MessageMutationProviderGmail)
	ctx := t.Context()
	if _, err := db.Write().Exec(`UPDATE messages SET remote_message_id = 'gmail-message' WHERE id = ?`, messageID); err != nil {
		t.Fatalf("set provider identity: %v", err)
	}
	if err := db.MoveMessageAndQueue(ctx, messageID, "inbox", "archive"); err != nil {
		t.Fatalf("MoveMessageAndQueue() error = %v", err)
	}
	claimed, err := db.ClaimDueMessageMutations(ctx, time.Now(), 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim move = %#v, %v", claimed, err)
	}
	if err := db.AdvanceMessageMoveMutation(ctx, claimed[0].ID, "archive", "gmail-message", 0); err != nil {
		t.Fatalf("advance move: %v", err)
	}
	if err := db.CompleteMessageMutation(ctx, claimed[0].ID); err != nil {
		t.Fatalf("complete move: %v", err)
	}
	upsert := func(folderID string) error {
		_, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{{
			AccountID: "acc", FolderID: folderID, ProviderMessageID: "gmail-message",
			InternetMessageID: "<move@example.com>", Subject: "Move", FromEmail: "sender@example.com",
			DateSent: time.Now(), DateReceived: time.Now(), IsStarred: true,
		}})
		return err
	}
	if err := upsert("inbox"); err != nil {
		t.Fatalf("stale provider source sync: %v", err)
	}
	var sourceDeleted, destinationDeleted, remaining int
	_ = db.Read().QueryRow(`SELECT is_deleted FROM message_folder_state WHERE message_id = ? AND folder_id = 'inbox'`, messageID).Scan(&sourceDeleted)
	_ = db.Read().QueryRow(`SELECT is_deleted FROM message_folder_state WHERE message_id = ? AND folder_id = 'archive'`, messageID).Scan(&destinationDeleted)
	_ = db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE id = ?`, claimed[0].ID).Scan(&remaining)
	if sourceDeleted != 1 || destinationDeleted != 0 || remaining != 1 {
		t.Fatalf("stale provider move source=%d destination=%d remaining=%d", sourceDeleted, destinationDeleted, remaining)
	}
	if err := upsert("archive"); err != nil {
		t.Fatalf("provider destination sync: %v", err)
	}
	_ = db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE id = ?`, claimed[0].ID).Scan(&remaining)
	if remaining != 0 {
		t.Fatalf("confirmed provider move remaining=%d", remaining)
	}
}

func TestMessageMutationSurvivesRestartAndWaitsForRemoteConfirmation(t *testing.T) {
	db, messageID := seedMessageMutationTest(t, "imap", []UpsertFolderInput{{
		ID: "inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true,
	}})
	ctx := context.Background()
	if err := db.SetMessageReadAndQueue(ctx, messageID, false); err != nil {
		t.Fatalf("SetMessageReadAndQueue() error = %v", err)
	}
	claimed, err := db.ClaimDueMessageMutations(ctx, time.Now(), 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDueMessageMutations() = %#v, %v", claimed, err)
	}
	if recovered, err := db.MarkInterruptedMessageMutationsPending(ctx); err != nil || recovered != 1 {
		t.Fatalf("MarkInterruptedMessageMutationsPending() = %d, %v", recovered, err)
	}
	claimed, err = db.ClaimDueMessageMutations(ctx, time.Now(), 10)
	if err != nil || len(claimed) != 1 || claimed[0].AttemptCount != 2 {
		t.Fatalf("reclaimed mutations = %#v, %v", claimed, err)
	}
	if err := db.CompleteMessageMutation(ctx, claimed[0].ID); err != nil {
		t.Fatalf("CompleteMessageMutation() error = %v", err)
	}

	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
		AccountID: "acc", FolderID: "inbox", RemoteUID: 1, MessageID: "<mutation@example.com>",
		Subject: "Mutation", FromEmail: "sender@example.com", DateSent: time.Now(), IsRead: true,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages(stale) error = %v", err)
	}
	var isRead int
	var status string
	if err := db.Read().QueryRow(`
		SELECT mfs.is_read, mm.status
		FROM message_folder_state mfs JOIN message_mutations mm ON mm.message_id = mfs.message_id AND mm.folder_id = mfs.folder_id
		WHERE mfs.message_id = ?`, messageID).Scan(&isRead, &status); err != nil {
		t.Fatalf("query protected mutation: %v", err)
	}
	if isRead != 0 || status != MessageMutationApplied {
		t.Fatalf("stale sync read=%d status=%q", isRead, status)
	}

	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
		AccountID: "acc", FolderID: "inbox", RemoteUID: 1, MessageID: "<mutation@example.com>",
		Subject: "Mutation", FromEmail: "sender@example.com", DateSent: time.Now(), IsRead: false,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages(confirmed) error = %v", err)
	}
	var remaining int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE message_id = ?`, messageID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("confirmed mutation count = %d, %v", remaining, err)
	}
}

func TestNewMessageStateDoesNotGetLostBehindProcessingMutation(t *testing.T) {
	db, messageID := seedMessageMutationTest(t, "imap", []UpsertFolderInput{{
		ID: "inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true,
	}})
	ctx := context.Background()
	if err := db.SetMessageReadAndQueue(ctx, messageID, false); err != nil {
		t.Fatalf("SetMessageReadAndQueue(false) error = %v", err)
	}
	claimed, err := db.ClaimDueMessageMutations(ctx, time.Now(), 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDueMessageMutations() = %#v, %v", claimed, err)
	}
	if err := db.SetMessageReadAndQueue(ctx, messageID, true); err != nil {
		t.Fatalf("SetMessageReadAndQueue(true) error = %v", err)
	}
	if err := db.CompleteMessageMutation(ctx, claimed[0].ID); err != nil {
		t.Fatalf("CompleteMessageMutation(old state) error = %v", err)
	}
	mutation, err := db.GetMessageMutation(ctx, claimed[0].ID)
	if err != nil || mutation.Status != MessageMutationPending || !mutation.TargetValue || mutation.AttemptCount != 0 {
		t.Fatalf("newer mutation = %#v, %v", mutation, err)
	}
}

func TestIMAPFlagRefreshConfirmsAppliedMutationWithoutRevertingIt(t *testing.T) {
	db, messageID := seedMessageMutationTest(t, "imap", []UpsertFolderInput{{
		ID: "inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true,
	}})
	ctx := context.Background()
	if err := db.SetMessageStarredAndQueue(ctx, messageID, true); err != nil {
		t.Fatalf("SetMessageStarredAndQueue() error = %v", err)
	}
	claimed, err := db.ClaimDueMessageMutations(ctx, time.Now(), 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDueMessageMutations() = %#v, %v", claimed, err)
	}
	if err := db.CompleteMessageMutation(ctx, claimed[0].ID); err != nil {
		t.Fatalf("CompleteMessageMutation() error = %v", err)
	}
	if _, err := db.BatchUpdateFlags(ctx, "inbox", []FlagUpdate{{UID: 1, IsRead: true, IsStarred: false}}); err != nil {
		t.Fatalf("BatchUpdateFlags(stale) error = %v", err)
	}
	var starred, remaining int
	if err := db.Read().QueryRow(`SELECT is_starred FROM message_folder_state WHERE message_id = ? AND folder_id = 'inbox'`, messageID).Scan(&starred); err != nil || starred != 1 {
		t.Fatalf("starred after stale refresh = %d, %v", starred, err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE id = ?`, claimed[0].ID).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("mutation after stale refresh = %d, %v", remaining, err)
	}
	if _, err := db.BatchUpdateFlags(ctx, "inbox", []FlagUpdate{{UID: 1, IsRead: true, IsStarred: true}}); err != nil {
		t.Fatalf("BatchUpdateFlags(confirmed) error = %v", err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE id = ?`, claimed[0].ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("confirmed mutation count = %d, %v", remaining, err)
	}
}

func TestProviderSyncConfirmsAppliedMutationWithoutRevertingIt(t *testing.T) {
	db, messageID := seedMessageMutationTest(t, MessageMutationProviderGmail, []UpsertFolderInput{{
		ID: "inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true,
	}})
	ctx := context.Background()
	if err := db.SetMessageReadAndQueue(ctx, messageID, false); err != nil {
		t.Fatalf("SetMessageReadAndQueue() error = %v", err)
	}
	claimed, err := db.ClaimDueMessageMutations(ctx, time.Now(), 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDueMessageMutations() = %#v, %v", claimed, err)
	}
	if err := db.CompleteMessageMutation(ctx, claimed[0].ID); err != nil {
		t.Fatalf("CompleteMessageMutation() error = %v", err)
	}
	upsert := func(read bool) error {
		_, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{{
			AccountID: "acc", FolderID: "inbox", ProviderMessageID: "gmail-message",
			InternetMessageID: "<mutation@example.com>", Subject: "Mutation", FromEmail: "sender@example.com",
			DateSent: time.Now(), DateReceived: time.Now(), IsRead: read,
		}})
		return err
	}
	if err := upsert(true); err != nil {
		t.Fatalf("UpsertProviderSyncMessages(stale) error = %v", err)
	}
	var isRead, remaining int
	if err := db.Read().QueryRow(`SELECT is_read FROM message_folder_state WHERE message_id = ? AND folder_id = 'inbox'`, messageID).Scan(&isRead); err != nil || isRead != 0 {
		t.Fatalf("read after stale provider sync = %d, %v", isRead, err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE id = ?`, claimed[0].ID).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("provider mutation after stale sync = %d, %v", remaining, err)
	}
	if err := upsert(false); err != nil {
		t.Fatalf("UpsertProviderSyncMessages(confirmed) error = %v", err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE id = ?`, claimed[0].ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("confirmed provider mutation count = %d, %v", remaining, err)
	}
}

func TestMessageMutationQueueAndLocalStateAreAtomic(t *testing.T) {
	db, messageID := seedMessageMutationTest(t, "imap", []UpsertFolderInput{{
		ID: "inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true,
	}})
	if _, err := db.Write().Exec(`
		CREATE TRIGGER fail_message_mutation_insert
		BEFORE INSERT ON message_mutations
		BEGIN
			SELECT RAISE(ABORT, 'forced mutation queue failure');
		END;
	`); err != nil {
		t.Fatalf("create queue failure trigger: %v", err)
	}
	if err := db.SetMessageReadAndQueue(context.Background(), messageID, false); err == nil || !strings.Contains(err.Error(), "forced mutation queue failure") {
		t.Fatalf("SetMessageReadAndQueue() error = %v", err)
	}
	var isRead int
	if err := db.Read().QueryRow(`SELECT is_read FROM message_folder_state WHERE message_id = ?`, messageID).Scan(&isRead); err != nil || isRead != 1 {
		t.Fatalf("local state after queue failure read=%d error=%v", isRead, err)
	}
	var mutationCount int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations`).Scan(&mutationCount); err != nil && err != sql.ErrNoRows {
		t.Fatalf("query mutation count: %v", err)
	}
	if mutationCount != 0 {
		t.Fatalf("mutation count after failed transaction = %d", mutationCount)
	}
}
