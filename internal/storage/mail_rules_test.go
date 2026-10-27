package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func newMailRulesTestDB(t *testing.T, provider string) *DB {
	t.Helper()
	db := newContactsTestDB(t)
	ctx := t.Context()
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'robin@example.org')`, provider); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "archive", AccountID: "acc", RemoteID: "Archive", Name: "Archive", Role: "archive", Selectable: true},
		{ID: "trash", AccountID: "acc", RemoteID: "Trash", Name: "Trash", Role: "trash", Selectable: true},
		{ID: "junk", AccountID: "acc", RemoteID: "Junk", Name: "Junk", Role: "junk", Selectable: true},
		{ID: "sent", AccountID: "acc", RemoteID: "Sent", Name: "Sent", Role: "sent", Selectable: true},
		{ID: "receipts", AccountID: "acc", RemoteID: "Receipts", Name: "Receipts", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

func saveTestRule(t *testing.T, db *DB, rule models.MailRule) models.MailRule {
	t.Helper()
	if rule.Name == "" {
		rule.Name = "Bakery"
	}
	rule.Enabled = true
	saved, err := db.SaveMailRuleGuarded(t.Context(), "default", rule, nil)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

var mailRuleTestUID uint32

// arrive syncs a message as new mail; backfill as mail a first sync finds.
func arrive(t *testing.T, db *DB, folderID string, msg SyncMessage) int64 {
	t.Helper()
	msg.Arrived = true
	return syncMailRuleTestMessage(t, db, folderID, msg)
}

func backfill(t *testing.T, db *DB, folderID string, msg SyncMessage) int64 {
	t.Helper()
	return syncMailRuleTestMessage(t, db, folderID, msg)
}

func syncMailRuleTestMessage(t *testing.T, db *DB, folderID string, msg SyncMessage) int64 {
	t.Helper()
	mailRuleTestUID++
	msg.AccountID, msg.FolderID, msg.RemoteUID = "acc", folderID, mailRuleTestUID
	if msg.DateSent.IsZero() {
		msg.DateSent = time.Now()
	}
	if err := db.UpsertSyncMessages(t.Context(), []SyncMessage{msg}); err != nil {
		t.Fatal(err)
	}
	id, err := db.GetMessageLocalIDByInternetIDInternal(t.Context(), "acc", msg.MessageID)
	if err != nil || id == 0 {
		t.Fatalf("message %s: %d %v", msg.MessageID, id, err)
	}
	return id
}

func visibleFolders(t *testing.T, db *DB, messageID int64) string {
	t.Helper()
	rows, err := db.Read().Query(`SELECT folder_id FROM message_folder_state WHERE message_id = ? AND is_deleted = 0 ORDER BY folder_id`, messageID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var folders []string
	for rows.Next() {
		var folder string
		if err := rows.Scan(&folder); err != nil {
			t.Fatal(err)
		}
		folders = append(folders, folder)
	}
	return strings.Join(folders, ",")
}

func ruleRuns(t *testing.T, db *DB) []models.MailRuleRun {
	t.Helper()
	runs, err := db.ListMailRuleRuns(t.Context(), "default", 0)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func fromBakery() []models.MailRuleCondition {
	return []models.MailRuleCondition{{Field: "from", Operator: "contains", Value: "harbor-bakery"}}
}

func TestMigrateV131ToV132AddsMailRules(t *testing.T) {
	db := newContactsTestDB(t)
	if _, err := db.Write().Exec(`DROP TABLE mail_rule_runs; DROP TABLE mail_rule_pending; DROP TABLE mail_rules; ALTER TABLE messages DROP COLUMN list_id;
		DELETE FROM schema_version; INSERT INTO schema_version (version) VALUES (131)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV131ToV132(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"mail_rules", "mail_rule_pending", "mail_rule_runs"} {
		if exists, err := tableExists(db.Read(), table); err != nil || !exists {
			t.Fatalf("%s exists = %v, %v", table, exists, err)
		}
	}
	if exists, err := columnExists(db.Read(), "messages", "list_id"); err != nil || !exists {
		t.Fatalf("messages.list_id exists = %v, %v", exists, err)
	}
}

