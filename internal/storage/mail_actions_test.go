package storage

import (
	"context"
	"database/sql"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestMigrateV150ToV151AddsMailActions(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `
		DROP INDEX idx_message_mutations_action;
		DROP INDEX idx_label_mutation_queue_action;
		ALTER TABLE message_mutations DROP COLUMN action_id;
		ALTER TABLE message_mutations DROP COLUMN action_kind;
		ALTER TABLE message_mutations DROP COLUMN action_source;
		ALTER TABLE message_mutations DROP COLUMN action_rule_id;
		ALTER TABLE label_mutation_queue DROP COLUMN action_id;
		ALTER TABLE label_mutation_queue DROP COLUMN action_kind;
		ALTER TABLE label_mutation_queue DROP COLUMN action_source;
		ALTER TABLE label_mutation_queue DROP COLUMN action_rule_id;
		DELETE FROM schema_version;
		INSERT INTO schema_version (version) VALUES (150);
		INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'imap', 'alex@example.com');
		INSERT INTO messages (id, account_id, internet_message_id, subject) VALUES (1, 'acc', '<lunch@example.com>', 'Lunch on Friday?');
		INSERT INTO message_mutations (id, account_id, message_id, folder_id, provider_type, kind, target_value) VALUES ('old', 'acc', 1, '', 'imap', 'read', 1);
		INSERT INTO label_mutation_queue (account_id, message_id, folder_id, provider_type, operation, label_name) VALUES ('acc', 1, '', 'keyword', 'add', 'Travel');`); err != nil {
		t.Fatalf("set up schema 150: %v", err)
	}
	for range 2 {
		if err := migrateV150ToV151(db.Write()); err != nil {
			t.Fatalf("migrateV150ToV151() error = %v", err)
		}
	}
	for _, table := range []string{"message_mutations", "label_mutation_queue"} {
		var id, kind, source, rule string
		if err := db.Read().QueryRow(`SELECT action_id, action_kind, action_source, action_rule_id FROM `+table).Scan(&id, &kind, &source, &rule); err != nil {
			t.Fatalf("%s after migration: %v", table, err)
		}
		if id != "" || kind != "" || source != "" || rule != "" {
			t.Fatalf("%s: an old change got an origin: %q %q %q %q", table, id, kind, source, rule)
		}
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name IN ('idx_message_mutations_action', 'idx_label_mutation_queue_action')`); n != 2 {
		t.Fatalf("action indexes = %d, want 2", n)
	}
	if n := countRows(t, db, `SELECT MAX(version) FROM schema_version`); n != 151 {
		t.Fatalf("schema version = %d, want 151", n)
	}
}

func TestMigrateV150ToV151WithoutQueueTables(t *testing.T) {
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP); INSERT INTO schema_version (version) VALUES (150)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV150ToV151(raw); err != nil {
			t.Fatalf("migrateV150ToV151() error = %v", err)
		}
	}
	var version int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 151 {
		t.Fatalf("version = %d, %v", version, err)
	}
}

func mutationAction(t *testing.T, db *DB, where string, args ...any) MailAction {
	t.Helper()
	var action MailAction
	if err := db.Read().QueryRow(`SELECT action_id, action_kind, action_source, action_rule_id FROM message_mutations WHERE `+where, args...).
		Scan(&action.ID, &action.Kind, &action.Source, &action.RuleID); err != nil {
		t.Fatalf("action of mutation %s: %v", where, err)
	}
	return action
}

func TestQueuedChangesRecordTheirAction(t *testing.T) {
	ctx := context.Background()
	db := seedUndoTest(t, "imap")

	// Without an action, nothing is recorded.
	if err := db.SetMessagesReadAndQueueForUser(ctx, []int64{2}, true, "default"); err != nil {
		t.Fatal(err)
	}
	if got := mutationAction(t, db, `message_id = 2 AND kind = 'read'`); got != (MailAction{}) {
		t.Fatalf("change without an action recorded %+v", got)
	}

	// A step inside a larger action stays part of it.
	moveCtx := WithUserMailAction(ctx, MailActionMove)
	moveCtx = WithUserMailAction(moveCtx, MailActionDelete)
	move := MailActionFrom(moveCtx)
	if move.Kind != MailActionMove || move.Source != MailActionSourceYou || move.ID == "" {
		t.Fatalf("action = %+v, want the outer move", move)
	}
	if err := db.MoveMessagesAndQueueForUser(moveCtx, []int64{2}, "inbox", "archive", "default"); err != nil {
		t.Fatal(err)
	}
	if got := mutationAction(t, db, `message_id = 2 AND kind = 'move'`); got != move {
		t.Fatalf("move recorded %+v, want %+v", got, move)
	}

	// A newer action takes over the change it replaces.
	again := WithUserMailAction(ctx, MailActionMove)
	if err := db.MoveMessagesAndQueueForUser(again, []int64{2}, "archive", "projects", "default"); err != nil {
		t.Fatal(err)
	}
	if got := mutationAction(t, db, `message_id = 2 AND kind = 'move'`); got != MailActionFrom(again) {
		t.Fatalf("second move recorded %+v, want %+v", got, MailActionFrom(again))
	}

	// Read changes take over on conflict too.
	read := WithUserMailAction(ctx, MailActionMarkUnread)
	if err := db.SetMessagesReadAndQueueForUser(read, []int64{2}, false, "default"); err != nil {
		t.Fatal(err)
	}
	// IMAP queues read changes per folder; the message is in Projects now.
	if got := mutationAction(t, db, `message_id = 2 AND kind = 'read' AND folder_id = 'projects'`); got != MailActionFrom(read) {
		t.Fatalf("read change recorded %+v", got)
	}

	deleted := WithUserMailAction(ctx, MailActionDelete)
	if err := db.PermanentlyDeleteMessagesAndQueueForUser(deleted, []int64{1}, "projects", "default"); err != nil {
		t.Fatal(err)
	}
	if got := mutationAction(t, db, `message_id = 1 AND kind = 'delete'`); got != MailActionFrom(deleted) {
		t.Fatalf("delete recorded %+v", got)
	}

	// Label changes, both ways they are queued.
	info, err := db.GetMessageMutationInfoForUser(ctx, 2, "default")
	if err != nil || info == nil {
		t.Fatalf("mutation info: %v", err)
	}
	labelled := WithUserMailAction(ctx, MailActionLabelAdd)
	if err := db.QueueUserLabels(labelled, "default", []ThreadMessageMutationInfo{{MessageID: 2, MessageMutationInfo: *info}}, "Travel", LabelMutationAdd); err != nil {
		t.Fatal(err)
	}
	removed := WithUserMailAction(ctx, MailActionLabelRemove)
	if err := db.ReplaceLabelMutation(removed, "acc", 1, "projects", LabelProviderKeyword, LabelMutationRemove, "Family"); err != nil {
		t.Fatal(err)
	}
	operations, err := db.ListMailOperationsForUser(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	byLabel := map[string]models.MailOperationSummary{}
	for _, operation := range operations {
		if operation.LabelName != "" {
			byLabel[operation.LabelName] = operation
		}
	}
	if got := byLabel["Travel"]; got.ActionID != MailActionFrom(labelled).ID || got.ActionKind != MailActionLabelAdd || got.ActionSource != MailActionSourceYou {
		t.Fatalf("label add = %+v", got)
	}
	if got := byLabel["Family"]; got.ActionID != MailActionFrom(removed).ID || got.ActionKind != MailActionLabelRemove {
		t.Fatalf("label remove = %+v", got)
	}
}

func TestEmptyFolderIsOneAction(t *testing.T) {
	ctx := context.Background()
	db := newMailRulesTestDB(t, "imap")
	// More than one chunk of messages in Trash.
	if _, err := db.Write().ExecContext(ctx, `
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 501)
		INSERT INTO messages (id, account_id, internet_message_id, subject) SELECT i, 'acc', '<old-' || i || '@example.com>', 'Weekend plans' FROM n`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO message_folder_state (message_id, folder_id, remote_uid) SELECT id, 'trash', id FROM messages`); err != nil {
		t.Fatal(err)
	}
	emptied := WithUserMailAction(ctx, MailActionEmptyFolder)
	result, err := db.EmptyFolderForUser(emptied, "trash", "default", func(string) bool { return true })
	if err != nil || result.Removed != 501 {
		t.Fatalf("empty Trash = %+v, %v", result, err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM message_mutations WHERE action_id = ? AND action_kind = ?`, MailActionFrom(emptied).ID, MailActionEmptyFolder); n != 501 {
		t.Fatalf("%d of 501 deletes belong to the Empty folder action", n)
	}
}

func TestMailRulesRecordTheirAction(t *testing.T) {
	ctx := t.Context()
	db := newMailRulesTestDB(t, "imap")
	rule := saveTestRule(t, db, models.MailRule{Name: "Receipts", Conditions: fromBakery(), Actions: []models.MailRuleAction{
		{Kind: "mark_read"},
		{Kind: "move", Folder: &models.MailRuleFolderTarget{FolderID: "receipts"}},
	}})
	first := arrive(t, db, "inbox", SyncMessage{MessageID: "<r1@harbor-bakery.example>", FromEmail: "news@harbor-bakery.example", Subject: "Your receipt"})
	second := arrive(t, db, "inbox", SyncMessage{MessageID: "<r2@harbor-bakery.example>", FromEmail: "news@harbor-bakery.example", Subject: "Your receipt"})
	if _, err := db.ApplyPendingMailRules(ctx, "acc"); err != nil {
		t.Fatal(err)
	}
	firstMove := mutationAction(t, db, `message_id = ? AND kind = 'move'`, first)
	if firstMove.Source != MailActionSourceRule || firstMove.RuleID != rule.ID || firstMove.Kind != MailActionMove {
		t.Fatalf("rule move recorded %+v", firstMove)
	}
	if got := mutationAction(t, db, `message_id = ? AND kind = 'move'`, second); got != firstMove {
		t.Fatalf("one run of a rule should keep its moves together: %+v vs %+v", got, firstMove)
	}
	if got := mutationAction(t, db, `message_id = ? AND kind = 'read'`, first); got.Kind != MailActionMarkRead || got.ID == firstMove.ID {
		t.Fatalf("rule read recorded %+v", got)
	}

	names := func() map[string]bool {
		operations, err := db.ListMailOperationsForUser(ctx, "default")
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, operation := range operations {
			if operation.ActionSource == MailActionSourceRule {
				seen[operation.ActionRuleName] = true
			}
		}
		return seen
	}
	if got := names(); !got["Receipts"] || len(got) != 1 {
		t.Fatalf("rule names = %v, want Receipts", got)
	}
	if err := db.DeleteMailRuleGuarded(ctx, "default", rule.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := names(); !got[""] || len(got) != 1 {
		t.Fatalf("rule names after deleting it = %v, want none", got)
	}
}

func TestSpamTrainingAndMoveAreOneAction(t *testing.T) {
	ctx := context.Background()
	db := newMailRulesTestDB(t, "imap")
	id := arrive(t, db, "inbox", SyncMessage{MessageID: "<offer@example.net>", FromEmail: "sam@example.net", Subject: "Weekend plans"})
	info, err := db.GetMessageMutationInfoForUser(ctx, id, "default")
	if err != nil || info == nil {
		t.Fatalf("mutation info: %v", err)
	}
	spam := WithUserMailAction(ctx, MailActionSpam)
	if err := db.QueueUserIMAPSpam(spam, "default", []ThreadMessageMutationInfo{{MessageID: id, MessageMutationInfo: *info}}, "junk", true); err != nil {
		t.Fatal(err)
	}
	want := MailActionFrom(spam)
	if got := mutationAction(t, db, `message_id = ? AND kind = 'move'`, id); got != want {
		t.Fatalf("spam move recorded %+v, want %+v", got, want)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM label_mutation_queue WHERE message_id = ? AND action_id = ? AND action_kind = ?`, id, want.ID, MailActionSpam); n != 2 {
		t.Fatalf("%d of 2 spam keyword changes belong to the action", n)
	}
}
