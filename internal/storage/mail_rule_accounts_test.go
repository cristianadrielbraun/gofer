package storage

import (
	"context"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/mailrules"
	"slices"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// addSecondMailRulesAccount adds another of the owner's accounts, with an
// Inbox and a custom folder.
func addSecondMailRulesAccount(t *testing.T, db *DB) {
	t.Helper()
	if _, err := db.Write().Exec(`INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc2', 'default', 'imap', 'sky@example.org')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(t.Context(), []UpsertFolderInput{
		{ID: "inbox2", AccountID: "acc2", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "keep2", AccountID: "acc2", RemoteID: "Keep", Name: "Keep", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatal(err)
	}
}

// Upgrading keeps a rule's one account as its chosen account, and deleting
// that account afterwards leaves the rule, now watching no account.
func TestMigrateV134ToV135KeepsRuleAccounts(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	if _, err := db.Write().Exec(`DROP TABLE mail_rule_transfers; DROP TABLE mail_rule_accounts; ALTER TABLE mail_rules DROP COLUMN accounts_chosen;
		DELETE FROM schema_version; INSERT INTO schema_version (version) VALUES (134);
		INSERT INTO mail_rules (id, user_id, account_id, name, conditions_json, actions_json)
		VALUES ('one', 'default', 'acc', 'One account', '[{"field":"from","operator":"contains","value":"bakery"}]', '[{"kind":"star"}]'),
		       ('every', 'default', NULL, 'Every account', '[{"field":"from","operator":"contains","value":"bakery"}]', '[{"kind":"star"}]')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV134ToV135(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	one, err := db.GetMailRule(t.Context(), "default", "one")
	if err != nil || !one.AccountsChosen || !slices.Equal(one.AccountIDs, []string{"acc"}) {
		t.Fatalf("one-account rule = %+v, %v", one, err)
	}
	every, err := db.GetMailRule(t.Context(), "default", "every")
	if err != nil || every.AccountsChosen || len(every.AccountIDs) != 0 {
		t.Fatalf("every-account rule = %+v, %v", every, err)
	}
	if _, err := db.Write().Exec(`DELETE FROM accounts WHERE id = 'acc'`); err != nil {
		t.Fatal(err)
	}
	one, err = db.GetMailRule(t.Context(), "default", "one")
	if err != nil || !one.AccountsChosen || len(one.AccountIDs) != 0 {
		t.Fatalf("rule after its account went = %+v, %v", one, err)
	}
}

// A rule for chosen accounts runs only on theirs, and a folder condition
// must name a folder of one of them.
func TestMailRuleForChosenAccounts(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	addSecondMailRulesAccount(t, db)
	ctx := t.Context()
	rule := saveTestRule(t, db, models.MailRule{AccountsChosen: true, AccountIDs: []string{"acc2", "acc", "acc"}, Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "star"}}})
	if !slices.Equal(rule.AccountIDs, []string{"acc", "acc2"}) {
		t.Fatalf("accounts = %v", rule.AccountIDs)
	}
	rule.AccountIDs = []string{"acc"}
	if _, err := db.SaveMailRuleGuarded(ctx, "default", rule, nil); err != nil {
		t.Fatal(err)
	}
	if runner, err := db.newMailRuleRunner(ctx, "acc2", nil); err != nil || runner != nil {
		t.Fatalf("runner for an account the rule isn't for = %v, %v", runner, err)
	}
	if runner, err := db.newMailRuleRunner(ctx, "acc", nil); err != nil || runner == nil {
		t.Fatalf("runner for the rule's account = %v, %v", runner, err)
	}
	rule.Conditions = []models.MailRuleCondition{{Field: "folder", Operator: "is", Folder: &models.MailRuleFolderTarget{FolderID: "keep2"}}}
	if _, err := db.SaveMailRuleGuarded(ctx, "default", rule, nil); !errors.Is(err, ErrMailRuleFolder) {
		t.Fatalf("folder of an account the rule isn't for = %v", err)
	}
}

// A rule moving mail to another account's folder leaves the message where it
// is and queues the move, which a mail transfer then makes.
func TestMailRuleQueuesMoveToAnotherAccount(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	addSecondMailRulesAccount(t, db)
	ctx := t.Context()
	var announced []string
	MailRuleTransfersQueued = func(owner string) { announced = append(announced, owner) }
	t.Cleanup(func() { MailRuleTransfersQueued = nil })

	saved := saveTestRule(t, db, models.MailRule{Name: "Keep elsewhere", Conditions: fromBakery(),
		Actions: []models.MailRuleAction{{Kind: "move", Folder: &models.MailRuleFolderTarget{FolderID: "keep2"}, ForGood: true}}})
	if target := saved.Actions[0].Folder; target.AccountID != "acc2" || target.Path != "Keep" {
		t.Fatalf("saved target = %+v", target)
	}
	id := arrive(t, db, "inbox", SyncMessage{MessageID: "<rolls@example.org>", Subject: "Fresh rolls", FromEmail: "orders@harbor-bakery.example"})
	if _, err := db.ApplyPendingMailRules(ctx, "acc"); err != nil {
		t.Fatal(err)
	}
	if folders := visibleFolders(t, db, id); folders != "inbox" {
		t.Fatalf("message moved before its transfer: %q", folders)
	}
	queued, err := db.ListMailRuleTransfers(ctx, "default", 10)
	if err != nil || len(queued) != 1 {
		t.Fatalf("queued = %+v, %v", queued, err)
	}
	if q := queued[0]; q.MessageID != id || q.SourceAccountID != "acc" || q.SourceFolderID != "inbox" || q.TargetFolderID != "keep2" || !q.ForGood {
		t.Fatalf("queued move = %+v", q)
	}
	if !slices.Equal(announced, []string{"default"}) {
		t.Fatalf("announced = %v", announced)
	}
	runs := ruleRuns(t, db)
	if len(runs) != 1 || mailrules.DetailText(context.Background(), runs[0].Detail) != "moving to “Keep” in sky@example.org" {
		t.Fatalf("activity = %+v", runs)
	}
	if err := db.DeleteMailRuleTransfers(ctx, "default", []int64{queued[0].ID}); err != nil {
		t.Fatal(err)
	}
	if owners, err := db.MailRuleTransferOwners(ctx); err != nil || len(owners) != 0 {
		t.Fatalf("owners after = %v, %v", owners, err)
	}
}

// The rules list finds the folders a rule moves mail into as the rule finds
// them when it runs: a folder every account has in each account the rule
// is for, a folder by its saved name once its ID changed, and another
// account's folder. Rules turned off count; Local folders and rules that
// move no mail add nothing.
func TestMailRuleDestinations(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	addSecondMailRulesAccount(t, db)
	ctx := t.Context()
	if _, err := db.Write().Exec(`INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('local', 'default', 'local', '')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "archive2", AccountID: "acc2", RemoteID: "Archive", Name: "Archive", Role: "archive", Selectable: true},
		{ID: "local-trash", AccountID: "local", RemoteID: "Trash", Name: "Trash", Role: "trash", Selectable: true},
		{ID: "local-trips", AccountID: "local", RemoteID: "Trips", Name: "Trips", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatal(err)
	}
	move := func(folderID string) models.MailRuleAction {
		return models.MailRuleAction{Kind: "move", Folder: &models.MailRuleFolderTarget{FolderID: folderID}}
	}
	archive := saveTestRule(t, db, models.MailRule{Name: "Old newsletters", Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "archive"}}})
	receipts := saveTestRule(t, db, models.MailRule{Name: "Receipts", AccountsChosen: true, AccountIDs: []string{"acc"}, Conditions: fromBakery(),
		Actions: []models.MailRuleAction{{Kind: "star"}, move("receipts")}})
	elsewhere := saveTestRule(t, db, models.MailRule{Name: "Keep elsewhere", AccountsChosen: true, AccountIDs: []string{"acc"}, Conditions: fromBakery(), Actions: []models.MailRuleAction{move("keep2")}})
	trips := saveTestRule(t, db, models.MailRule{Name: "Trips", Conditions: fromBakery(), Actions: []models.MailRuleAction{move("local-trips")}})
	spam := saveTestRule(t, db, models.MailRule{Name: "Spam on sky", AccountsChosen: true, AccountIDs: []string{"acc2"}, Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "spam"}}})
	star := saveTestRule(t, db, models.MailRule{Name: "Star", Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "star"}}})
	off := saveTestRule(t, db, models.MailRule{Name: "Lunch on Friday?", Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "trash"}}})
	if err := db.SetMailRuleEnabledGuarded(ctx, "default", off.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	// Renamed elsewhere and back, an IMAP folder comes back with a new ID.
	if _, err := db.Write().Exec(`DELETE FROM folders WHERE id = 'receipts'`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "receipts-2", AccountID: "acc", RemoteID: "Receipts", Name: "Receipts", Role: "custom", Selectable: true}}); err != nil {
		t.Fatal(err)
	}

	rules, err := db.ListMailRules(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.MailRuleDestinations(ctx, "default", rules)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		name string
		id   string
		want []string
	}{
		{"Archive, in each account", archive.ID, []string{"archive", "archive2"}},
		{"a folder found by its name", receipts.ID, []string{"receipts-2"}},
		{"another account's folder", elsewhere.ID, []string{"keep2"}},
		{"a Local folder, from every account", trips.ID, []string{"local-trips"}},
		{"Spam where there is none", spam.ID, nil},
		{"no move", star.ID, nil},
		{"a rule turned off, not in Local folders", off.ID, []string{"trash"}},
	} {
		if !slices.Equal(got[check.id], check.want) {
			t.Errorf("%s: folders = %v, want %v", check.name, got[check.id], check.want)
		}
	}
	if none, err := db.MailRuleDestinations(ctx, "default", nil); err != nil || len(none) != 0 {
		t.Fatalf("no rules = %v, %v", none, err)
	}
}