func TestSaveListOrderAndDeleteMailRules(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	first := saveTestRule(t, db, models.MailRule{Name: "First", AccountID: "acc", MatchAll: true, Conditions: fromBakery(),
		Actions: []models.MailRuleAction{{Kind: "move", Folder: &models.MailRuleFolderTarget{FolderID: "receipts"}}}})
	if first.Actions[0].Folder.Path != "Receipts" {
		t.Fatalf("folder path not kept: %+v", first.Actions[0].Folder)
	}
	second := saveTestRule(t, db, models.MailRule{Name: "Second", Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "star"}}})
	if second.SortOrder != first.SortOrder+1 || second.AccountID != "" {
		t.Fatalf("second rule = %+v", second)
	}

	if err := db.SetMailRuleOrderGuarded(ctx, "default", []string{second.ID, first.ID}, nil); err != nil {
		t.Fatal(err)
	}
	rules, err := db.ListMailRules(ctx, "default")
	if err != nil || len(rules) != 2 || rules[0].ID != second.ID {
		t.Fatalf("rules after reorder = %+v, %v", rules, err)
	}
	if err := db.SetMailRuleOrderGuarded(ctx, "default", []string{second.ID}, nil); !errors.Is(err, ErrMailRuleOrderChanged) {
		t.Fatalf("partial order = %v", err)
	}

	if err := db.SetMailRuleEnabledGuarded(ctx, "default", first.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if rule, err := db.GetMailRule(ctx, "default", first.ID); err != nil || rule.Enabled {
		t.Fatalf("disabled rule = %+v, %v", rule, err)
	}

	edited := first
	edited.Name = "Renamed"
	edited.Enabled = true
	if saved, err := db.SaveMailRuleGuarded(ctx, "default", edited, nil); err != nil || saved.Name != "Renamed" || saved.SortOrder != 1 {
		t.Fatalf("edited rule = %+v, %v", saved, err)
	}

	if err := db.DeleteMailRuleGuarded(ctx, "default", first.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteMailRuleGuarded(ctx, "default", first.ID, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second delete = %v", err)
	}
}

func TestSaveMailRuleChecksOwnership(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	if _, err := db.Write().Exec(`INSERT INTO users (id, username, username_normalized) VALUES ('other', 'other', 'other');
		INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('other-acc', 'other', 'imap', 'sam@example.net')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "other-inbox", AccountID: "other-acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatal(err)
	}
	star := []models.MailRuleAction{{Kind: "star"}}
	if _, err := db.SaveMailRuleGuarded(ctx, "default", models.MailRule{Name: "x", AccountID: "other-acc", Conditions: fromBakery(), Actions: star}, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign account = %v", err)
	}
	move := []models.MailRuleAction{{Kind: "move", Folder: &models.MailRuleFolderTarget{FolderID: "other-inbox"}}}
	if _, err := db.SaveMailRuleGuarded(ctx, "default", models.MailRule{Name: "x", AccountID: "acc", Conditions: fromBakery(), Actions: move}, nil); !errors.Is(err, ErrMailRuleFolder) {
		t.Fatalf("foreign folder = %v", err)
	}
	mine := saveTestRule(t, db, models.MailRule{Conditions: fromBakery(), Actions: star})
	mine.Name = "Stolen"
	if _, err := db.SaveMailRuleGuarded(ctx, "other", mine, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign edit = %v", err)
	}
	if err := db.DeleteMailRuleGuarded(ctx, "other", mine.ID, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign delete = %v", err)
	}
}

func TestArrivalsAreSetAsideOnlyWithRules(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	var pending int
	count := func() int {
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM mail_rule_pending`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		return pending
	}
	arrive(t, db, "inbox", SyncMessage{MessageID: "<before@harbor-bakery.example>", FromEmail: "news@harbor-bakery.example"})
	if count() != 0 {
		t.Fatal("an arrival was set aside with no rules")
	}
	rule := saveTestRule(t, db, models.MailRule{Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "star"}}})
	backfill(t, db, "inbox", SyncMessage{MessageID: "<old@harbor-bakery.example>", FromEmail: "news@harbor-bakery.example"})
	if count() != 0 {
		t.Fatal("mail found by a backfill was set aside")
	}
	arrive(t, db, "inbox", SyncMessage{MessageID: "<new@harbor-bakery.example>", FromEmail: "news@harbor-bakery.example"})
	if count() != 1 {
		t.Fatal("an arrival was not set aside")
	}
	// Seen again in another folder, it is not new.
	arrive(t, db, "archive", SyncMessage{MessageID: "<new@harbor-bakery.example>", FromEmail: "news@harbor-bakery.example"})
	if count() != 1 {
		t.Fatal("a known message was set aside again")
	}
	if err := db.SetMailRuleEnabledGuarded(ctx, "default", rule.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	arrive(t, db, "inbox", SyncMessage{MessageID: "<off@harbor-bakery.example>", FromEmail: "news@harbor-bakery.example"})
	if count() != 1 {
		t.Fatal("an arrival was set aside with every rule off")
	}
}

