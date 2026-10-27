package storage

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func previewSubjects(preview MailRulePreview) []string {
	subjects := []string{}
	for _, m := range preview.Sample {
		subjects = append(subjects, m.Subject)
	}
	return subjects
}

func inboxScope() MailRuleRunScope {
	return MailRuleRunScope{Folder: models.MailRuleFolderTarget{Role: "inbox"}}
}

// A rule runs on the mail already in a folder only after a preview, and acts
// on what the preview found; a second run has nothing left to do.
func TestPreviewAndRunMailRuleOnExistingMail(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	rule := saveTestRule(t, db, models.MailRule{Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "archive"}}})
	// Running by hand works on a rule that is off too.
	if err := db.SetMailRuleEnabledGuarded(ctx, "default", rule.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	older := backfill(t, db, "inbox", SyncMessage{MessageID: "<rolls@harbor-bakery.example>", Subject: "Rolls are back", FromEmail: "news@harbor-bakery.example", DateSent: day})
	newer := backfill(t, db, "inbox", SyncMessage{MessageID: "<pies@harbor-bakery.example>", Subject: "Pie week", FromEmail: "news@harbor-bakery.example", DateSent: day.AddDate(0, 0, 3)})
	other := backfill(t, db, "inbox", SyncMessage{MessageID: "<dentist@clinic.example>", Subject: "Appointment", FromEmail: "desk@clinic.example", DateSent: day.AddDate(0, 0, 2)})
	elsewhere := backfill(t, db, "receipts", SyncMessage{MessageID: "<bill@harbor-bakery.example>", Subject: "Your bill", FromEmail: "till@harbor-bakery.example", DateSent: day})

	preview, err := db.PreviewMailRuleRun(ctx, "default", rule.ID, inboxScope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(preview.Matches, []int64{newer, older}) || preview.Scanned != 3 || preview.Truncated {
		t.Fatalf("preview = %+v", preview)
	}
	if got := previewSubjects(preview); !slices.Equal(got, []string{"Pie week", "Rolls are back"}) || preview.Sample[0].FolderID != "inbox" || !preview.Sample[0].Date.Equal(day.AddDate(0, 0, 3)) {
		t.Fatalf("sample = %+v", preview.Sample)
	}
	if visibleFolders(t, db, newer) != "inbox" || len(ruleRuns(t, db)) != 0 {
		t.Fatal("the preview changed something")
	}

	result, err := db.RunMailRuleNow(ctx, "default", rule.ID, inboxScope(), preview.Matches, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed != 2 || result.Unchanged != 0 {
		t.Fatalf("result = %+v", result)
	}
	if !slices.Contains(result.Folders, FolderRef{AccountID: "acc", FolderID: "inbox"}) || !slices.Contains(result.Folders, FolderRef{AccountID: "acc", FolderID: "archive"}) {
		t.Fatalf("changed folders = %+v", result.Folders)
	}
	for id, want := range map[int64]string{older: "archive", newer: "archive", other: "inbox", elsewhere: "receipts"} {
		if got := visibleFolders(t, db, id); got != want {
			t.Fatalf("message %d in %q, want %q", id, got, want)
		}
	}
	runs := ruleRuns(t, db)
	if len(runs) != 2 || runs[0].Trigger != models.MailRuleTriggerManual || runs[0].Status != models.MailRuleRunApplied || runs[0].Detail != "archived" {
		t.Fatalf("activity = %+v", runs)
	}

	again, err := db.PreviewMailRuleRun(ctx, "default", rule.ID, inboxScope(), nil)
	if err != nil || len(again.Matches) != 0 || again.Scanned != 1 {
		t.Fatalf("second preview = %+v, %v", again, err)
	}
	// Confirming a stale preview does nothing either.
	result, err = db.RunMailRuleNow(ctx, "default", rule.ID, inboxScope(), preview.Matches, nil)
	if err != nil || result.Changed != 0 || result.Unchanged != 2 || len(ruleRuns(t, db)) != 2 {
		t.Fatalf("second run = %+v, %v", result, err)
	}
}

// A preview leaves out mail the rule would leave as it is.
func TestPreviewMailRuleRunLeavesOutMailWithNothingToDo(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	rule := saveTestRule(t, db, models.MailRule{Conditions: fromBakery(),
		Actions: []models.MailRuleAction{{Kind: "mark_read"}, {Kind: "label", Label: "Bread"}}})
	read := backfill(t, db, "inbox", SyncMessage{MessageID: "<read@harbor-bakery.example>", Subject: "Read", FromEmail: "news@harbor-bakery.example", IsRead: true,
		Labels: []LabelInput{{AccountID: "acc", ProviderType: LabelProviderLocal, Name: "bread"}}, LabelsKnown: true, LabelProvider: LabelProviderLocal})
	unread := backfill(t, db, "inbox", SyncMessage{MessageID: "<unread@harbor-bakery.example>", Subject: "Unread", FromEmail: "news@harbor-bakery.example"})

	preview, err := db.PreviewMailRuleRun(ctx, "default", rule.ID, inboxScope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(preview.Matches, []int64{unread}) {
		t.Fatalf("matches = %v, want only %d (not %d)", preview.Matches, unread, read)
	}
	if _, err := db.RunMailRuleNow(ctx, "default", rule.ID, inboxScope(), preview.Matches, nil); err != nil {
		t.Fatal(err)
	}
	again, err := db.PreviewMailRuleRun(ctx, "default", rule.ID, inboxScope(), nil)
	if err != nil || len(again.Matches) != 0 {
		t.Fatalf("after labeling, preview = %+v, %v", again, err)
	}
}

// One run looks at the newest mail of a folder only, and acts on so many
// messages at most.
func TestPreviewMailRuleRunIsCapped(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	rule := saveTestRule(t, db, models.MailRule{Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "star"}}})
	day := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	var ids []int64
	for i := range 5 {
		ids = append(ids, backfill(t, db, "inbox", SyncMessage{MessageID: "<loaf-" + string(rune('a'+i)) + "@harbor-bakery.example>",
			Subject: "Loaf", FromEmail: "news@harbor-bakery.example", DateSent: day.AddDate(0, 0, i)}))
	}
	scan, match := mailRuleRunScanLimit, mailRuleRunMatchLimit
	t.Cleanup(func() { mailRuleRunScanLimit, mailRuleRunMatchLimit = scan, match })

	mailRuleRunScanLimit, mailRuleRunMatchLimit = 3, 1000
	preview, err := db.PreviewMailRuleRun(ctx, "default", rule.ID, inboxScope(), nil)
	if err != nil || !slices.Equal(preview.Matches, []int64{ids[4], ids[3], ids[2]}) || !preview.Truncated {
		t.Fatalf("scan-capped preview = %+v, %v", preview, err)
	}
	mailRuleRunScanLimit, mailRuleRunMatchLimit = 1000, 2
	preview, err = db.PreviewMailRuleRun(ctx, "default", rule.ID, inboxScope(), nil)
	if err != nil || !slices.Equal(preview.Matches, []int64{ids[4], ids[3]}) || !preview.Truncated {
		t.Fatalf("match-capped preview = %+v, %v", preview, err)
	}
	result, err := db.RunMailRuleNow(ctx, "default", rule.ID, inboxScope(), ids, nil)
	if err != nil || result.Changed != 2 {
		t.Fatalf("run of more than the cap = %+v, %v", result, err)
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

	preview, err := db.PreviewMailRuleRun(ctx, "default", everywhere.ID, inboxScope(), nil)
	if err != nil || len(preview.Matches) != 2 {
		t.Fatalf("every inbox = %+v, %v", preview, err)
	}
	onlyFirst := func(accountID string) bool { return accountID == "acc" }
	if preview, err = db.PreviewMailRuleRun(ctx, "default", everywhere.ID, inboxScope(), onlyFirst); err != nil || !slices.Equal(preview.Matches, []int64{first}) {
		t.Fatalf("allowed accounts = %+v, %v", preview, err)
	}
	byID := MailRuleRunScope{Folder: models.MailRuleFolderTarget{FolderID: "inbox2"}}
	if preview, err = db.PreviewMailRuleRun(ctx, "default", everywhere.ID, byID, nil); err != nil || !slices.Equal(preview.Matches, []int64{second}) {
		t.Fatalf("one folder = %+v, %v", preview, err)
	}
	if _, err := db.PreviewMailRuleRun(ctx, "default", everywhere.ID, MailRuleRunScope{AccountID: "acc", Folder: models.MailRuleFolderTarget{FolderID: "inbox2"}}, nil); !errors.Is(err, ErrMailRuleRunFolder) {
		t.Fatalf("folder of another account: %v", err)
	}
	firstOnly := saveTestRule(t, db, models.MailRule{Name: "First only", AccountID: "acc", Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "trash"}}})
	if _, err := db.PreviewMailRuleRun(ctx, "default", firstOnly.ID, byID, nil); !errors.Is(err, ErrMailRuleRunFolder) {
		t.Fatalf("folder outside the rule's account: %v", err)
	}
	if _, err := db.PreviewMailRuleRun(ctx, "someone-else", everywhere.ID, inboxScope(), nil); err == nil {
		t.Fatal("another user ran the rule")
	}

	// Mail that left the folder since the preview, and mail of an account
	// not in the run, are left alone.
	if err := db.MoveMessagesAndQueueForUser(ctx, []int64{first}, "inbox", "receipts", "default"); err != nil {
		t.Fatal(err)
	}
	result, err := db.RunMailRuleNow(ctx, "default", firstOnly.ID, inboxScope(), []int64{first, second}, nil)
	if err != nil || result.Changed != 0 || result.Unchanged != 2 {
		t.Fatalf("run = %+v, %v", result, err)
	}
	if visibleFolders(t, db, first) != "receipts" || visibleFolders(t, db, second) != "inbox2" {
		t.Fatal("a message outside the run was moved")
	}
}
