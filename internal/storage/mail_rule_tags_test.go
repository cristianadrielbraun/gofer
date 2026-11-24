package storage

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// Upgrading adds the table of rules' tags, and the rules have none.
func TestMigrateV152ToV153AddsRuleTags(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	saved := saveTestRule(t, db, models.MailRule{Name: "Receipts", Conditions: fromBakery(), Actions: []models.MailRuleAction{{Kind: "star"}}})
	if _, err := db.Write().Exec(`DROP TABLE mail_rule_tags`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`DELETE FROM schema_version`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO schema_version (version) VALUES (152)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV152ToV153(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	if exists, err := tableExists(db.Read(), "mail_rule_tags"); err != nil || !exists {
		t.Fatalf("mail_rule_tags exists = %v, %v", exists, err)
	}
	if n := countRows(t, db, `SELECT MAX(version) FROM schema_version`); n != 153 {
		t.Fatalf("schema version = %d, want 153", n)
	}
	rule, err := db.GetMailRule(t.Context(), "default", saved.ID)
	if err != nil || len(rule.Tags) != 0 {
		t.Fatalf("rule after the upgrade = %+v, %v", rule, err)
	}
}

// A database from before rules gets no table of their tags, and is upgraded.
func TestMigrateV152ToV153WithoutRules(t *testing.T) {
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_version (version) VALUES (152)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV152ToV153(raw); err != nil {
			t.Fatal(err)
		}
	}
	var version int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 153 {
		t.Fatalf("version = %d, %v", version, err)
	}
	if exists, err := tableExists(raw, "mail_rule_tags"); err != nil || exists {
		t.Fatalf("mail_rule_tags exists = %v, %v", exists, err)
	}
}

// A rule's tags are kept cleaned and in alphabetical order, each spelled as
// the owner's other rules spell it, and go with the rule.
func TestMailRuleTags(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	star := []models.MailRuleAction{{Kind: "star"}}
	receipts := saveTestRule(t, db, models.MailRule{Name: "Receipts", Conditions: fromBakery(), Actions: star,
		Tags: []string{"  #Finance ", "bills", "finance", "", "Lunch   on Friday"}})
	if want := []string{"bills", "Finance", "Lunch on Friday"}; !slices.Equal(receipts.Tags, want) {
		t.Fatalf("saved tags = %q, want %q", receipts.Tags, want)
	}
	// Another rule's "FINANCE" is the tag the owner already has.
	bank := saveTestRule(t, db, models.MailRule{Name: "Bank", Conditions: fromBakery(), Actions: star, Tags: []string{"FINANCE"}})
	if !slices.Equal(bank.Tags, []string{"Finance"}) {
		t.Fatalf("second rule's tags = %q", bank.Tags)
	}
	rules, err := db.ListMailRules(ctx, "default")
	if err != nil || len(rules) != 2 || !slices.Equal(rules[0].Tags, receipts.Tags) || !slices.Equal(rules[1].Tags, bank.Tags) {
		t.Fatalf("listed rules = %+v, %v", rules, err)
	}
	if tags, err := db.ListMailRuleTags(ctx, "default"); err != nil || !slices.Equal(tags, []string{"bills", "Finance", "Lunch on Friday"}) {
		t.Fatalf("owner's tags = %q, %v", tags, err)
	}

	// Saving a rule with other tags replaces its own, and a rule can change
	// how it spells a tag no other rule has.
	receipts.Tags = []string{"BILLS"}
	if saved, err := db.SaveMailRuleGuarded(ctx, "default", receipts, nil); err != nil || !slices.Equal(saved.Tags, []string{"BILLS"}) {
		t.Fatalf("resaved tags = %+v, %v", saved, err)
	}
	if tags, err := db.ListMailRuleTags(ctx, "default"); err != nil || !slices.Equal(tags, []string{"BILLS", "Finance"}) {
		t.Fatalf("owner's tags after the change = %q, %v", tags, err)
	}

	for _, bad := range []struct {
		tags []string
		want string
	}{
		{[]string{"a,b"}, "comma"},
		{[]string{strings.Repeat("x", 41)}, "at most 40 characters"},
		{[]string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}, "at most 10 tags"},
	} {
		rule := receipts
		rule.Tags = bad.tags
		if _, err := db.SaveMailRuleGuarded(ctx, "default", rule, nil); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("tags %q: error = %v, want %q", bad.tags, err, bad.want)
		}
	}

	if err := db.DeleteMailRuleGuarded(ctx, "default", bank.ID, nil); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM mail_rule_tags WHERE rule_id = '`+bank.ID+`'`); n != 0 {
		t.Fatalf("a deleted rule left %d tags", n)
	}
}

// A tag is added to several rules at once, or taken off them, and the
// rules that already have it, as many tags as they may, or another owner
// are dealt with as they should be.
func TestAddAndRemoveMailRulesTag(t *testing.T) {
	db := newMailRulesTestDB(t, "imap")
	ctx := t.Context()
	star := []models.MailRuleAction{{Kind: "star"}}
	var ids []string
	for _, name := range []string{"Offers", "Orders", "Lunch on Friday"} {
		ids = append(ids, saveTestRule(t, db, models.MailRule{Name: name, Conditions: fromBakery(), Actions: star}).ID)
	}
	if _, err := db.Write().Exec(`INSERT INTO users (id, username, username_normalized, name) VALUES ('sky', 'sky', 'sky', 'Sky')`); err != nil {
		t.Fatal(err)
	}
	other, err := db.SaveMailRuleGuarded(ctx, "sky", models.MailRule{Name: "Sky's", Enabled: true, Conditions: fromBakery(), Actions: star}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tagsOf := func(id string) []string {
		t.Helper()
		owner := "default"
		if id == other.ID {
			owner = "sky"
		}
		rule, err := db.GetMailRule(ctx, owner, id)
		if err != nil {
			t.Fatal(err)
		}
		return rule.Tags
	}

	if n, err := db.AddMailRulesTagGuarded(ctx, "default", []string{ids[0], ids[1], ids[1], "missing", other.ID}, " #Shopping", nil); err != nil || n != 2 {
		t.Fatalf("add = %d, %v", n, err)
	}
	if n, err := db.AddMailRulesTagGuarded(ctx, "default", ids[1:], "shopping", nil); err != nil || n != 1 {
		t.Fatalf("add again = %d, %v", n, err)
	}
	for _, id := range ids {
		if got := tagsOf(id); !slices.Equal(got, []string{"Shopping"}) {
			t.Fatalf("tags of %s = %q", id, got)
		}
	}
	if got := tagsOf(other.ID); len(got) != 0 {
		t.Fatalf("another owner's rule got %q", got)
	}
	if _, err := db.AddMailRulesTagGuarded(ctx, "default", ids, "  ", nil); err == nil || !strings.Contains(err.Error(), "enter the tag") {
		t.Fatalf("empty tag = %v", err)
	}

	// A rule with as many tags as it may takes no more, and then no rule
	// changes.
	full := saveTestRule(t, db, models.MailRule{Name: "Full", Conditions: fromBakery(), Actions: star,
		Tags: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}})
	if n, err := db.AddMailRulesTagGuarded(ctx, "default", []string{ids[0], full.ID}, "Travel", nil); !errors.Is(err, ErrTooManyMailRuleTags) || n != 0 {
		t.Fatalf("add to a full rule = %d, %v", n, err)
	}
	if got := tagsOf(ids[0]); !slices.Equal(got, []string{"Shopping"}) {
		t.Fatalf("a refused change still tagged a rule: %q", got)
	}

	if n, err := db.RemoveMailRulesTagGuarded(ctx, "default", []string{ids[0], ids[2], "missing", other.ID}, "SHOPPING", nil); err != nil || n != 2 {
		t.Fatalf("remove = %d, %v", n, err)
	}
	if a, b, c := tagsOf(ids[0]), tagsOf(ids[1]), tagsOf(ids[2]); len(a) != 0 || !slices.Equal(b, []string{"Shopping"}) || len(c) != 0 {
		t.Fatalf("tags after removing = %q %q %q", a, b, c)
	}
}