func TestApplyPendingMailRulesActs(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	saveTestRule(t, db, models.MailRule{Name: "Receipts", AccountID: "acc", MatchAll: true,
		Conditions: append(fromBakery(), models.MailRuleCondition{Field: "subject", Operator: "contains", Value: "receipt"}),
		Actions: []models.MailRuleAction{
			{Kind: "move", Folder: &models.MailRuleFolderTarget{FolderID: "receipts"}},
			{Kind: "mark_read"},
			{Kind: "label", Label: "Bread"},
		}})
	saveTestRule(t, db, models.MailRule{Name: "Star bakery", Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "star"}}})

	receipt := arrive(t, db, "inbox", SyncMessage{MessageID: "<r1@harbor-bakery.example>", FromName: "Harbor Bakery", FromEmail: "news@harbor-bakery.example", Subject: "Your receipt"})
	news := arrive(t, db, "inbox", SyncMessage{MessageID: "<n1@harbor-bakery.example>", FromEmail: "news@harbor-bakery.example", Subject: "Rye is back"})
	other := arrive(t, db, "inbox", SyncMessage{MessageID: "<x1@example.net>", FromEmail: "sam@example.net", Subject: "Your receipt"})

	outcome, err := db.ApplyPendingMailRules(ctx, "acc")
	if err != nil {
		t.Fatal(err)
	}
	if got := visibleFolders(t, db, receipt); got != "receipts" {
		t.Fatalf("receipt is in %q", got)
	}
	if got := visibleFolders(t, db, news); got != "inbox" {
		t.Fatalf("news is in %q", got)
	}
	if got := visibleFolders(t, db, other); got != "inbox" {
		t.Fatalf("unmatched mail is in %q", got)
	}
	if !outcome.Moved["r1@harbor-bakery.example"] || !outcome.Read["r1@harbor-bakery.example"] || outcome.Moved["n1@harbor-bakery.example"] {
		t.Fatalf("outcome = %+v", outcome)
	}

	var read, starred bool
	if err := db.Read().QueryRow(`SELECT is_read, is_starred FROM message_folder_state WHERE message_id = ? AND folder_id = 'receipts'`, receipt).Scan(&read, &starred); err != nil {
		t.Fatal(err)
	}
	if !read || !starred {
		t.Fatalf("receipt read=%v starred=%v", read, starred)
	}
	var queued int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE message_id = ? AND kind = 'move'`, receipt).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("queued moves = %d, %v", queued, err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM label_mutation_queue WHERE message_id = ? AND label_name = 'Bread'`, receipt).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("queued labels = %d, %v", queued, err)
	}

	runs := ruleRuns(t, db)
	if len(runs) != 3 {
		t.Fatalf("runs = %+v", runs)
	}
	for _, run := range runs {
		if run.Status != models.MailRuleRunApplied || run.Trigger != models.MailRuleTriggerArrival {
			t.Fatalf("run = %+v", run)
		}
	}
	if !strings.Contains(runs[2].Detail, "moved to “Receipts”") || !strings.Contains(runs[2].Detail, "marked as read") {
		t.Fatalf("receipt detail = %q", runs[2].Detail)
	}

	// Each arrival is looked at once.
	if _, err := db.ApplyPendingMailRules(ctx, "acc"); err != nil {
		t.Fatal(err)
	}
	if len(ruleRuns(t, db)) != 3 {
		t.Fatal("rules ran twice on the same arrivals")
	}
}

