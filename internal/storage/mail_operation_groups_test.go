package storage

import (
	"context"
	"testing"
)

func TestMailOperationsAreGroupedByAction(t *testing.T) {
	ctx := context.Background()
	db := seedUndoTest(t, "imap")

	// Queued before Gofer recorded actions: listed on its own.
	if err := db.SetMessagesReadAndQueueForUser(ctx, []int64{2}, true, "default"); err != nil {
		t.Fatal(err)
	}
	starCtx := WithUserMailAction(ctx, MailActionStar)
	if err := db.SetMessagesStarredAndQueueForUser(starCtx, []int64{2}, true, "default"); err != nil {
		t.Fatal(err)
	}
	moveCtx := WithUserMailAction(ctx, MailActionMove)
	if err := db.MoveMessagesAndQueueForUser(moveCtx, []int64{1}, "projects", "archive", "default"); err != nil {
		t.Fatal(err)
	}
	if err := db.MoveMessagesAndQueueForUser(moveCtx, []int64{2}, "inbox", "archive", "default"); err != nil {
		t.Fatal(err)
	}
	move := MailActionFrom(moveCtx)
	// Projects is deleted on the server, which parks the move out of it, and
	// the move out of Inbox failed.
	removeProjectsFolder(t, db)
	if _, err := db.Write().Exec(`UPDATE message_mutations SET status = 'failed', attempt_count = 1, last_error = 'timeout' WHERE message_id = 2 AND kind = 'move'`); err != nil {
		t.Fatal(err)
	}

	groups, err := db.ListMailOperationGroupsForUser(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %+v, want the move and the star", groups)
	}
	got := groups[0]
	if got.ActionID != move.ID || got.ActionKind != MailActionMove || got.ActionSource != MailActionSourceYou {
		t.Fatalf("first group = %+v, want the move, which needs attention", got)
	}
	if got.Messages != 2 || got.Jobs != 2 || got.Failed != 1 || got.Parked != 1 || got.Accounts != 1 || got.AccountEmail != "alex@example.com" || !got.NeedsAttention() {
		t.Fatalf("move counts = %+v", got)
	}
	// Two source folders: none is named. One destination, as the sidebar
	// names it.
	if got.FolderID != "" || got.DestinationFolderID != "archive" || got.DestinationName != "Archive" || got.DestinationRole != "archive" {
		t.Fatalf("move folders = %+v", got)
	}
	if star := groups[1]; star.ActionKind != MailActionStar || star.FolderID != "inbox" || star.FolderName != "Inbox" || star.Messages != 1 || star.NeedsAttention() {
		t.Fatalf("star group = %+v", star)
	}

	ungrouped, err := db.ListUngroupedMailOperationsForUser(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(ungrouped) != 1 || ungrouped[0].Operation != MessageMutationRead || ungrouped[0].ActionID != "" {
		t.Fatalf("ungrouped = %+v, want only the read change", ungrouped)
	}

	messages, err := db.ListMailActionMessagesForUser(ctx, "default", move.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %+v", messages)
	}
	subjects := map[string]bool{}
	for _, message := range messages {
		subjects[message.Subject] = true
		if message.FromEmail != "sam@example.com" || message.DestinationName != "Archive" {
			t.Fatalf("message = %+v", message)
		}
	}
	if !subjects["Lunch on Friday?"] || !subjects["Weekend plans"] {
		t.Fatalf("subjects = %v", subjects)
	}
	if limited, err := db.ListMailActionMessagesForUser(ctx, "default", move.ID, 1); err != nil || len(limited) != 1 {
		t.Fatalf("limited = %d, %v", len(limited), err)
	}
	if others, err := db.ListMailActionMessagesForUser(ctx, "other", move.ID, 50); err != nil || len(others) != 0 {
		t.Fatalf("another user sees %d messages, %v", len(others), err)
	}
	if accounts, err := db.MailActionAccountsForUser(ctx, "other", move.ID); err != nil || len(accounts) != 0 {
		t.Fatalf("another user's accounts = %v, %v", accounts, err)
	}
	accounts, err := db.MailActionAccountsForUser(ctx, "default", move.ID)
	if err != nil || len(accounts) != 1 || accounts[0] != "acc" {
		t.Fatalf("accounts = %v, %v", accounts, err)
	}

	// Another user can't retry or undo it.
	if n, err := db.RetryMailActionForUser(ctx, "other", move.ID, "acc", nil); err != nil || n != 0 {
		t.Fatalf("another user's retry = %d, %v", n, err)
	}
	if n, err := db.UndoMailActionForUser(ctx, "other", move.ID, "acc", nil); err != nil || n != 0 {
		t.Fatalf("another user's undo = %d, %v", n, err)
	}

	// Retry tries the failed move again and leaves the parked one alone.
	retried, err := db.RetryMailActionForUser(ctx, "default", move.ID, "acc", nil)
	if err != nil || retried != 1 {
		t.Fatalf("retry = %d, %v", retried, err)
	}
	var status string
	if err := db.Read().QueryRow(`SELECT status FROM message_mutations WHERE message_id = 2 AND kind = 'move'`).Scan(&status); err != nil || status != MessageMutationPending {
		t.Fatalf("retried move status = %q, %v", status, err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM message_mutations WHERE message_id = 1 AND kind = 'move' AND status = 'failed' AND next_attempt_at = ?`, parkedRetryAt); n != 1 {
		t.Fatalf("parked move was retried")
	}

	// Undo takes back the parked move only.
	undone, err := db.UndoMailActionForUser(ctx, "default", move.ID, "acc", nil)
	if err != nil || undone != 1 {
		t.Fatalf("undo = %d, %v", undone, err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM message_mutations WHERE kind = 'move'`); n != 1 {
		t.Fatalf("moves left = %d, want the one out of Inbox", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM message_mutations WHERE kind = 'move' AND message_id = 2`); n != 1 {
		t.Fatalf("the move that can still finish was undone")
	}
}

func TestUndoMailActionTakesBackSpamKeywordsAndMove(t *testing.T) {
	ctx := context.Background()
	db := seedUndoTest(t, "imap")
	spam := WithUserMailAction(ctx, MailActionSpam)
	info, err := db.GetMessageMutationInfoForUser(ctx, 1, "default")
	if err != nil || info == nil {
		t.Fatalf("mutation info: %v", err)
	}
	if err := db.QueueUserLabels(spam, "default", []ThreadMessageMutationInfo{{MessageID: 1, MessageMutationInfo: *info}}, "$Junk", LabelMutationAdd); err != nil {
		t.Fatal(err)
	}
	if err := db.MoveMessagesAndQueueForUser(spam, []int64{1}, "projects", "archive", "default"); err != nil {
		t.Fatal(err)
	}
	removeProjectsFolder(t, db)
	groups, err := db.ListMailOperationGroupsForUser(ctx, "default")
	if err != nil || len(groups) != 1 || groups[0].Parked != 2 || groups[0].Messages != 1 || groups[0].Jobs != 2 {
		t.Fatalf("groups = %+v, %v", groups, err)
	}
	undone, err := db.UndoMailActionForUser(ctx, "default", MailActionFrom(spam).ID, "acc", nil)
	if err != nil || undone != 2 {
		t.Fatalf("undo = %d, %v", undone, err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM message_mutations`) + countRows(t, db, `SELECT COUNT(*) FROM label_mutation_queue`); n != 0 {
		t.Fatalf("%d changes still queued", n)
	}
}

func TestMailOperationsAttentionCountsAsTheList(t *testing.T) {
	ctx := context.Background()
	db := seedUndoTest(t, "imap")
	count, fingerprint, err := db.MailOperationsAttentionForUser(ctx, "default")
	if err != nil || count != 0 || fingerprint != "" {
		t.Fatalf("empty = %d %q %v", count, fingerprint, err)
	}
	// Queued work doesn't need attention.
	moveCtx := WithUserMailAction(ctx, MailActionMove)
	if err := db.MoveMessagesAndQueueForUser(moveCtx, []int64{1}, "projects", "archive", "default"); err != nil {
		t.Fatal(err)
	}
	if err := db.MoveMessagesAndQueueForUser(moveCtx, []int64{2}, "inbox", "archive", "default"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMessagesReadAndQueueForUser(ctx, []int64{2}, true, "default"); err != nil {
		t.Fatal(err)
	}
	if count, _, _ := db.MailOperationsAttentionForUser(ctx, "default"); count != 0 {
		t.Fatalf("queued work counted: %d", count)
	}
	// One action with a parked and a failed change counts once; a failed
	// change with no action counts on its own.
	removeProjectsFolder(t, db)
	if _, err := db.Write().Exec(`UPDATE message_mutations SET status = 'failed', attempt_count = 1, last_error = 'timeout', updated_at = '2026-11-20 10:00:00' WHERE message_id = 2`); err != nil {
		t.Fatal(err)
	}
	count, first, err := db.MailOperationsAttentionForUser(ctx, "default")
	if err != nil || count != 2 || first == "" {
		t.Fatalf("attention = %d %q %v, want the action and the read change", count, first, err)
	}
	list, err := db.ListMailOperationGroupsForUser(ctx, "default")
	if err != nil || len(list) != 1 || !list[0].NeedsAttention() {
		t.Fatalf("groups = %+v %v", list, err)
	}
	if others, _, err := db.MailOperationsAttentionForUser(ctx, "other"); err != nil || others != 0 {
		t.Fatalf("another user's count = %d %v", others, err)
	}
	// A newer failure changes the fingerprint even when the count doesn't.
	if _, err := db.Write().Exec(`UPDATE message_mutations SET updated_at = '2026-11-21 09:00:00' WHERE message_id = 2 AND kind = 'read'`); err != nil {
		t.Fatal(err)
	}
	if count, second, _ := db.MailOperationsAttentionForUser(ctx, "default"); count != 2 || second == first {
		t.Fatalf("fingerprint %q didn't change from %q", second, first)
	}
}
