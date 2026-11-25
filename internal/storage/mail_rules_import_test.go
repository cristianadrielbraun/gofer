package storage

import (
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mailrules"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// missingReceipts is a folder an import didn't find: Receipts of the
// owner's account, which has a folder of that name only by chance.
func missingReceipts() *models.MailRuleFolderTarget {
	return &models.MailRuleFolderTarget{Path: "Receipts", AccountID: "acc", AccountName: "robin@example.org"}
}

func TestImportMailRules(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	first := saveTestRule(t, db, models.MailRule{Name: "Star bakery", Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "star"}}})

	imported, err := db.ImportMailRulesGuarded(ctx, "default", []models.MailRule{
		{Name: "Receipts", Enabled: true, MatchAll: true, AccountsChosen: true, AccountIDs: []string{"acc"}, Tags: []string{"finance"},
			Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "move", Folder: &models.MailRuleFolderTarget{FolderID: "receipts"}}}},
		{Name: "Trips", MatchAll: true, Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "move", Folder: missingReceipts()}}},
		{Name: "For a mailbox not here", MatchAll: true, AccountsChosen: true, Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "archive"}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(imported) != 3 {
		t.Fatalf("imported %d rules", len(imported))
	}
	rules, err := db.ListMailRules(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, rule := range rules {
		names = append(names, rule.Name)
	}
	if !slices.Equal(names, []string{first.Name, "Receipts", "Trips", "For a mailbox not here"}) {
		t.Fatalf("rules in order = %v", names)
	}
	receipts, trips, none := rules[1], rules[2], rules[3]
	if !receipts.Enabled || receipts.Actions[0].Folder.Path != "Receipts" || receipts.Actions[0].Folder.AccountID != "acc" || !slices.Equal(receipts.Tags, []string{"finance"}) {
		t.Fatalf("receipts = %+v, folder %+v", receipts, receipts.Actions[0].Folder)
	}
	if folder := trips.Actions[0].Folder; !folder.Missing() || folder.Path != "Receipts" || folder.AccountName != "robin@example.org" || !trips.MissingFolders() {
		t.Fatalf("missing folder = %+v", folder)
	}
	if !none.AccountsChosen || len(none.AccountIDs) != 0 {
		t.Fatalf("rule for no account = %+v", none)
	}
}

// A rule refused, or one rule too many, adds none of them.
func TestImportMailRulesAllOrNothing(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	good := models.MailRule{Name: "Archive bakery", MatchAll: true, Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "archive"}}}
	for _, check := range []struct {
		name  string
		rules []models.MailRule
		want  error
	}{
		{"a folder of another owner", []models.MailRule{good, {Name: "Elsewhere", MatchAll: true, Conditions: fromBakery(),
			Actions: []models.MailRuleAction{{Kind: "move", Folder: &models.MailRuleFolderTarget{FolderID: "not-mine"}}}}}, ErrMailRuleFolder},
		{"a missing folder in an account not the owner's", []models.MailRule{good, {Name: "Elsewhere", MatchAll: true, Conditions: fromBakery(),
			Actions: []models.MailRuleAction{{Kind: "move", Folder: &models.MailRuleFolderTarget{Path: "Receipts", AccountID: "not-mine"}}}}}, sql.ErrNoRows},
		{"a missing folder with no name", []models.MailRule{good, {Name: "Nowhere", MatchAll: true, Conditions: fromBakery(),
			Actions: []models.MailRuleAction{{Kind: "move", Folder: &models.MailRuleFolderTarget{AccountID: "acc"}}}}}, nil},
	} {
		if _, err := db.ImportMailRulesGuarded(ctx, "default", check.rules, nil); err == nil || (check.want != nil && !errors.Is(err, check.want)) {
			t.Errorf("%s: err = %v, want %v", check.name, err, check.want)
		}
	}
	many := make([]models.MailRule, mailrules.MaxRulesPerOwner+1)
	for i := range many {
		many[i] = good
	}
	if _, err := db.ImportMailRulesGuarded(ctx, "default", many, nil); !errors.Is(err, ErrTooManyMailRules) {
		t.Fatalf("too many rules: err = %v", err)
	}
	if rules, err := db.ListMailRules(ctx, "default"); err != nil || len(rules) != 0 {
		t.Fatalf("rules after refused imports = %d, %v", len(rules), err)
	}
}

// A rule's form never sends a folder by name alone; only an import keeps
// one it didn't find.
func TestSaveMailRuleRefusesMissingFolder(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	_, err := db.SaveMailRuleGuarded(t.Context(), "default", models.MailRule{Name: "Trips", MatchAll: true, Conditions: fromBakery(),
		Actions: []models.MailRuleAction{{Kind: "move", Folder: missingReceipts()}}}, nil)
	if err == nil {
		t.Fatal("saved a rule with a folder that wasn't found")
	}
}