func TestMailRulesStopAndMoveOnce(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	saveTestRule(t, db, models.MailRule{Name: "Archive", Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "archive"}}})
	saveTestRule(t, db, models.MailRule{Name: "Trash", Conditions: fromBakery(), StopProcessing: true, Actions: []models.MailRuleAction{{Kind: "trash"}}})
	saveTestRule(t, db, models.MailRule{Name: "Never", Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "star"}}})

	id := arrive(t, db, "inbox", SyncMessage{MessageID: "<a@harbor-bakery.example>", FromEmail: "news@harbor-bakery.example"})
	if _, err := db.ApplyPendingMailRules(ctx, "acc"); err != nil {
		t.Fatal(err)
	}
	if got := visibleFolders(t, db, id); got != "archive" {
		t.Fatalf("message is in %q", got)
	}
	runs := ruleRuns(t, db)
	if len(runs) != 2 || runs[0].RuleName != "Trash" || runs[0].Status != models.MailRuleRunSkipped || !strings.Contains(runs[0].Detail, "already moved by") {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestMailRulesLeaveOwnMailAndMissingFolders(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	saveTestRule(t, db, models.MailRule{Name: "Gone", AccountID: "acc", Conditions: fromBakery(),
		Actions: []models.MailRuleAction{{Kind: "move", Folder: &models.MailRuleFolderTarget{FolderID: "receipts"}}}})
	if _, err := db.Write().Exec(`DELETE FROM folders WHERE id = 'receipts'`); err != nil {
		t.Fatal(err)
	}
	sent := arrive(t, db, "sent", SyncMessage{MessageID: "<s@example.org>", FromEmail: "robin@example.org", ToRecipients: []Recipient{{Email: "news@harbor-bakery.example"}}})
	missing := arrive(t, db, "inbox", SyncMessage{MessageID: "<m@harbor-bakery.example>", FromEmail: "news@harbor-bakery.example"})
	if _, err := db.ApplyPendingMailRules(ctx, "acc"); err != nil {
		t.Fatal(err)
	}
	if visibleFolders(t, db, sent) != "sent" || visibleFolders(t, db, missing) != "inbox" {
		t.Fatal("rules moved mail they should have left")
	}
	runs := ruleRuns(t, db)
	if len(runs) != 1 || runs[0].MessageID != missing || runs[0].Status != models.MailRuleRunSkipped || !strings.Contains(runs[0].Detail, "“Receipts” is gone") {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestMailRuleFolderFoundByPathAfterRename(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	saveTestRule(t, db, models.MailRule{Name: "Receipts", AccountID: "acc", Conditions: fromBakery(),
		Actions: []models.MailRuleAction{{Kind: "move", Folder: &models.MailRuleFolderTarget{FolderID: "receipts"}}}})
	// Renamed elsewhere and back, an IMAP folder comes back with a new ID.
	if _, err := db.Write().Exec(`DELETE FROM folders WHERE id = 'receipts'`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "receipts-2", AccountID: "acc", RemoteID: "Receipts", Name: "Receipts", Role: "custom", Selectable: true}}); err != nil {
		t.Fatal(err)
	}
	id := arrive(t, db, "inbox", SyncMessage{MessageID: "<p@harbor-bakery.example>", FromEmail: "news@harbor-bakery.example"})
	if _, err := db.ApplyPendingMailRules(ctx, "acc"); err != nil {
		t.Fatal(err)
	}
	if got := visibleFolders(t, db, id); got != "receipts-2" {
		t.Fatalf("message is in %q", got)
	}
}

func TestMailRuleSpamOnIMAPQueuesTraining(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	saveTestRule(t, db, models.MailRule{Name: "Spam", Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "spam"}}})
	id := arrive(t, db, "inbox", SyncMessage{MessageID: "<j@harbor-bakery.example>", FromEmail: "news@harbor-bakery.example"})
	if _, err := db.ApplyPendingMailRules(ctx, "acc"); err != nil {
		t.Fatal(err)
	}
	if got := visibleFolders(t, db, id); got != "junk" {
		t.Fatalf("message is in %q", got)
	}
	var junk int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM label_mutation_queue WHERE message_id = ? AND label_name = '$Junk' AND operation = ?`, id, LabelMutationAdd).Scan(&junk); err != nil || junk != 1 {
		t.Fatalf("queued $Junk = %d, %v", junk, err)
	}
}

func TestProviderArrivalsGetRulesAndListID(t *testing.T) {
	db := newMailRulesTestDB(t, "gmail")
	ctx := context.Background()
	saveTestRule(t, db, models.MailRule{Name: "List", Conditions: []models.MailRuleCondition{{Field: "list_id", Operator: "contains", Value: "news.harbor-bakery.example"}},
		Actions: []models.MailRuleAction{{Kind: "archive"}}})
	ids, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{
		{AccountID: "acc", FolderID: "inbox", ProviderMessageID: "g1", InternetMessageID: "<g1@harbor-bakery.example>", FromEmail: "news@harbor-bakery.example",
			ListID: "Harbor news <news.harbor-bakery.example>", DateSent: time.Now(), Arrived: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplyPendingMailRules(ctx, "acc"); err != nil {
		t.Fatal(err)
	}
	if got := visibleFolders(t, db, ids["g1"]); got != "archive" {
		t.Fatalf("message is in %q", got)
	}
	var listID string
	if err := db.Read().QueryRow(`SELECT list_id FROM messages WHERE id = ?`, ids["g1"]).Scan(&listID); err != nil || listID != "Harbor news <news.harbor-bakery.example>" {
		t.Fatalf("list_id = %q, %v", listID, err)
	}
}

func TestMailRuleRunsArePruned(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	for range mailRuleRunsKept + 5 {
		if err := recordMailRuleRun(ctx, db.Write(), "default", models.MailRuleRun{AccountID: "acc", Trigger: "arrival", Status: "applied"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneMailRuleRuns(ctx, db.Write(), "default"); err != nil {
		t.Fatal(err)
	}
	if runs := ruleRuns(t, db); len(runs) != mailRuleRunsKept {
		t.Fatalf("kept %d runs", len(runs))
	}
}
