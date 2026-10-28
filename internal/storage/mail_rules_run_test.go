package storage

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// scanAll finds every message a rule would change, step by step, as a run
// by hand does.
func scanAll(t *testing.T, db *DB, ruleID string, scope MailRuleRunScope, allow func(string) bool) (matches []int64, total, steps int, err error) {
	return scanAllBy(t, db, ruleID, scope, allow, MailRuleRunScanBatch)
}

func scanAllBy(t *testing.T, db *DB, ruleID string, scope MailRuleRunScope, allow func(string) bool, limit int) (matches []int64, total, steps int, err error) {
	t.Helper()
	matches = []int64{}
	var before int64
	for {
		step, err := db.ScanMailRuleRun(t.Context(), "default", ruleID, scope, allow, before, limit)
		if err != nil {
			return matches, total, steps, err
		}
		if steps == 0 {
			total = step.Total
		}
		steps++
		matches = append(matches, step.Matches...)
		if step.Done {
			return matches, total, steps, nil
		}
		before = step.Next
	}
}

func changedCount(result MailRuleRunResult) int {
	n := 0
	for _, changed := range result.Changed {
		n += changed
	}
	return n
}

func inboxScope() MailRuleRunScope {
	return MailRuleRunScope{Folder: models.MailRuleFolderTarget{Role: "inbox"}}
}

