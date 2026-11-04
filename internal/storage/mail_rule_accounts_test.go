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