// An imported rule whose folder wasn't found does nothing, even turned on,
// and isn't taken to move mail to a folder of the same name: that may not
// be the folder it was written for.
func TestImportedRuleWithMissingFolderDoesNotRun(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	imported, err := db.ImportMailRulesGuarded(ctx, "default", []models.MailRule{
		{Name: "Trips", MatchAll: true, Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "move", Folder: missingReceipts()}}},
		{Name: "Not in Receipts", MatchAll: true, Conditions: []models.MailRuleCondition{
			{Field: "folder", Operator: "is_not", Folder: missingReceipts()},
		}, Actions: []models.MailRuleAction{{Kind: "star"}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range imported {
		if err := db.SetMailRuleEnabledGuarded(ctx, "default", rule.ID, true, nil); !errors.Is(err, ErrMailRuleMissingFolder) {
			t.Fatalf("turned on a rule with a missing folder: %v", err)
		}
	}
	if changed, err := db.SetMailRulesEnabledGuarded(ctx, "default", []string{imported[0].ID, imported[1].ID}, true, nil); err != nil || changed != 0 {
		t.Fatalf("turned on %d selected rules with missing folders: %v", changed, err)
	}
	// Turned on some other way, as by an older Gofer, they still do
	// nothing.
	if _, err := db.Write().Exec(`UPDATE mail_rules SET enabled = 1`); err != nil {
		t.Fatal(err)
	}
	rules, err := db.ListMailRules(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	destinations, err := db.MailRuleDestinations(ctx, "default", rules)
	if err != nil {
		t.Fatal(err)
	}
	if got := destinations[imported[0].ID]; len(got) != 0 {
		t.Fatalf("the missing folder is taken to be %v", got)
	}
	id := arrive(t, db, "inbox", SyncMessage{MessageID: "<q@harbor-bakery.example>", FromEmail: "news@harbor-bakery.example"})
	if _, err := db.ApplyPendingMailRules(ctx, "acc"); err != nil {
		t.Fatal(err)
	}
	if got := visibleFolders(t, db, id); got != "inbox" {
		t.Fatalf("message is in %q", got)
	}
	var starred bool
	if err := db.Read().QueryRow(`SELECT is_starred FROM message_folder_state WHERE message_id = ?`, id).Scan(&starred); err != nil || starred {
		t.Fatalf("starred = %v, %v", starred, err)
	}
	if runs := ruleRuns(t, db); len(runs) != 0 {
		t.Fatalf("runs = %+v", runs)
	}
}

// A rule imported for an account whose folders haven't synced yet, as a
// profile import adds it, gets its folders when they come: only a folder at
// the very path the rule names, in its account. Once it misses no folder, it
// turns on if it was on where it was exported.
func TestImportedRulesFindTheirFoldersWhenTheAccountSyncs(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	waiting := func(path string, turnOn bool) *models.MailRuleFolderTarget {
		return &models.MailRuleFolderTarget{Path: path, AccountID: "acc", AccountName: "robin@example.org", TurnOnWhenFound: turnOn}
	}
	move := func(target *models.MailRuleFolderTarget) []models.MailRuleAction {
		return []models.MailRuleAction{{Kind: "move", Folder: target}}
	}
	imported, err := db.ImportMailRulesGuarded(ctx, "default", []models.MailRule{
		{Name: "Trips", MatchAll: true, Conditions: fromBakery(), Actions: move(waiting("Trips", true))},
		{Name: "Trips, off where exported", MatchAll: true, Conditions: fromBakery(), Actions: move(waiting("Trips", false))},
		{Name: "Trips, not from Travel", MatchAll: true, Conditions: append(fromBakery(),
			models.MailRuleCondition{Field: "folder", Operator: "is_not", Folder: waiting("Travel", true)}), Actions: move(waiting("Trips", true))},
		{Name: "Trips in lower case", MatchAll: true, Conditions: fromBakery(), Actions: move(waiting("trips", true))},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "trips", AccountID: "acc", RemoteID: "Trips", Name: "Trips", Role: "custom", Selectable: true}}); err != nil {
		t.Fatal(err)
	}
	rule := func(i int) models.MailRule {
		t.Helper()
		got, err := db.GetMailRule(ctx, "default", imported[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	found := models.MailRuleFolderTarget{FolderID: "trips", Path: "Trips", AccountID: "acc"}
	if got := rule(0); !got.Enabled || *got.Actions[0].Folder != found {
		t.Fatalf("rule waiting for Trips: on %v, folder %+v", got.Enabled, got.Actions[0].Folder)
	}
	if got := rule(1); got.Enabled || *got.Actions[0].Folder != found {
		t.Fatalf("rule off where exported: on %v, folder %+v", got.Enabled, got.Actions[0].Folder)
	}
	if got := rule(2); got.Enabled || *got.Actions[0].Folder != found || !got.MissingFolders() || !got.Conditions[1].Folder.TurnOnWhenFound {
		t.Fatalf("rule still missing Travel: on %v, %+v", got.Enabled, got)
	}
	if got := rule(3); got.Enabled || !got.MissingFolders() {
		t.Fatalf("a folder of another name was taken for %q: %+v", "trips", got.Actions[0].Folder)
	}
	id := arrive(t, db, "inbox", SyncMessage{MessageID: "<trip@harbor-bakery.example>", FromEmail: "news@harbor-bakery.example"})
	if _, err := db.ApplyPendingMailRules(ctx, "acc"); err != nil {
		t.Fatal(err)
	}
	if got := visibleFolders(t, db, id); got != "trips" {
		t.Fatalf("message is in %q", got)
	}
}