// A rule runs on the mail already in a folder only after a scan, and acts
// on what the scan found; a second run has nothing left to do.
func TestScanAndRunMailRuleOnExistingMail(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	rule := saveTestRule(t, db, models.MailRule{Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "archive"}}})
	// Running by hand works on an inactive rule too.
	if err := db.SetMailRuleEnabledGuarded(ctx, "default", rule.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	older := backfill(t, db, "inbox", SyncMessage{MessageID: "<rolls@harbor-bakery.example>", Subject: "Rolls are back", FromEmail: "news@harbor-bakery.example"})
	newer := backfill(t, db, "inbox", SyncMessage{MessageID: "<pies@harbor-bakery.example>", Subject: "Pie week", FromEmail: "news@harbor-bakery.example"})
	other := backfill(t, db, "inbox", SyncMessage{MessageID: "<dentist@clinic.example>", Subject: "Appointment", FromEmail: "desk@clinic.example"})
	elsewhere := backfill(t, db, "receipts", SyncMessage{MessageID: "<bill@harbor-bakery.example>", Subject: "Your bill", FromEmail: "till@harbor-bakery.example"})

	matches, total, _, err := scanAll(t, db, rule.ID, inboxScope(), nil)
	if err != nil || !slices.Equal(matches, []int64{newer, older}) || total != 3 {
		t.Fatalf("scan = %v of %d, %v", matches, total, err)
	}
	if visibleFolders(t, db, newer) != "inbox" || len(ruleRuns(t, db)) != 0 {
		t.Fatal("the scan changed something")
	}

	result, err := db.RunMailRuleNow(ctx, "default", rule.ID, inboxScope(), matches, nil)
	if err != nil || changedCount(result) != 2 || result.Changed["acc"] != 2 || result.Unchanged != 0 || result.Failed != 0 {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if !slices.Contains(result.Folders, FolderRef{AccountID: "acc", FolderID: "inbox"}) || !slices.Contains(result.Folders, FolderRef{AccountID: "acc", FolderID: "archive"}) {
		t.Fatalf("changed folders = %+v", result.Folders)
	}
	for id, want := range map[int64]string{older: "archive", newer: "archive", other: "inbox", elsewhere: "receipts"} {
		if got := visibleFolders(t, db, id); got != want {
			t.Fatalf("message %d in %q, want %q", id, got, want)
		}
	}
	// The messages a run changed aren't recorded one by one; the run
	// records a summary.
	if runs := ruleRuns(t, db); len(runs) != 0 {
		t.Fatalf("activity = %+v", runs)
	}
	if err := db.RecordMailRuleRunSummary(ctx, "default", rule.ID, "acc", "Run by hand on Inbox: changed 2 messages", 2); err != nil {
		t.Fatal(err)
	}
	if runs := ruleRuns(t, db); len(runs) != 1 || runs[0].Trigger != models.MailRuleTriggerManual || runs[0].Status != models.MailRuleRunApplied || runs[0].MessageID != 0 {
		t.Fatalf("activity = %+v", runs)
	}

	if again, _, _, err := scanAll(t, db, rule.ID, inboxScope(), nil); err != nil || len(again) != 0 {
		t.Fatalf("second scan = %v, %v", again, err)
	}
	// Running on a stale scan does nothing either.
	result, err = db.RunMailRuleNow(ctx, "default", rule.ID, inboxScope(), matches, nil)
	if err != nil || changedCount(result) != 0 || result.Unchanged != 2 {
		t.Fatalf("second run = %+v, %v", result, err)
	}
}

// A scan leaves out mail the rule would leave as it is.
func TestScanMailRuleRunLeavesOutMailWithNothingToDo(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	rule := saveTestRule(t, db, models.MailRule{Conditions: fromBakery(),
		Actions: []models.MailRuleAction{{Kind: "mark_read"}, {Kind: "label", Label: "Bread"}}})
	read := backfill(t, db, "inbox", SyncMessage{MessageID: "<read@harbor-bakery.example>", Subject: "Read", FromEmail: "news@harbor-bakery.example", IsRead: true,
		Labels: []LabelInput{{AccountID: "acc", ProviderType: LabelProviderLocal, Name: "bread"}}, LabelsKnown: true, LabelProvider: LabelProviderLocal})
	unread := backfill(t, db, "inbox", SyncMessage{MessageID: "<unread@harbor-bakery.example>", Subject: "Unread", FromEmail: "news@harbor-bakery.example"})

	matches, _, _, err := scanAll(t, db, rule.ID, inboxScope(), nil)
	if err != nil || !slices.Equal(matches, []int64{unread}) {
		t.Fatalf("matches = %v, want only %d (not %d), %v", matches, unread, read, err)
	}
	if _, err := db.RunMailRuleNow(ctx, "default", rule.ID, inboxScope(), matches, nil); err != nil {
		t.Fatal(err)
	}
	if again, _, _, err := scanAll(t, db, rule.ID, inboxScope(), nil); err != nil || len(again) != 0 {
		t.Fatalf("after labeling, scan = %v, %v", again, err)
	}
}

// A scan goes through the whole folder a step at a time, so a rule that
// leaves mail where it is still reaches the oldest of it; a run acts on so
// many messages a step.
func TestScanMailRuleRunCoversTheWholeFolder(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	rule := saveTestRule(t, db, models.MailRule{Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "star"}}})
	var ids []int64
	for i := range 5 {
		ids = append(ids, backfill(t, db, "inbox", SyncMessage{MessageID: "<loaf-" + string(rune('a'+i)) + "@harbor-bakery.example>",
			Subject: "Loaf", FromEmail: "news@harbor-bakery.example"}))
	}
	backfill(t, db, "inbox", SyncMessage{MessageID: "<dentist@clinic.example>", Subject: "Appointment", FromEmail: "desk@clinic.example"})
	apply := MailRuleRunApplyBatch
	t.Cleanup(func() { MailRuleRunApplyBatch = apply })
	MailRuleRunApplyBatch = 2

	matches, total, steps, err := scanAllBy(t, db, rule.ID, inboxScope(), nil, 2)
	if err != nil || total != 6 || steps != 4 || !slices.Equal(matches, []int64{ids[4], ids[3], ids[2], ids[1], ids[0]}) {
		t.Fatalf("scan = %v of %d in %d steps, %v", matches, total, steps, err)
	}
	result, err := db.RunMailRuleNow(ctx, "default", rule.ID, inboxScope(), matches, nil)
	if err != nil || changedCount(result) != 2 {
		t.Fatalf("run of more than a step = %+v, %v", result, err)
	}
	if rest, _, _, err := scanAllBy(t, db, rule.ID, inboxScope(), nil, 2); err != nil || !slices.Equal(rest, []int64{ids[2], ids[1], ids[0]}) {
		t.Fatalf("left after one step = %v, %v", rest, err)
	}
}

// A run covers the accounts the rule applies to and allow lets through, and
// mail still in the folder it was run on.
func TestRunMailRuleNowScope(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc2', 'default', 'imap', 'robin@example.net')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "inbox2", AccountID: "acc2", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "trash2", AccountID: "acc2", RemoteID: "Trash", Name: "Trash", Role: "trash", Selectable: true},
	}); err != nil {
		t.Fatal(err)
	}
	everywhere := saveTestRule(t, db, models.MailRule{Name: "Everywhere", Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "trash"}}})
	first := backfill(t, db, "inbox", SyncMessage{MessageID: "<one@harbor-bakery.example>", Subject: "One", FromEmail: "news@harbor-bakery.example"})
	mailRuleTestUID++
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{AccountID: "acc2", FolderID: "inbox2", RemoteUID: mailRuleTestUID,
		MessageID: "<two@harbor-bakery.example>", Subject: "Two", FromEmail: "news@harbor-bakery.example", DateSent: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	second, err := db.GetMessageLocalIDByInternetIDInternal(ctx, "acc2", "<two@harbor-bakery.example>")
	if err != nil {
		t.Fatal(err)
	}

	matches, _, _, err := scanAll(t, db, everywhere.ID, inboxScope(), nil)
	if err != nil || len(matches) != 2 {
		t.Fatalf("every inbox = %v, %v", matches, err)
	}
	onlyFirst := func(accountID string) bool { return accountID == "acc" }
	if matches, _, _, err = scanAll(t, db, everywhere.ID, inboxScope(), onlyFirst); err != nil || !slices.Equal(matches, []int64{first}) {
		t.Fatalf("allowed accounts = %v, %v", matches, err)
	}
	byID := MailRuleRunScope{Folder: models.MailRuleFolderTarget{FolderID: "inbox2"}}
	if matches, _, _, err = scanAll(t, db, everywhere.ID, byID, nil); err != nil || !slices.Equal(matches, []int64{second}) {
		t.Fatalf("one folder = %v, %v", matches, err)
	}
	if _, err := db.ScanMailRuleRun(ctx, "default", everywhere.ID, MailRuleRunScope{AccountID: "acc", Folder: models.MailRuleFolderTarget{FolderID: "inbox2"}}, nil, 0, MailRuleRunScanBatch); !errors.Is(err, ErrMailRuleRunFolder) {
		t.Fatalf("folder of another account: %v", err)
	}
	firstOnly := saveTestRule(t, db, models.MailRule{Name: "First only", AccountID: "acc", Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "trash"}}})
	if _, err := db.ScanMailRuleRun(ctx, "default", firstOnly.ID, byID, nil, 0, MailRuleRunScanBatch); !errors.Is(err, ErrMailRuleRunFolder) {
		t.Fatalf("folder outside the rule's account: %v", err)
	}
	if _, err := db.ScanMailRuleRun(ctx, "someone-else", everywhere.ID, inboxScope(), nil, 0, MailRuleRunScanBatch); err == nil {
		t.Fatal("another user ran the rule")
	}

	// Mail that left the folder since the preview, and mail of an account
	// not in the run, are left alone.
	if err := db.MoveMessagesAndQueueForUser(ctx, []int64{first}, "inbox", "receipts", "default"); err != nil {
		t.Fatal(err)
	}
	result, err := db.RunMailRuleNow(ctx, "default", firstOnly.ID, inboxScope(), []int64{first, second}, nil)
	if err != nil || changedCount(result) != 0 || result.Unchanged != 2 {
		t.Fatalf("run = %+v, %v", result, err)
	}
	if visibleFolders(t, db, first) != "receipts" || visibleFolders(t, db, second) != "inbox2" {
		t.Fatal("a message outside the run was moved")
	}
}
