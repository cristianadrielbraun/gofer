package mailrules

import (
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func compileRule(t *testing.T, rule models.MailRule) *Rule {
	t.Helper()
	if rule.Name == "" {
		rule.Name = "Test rule"
	}
	if len(rule.Actions) == 0 {
		rule.Actions = []models.MailRuleAction{{Kind: models.MailRuleActionMarkRead}}
	}
	compiled, err := Compile(rule)
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func condition(field, operator, value string) models.MailRuleCondition {
	return models.MailRuleCondition{Field: field, Operator: operator, Value: value}
}

var newsletter = Message{
	FromName:  "Harbor Bakery",
	FromEmail: "news@harbor-bakery.example",
	To:        []Address{{Name: "Robin Vale", Email: "robin@example.org"}},
	CC:        []Address{{Email: "team@example.org"}},
	Subject:   "Weekly loaves: rye is back",
	ListID:    "Harbor Bakery news <news.harbor-bakery.example>",
	Folders:   []Folder{{ID: "acct_f_inbox", Role: "inbox"}},
}

func TestConditionOperators(t *testing.T) {
	for _, tc := range []struct {
		name string
		cond models.MailRuleCondition
		want bool
	}{
		{"from contains address", condition("from", "contains", "HARBOR-bakery"), true},
		{"from contains name", condition("from", "contains", "bakery"), true},
		{"from is address", condition("from", "is", "news@harbor-bakery.example"), true},
		{"from is name", condition("from", "is", "harbor bakery"), true},
		{"from is partial", condition("from", "is", "news"), false},
		{"from ends with domain", condition("from", "ends_with", "@harbor-bakery.example"), true},
		{"from starts with", condition("from", "starts_with", "news@"), true},
		{"from not contains other", condition("from", "not_contains", "fishmonger"), true},
		{"from not contains own name", condition("from", "not_contains", "Harbor"), false},
		{"from is not", condition("from", "is_not", "news@harbor-bakery.example"), false},
		{"to contains", condition("to", "contains", "robin"), true},
		{"to misses cc", condition("to", "contains", "team@"), false},
		{"cc contains", condition("cc", "contains", "team@"), true},
		{"recipients include cc", condition("recipients", "is", "team@example.org"), true},
		{"subject matches", condition("subject", "matches", `^weekly\s+loaves`), true},
		{"subject pattern misses", condition("subject", "matches", `^daily`), false},
		{"list id contains", condition("list_id", "contains", "news.harbor-bakery.example"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rule := compileRule(t, models.MailRule{MatchAll: true, Conditions: []models.MailRuleCondition{tc.cond}})
			if got := rule.Match(newsletter); got != tc.want {
				t.Fatalf("Match = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEmptyFieldOnlyHoldsForNegativeComparisons(t *testing.T) {
	plain := newsletter
	plain.ListID = ""
	if compileRule(t, models.MailRule{MatchAll: true, Conditions: []models.MailRuleCondition{condition("list_id", "contains", "x")}}).Match(plain) {
		t.Fatal("a missing List-Id contained a value")
	}
	if !compileRule(t, models.MailRule{MatchAll: true, Conditions: []models.MailRuleCondition{condition("list_id", "not_contains", "x")}}).Match(plain) {
		t.Fatal("a missing List-Id did contain a value")
	}
}

func TestMatchAllAndAny(t *testing.T) {
	conditions := []models.MailRuleCondition{
		condition("from", "contains", "bakery"),
		condition("subject", "contains", "invoice"),
	}
	if compileRule(t, models.MailRule{MatchAll: true, Conditions: conditions}).Match(newsletter) {
		t.Fatal("all conditions held with one false")
	}
	if !compileRule(t, models.MailRule{MatchAll: false, Conditions: conditions}).Match(newsletter) {
		t.Fatal("any condition failed with one true")
	}
}

// A rule matches when every condition of one of its groups holds.
func TestConditionGroups(t *testing.T) {
	grouped := func(group int, c models.MailRuleCondition) models.MailRuleCondition {
		c.Group = group
		return c
	}
	// (From contains news and From ends with harbor-bakery.example) or
	// Subject contains invoice, with the groups' conditions given mixed up.
	rule := compileRule(t, models.MailRule{MatchAll: true, Conditions: []models.MailRuleCondition{
		grouped(4, condition("from", "contains", "news")),
		grouped(9, condition("subject", "contains", "invoice")),
		grouped(4, condition("from", "ends_with", "harbor-bakery.example")),
	}})
	if got := []int{rule.Conditions[0].Group, rule.Conditions[1].Group, rule.Conditions[2].Group}; got[0] != 0 || got[1] != 0 || got[2] != 1 || rule.Conditions[2].Field != "subject" {
		t.Fatalf("groups = %v, conditions = %+v", got, rule.Conditions)
	}
	if !rule.Match(newsletter) {
		t.Fatal("the first group held but the rule didn't match")
	}
	other := newsletter
	other.FromEmail, other.FromName = "news@mill.example", "The Mill"
	if rule.Match(other) {
		t.Fatal("matched with half of the first group and none of the second")
	}
	other.Subject = "Your invoice"
	if !rule.Match(other) {
		t.Fatal("the second group held but the rule didn't match")
	}

	// A rule saved before groups, matching any condition, is one group per
	// condition.
	legacy := models.MailRule{Conditions: []models.MailRuleCondition{condition("from", "contains", "mill"), condition("subject", "contains", "invoice")}}
	GroupConditions(&legacy)
	if !legacy.MatchAll || legacy.Conditions[0].Group != 0 || legacy.Conditions[1].Group != 1 {
		t.Fatalf("legacy rule = %+v", legacy)
	}
}

func TestOwnMailNeedsSentOrDraftsNamed(t *testing.T) {
	sent := newsletter
	sent.Folders = []Folder{{ID: "acct_f_sent", Role: "sent"}}
	byFrom := compileRule(t, models.MailRule{MatchAll: true, Conditions: []models.MailRuleCondition{condition("from", "contains", "bakery")}})
	if byFrom.Match(sent) {
		t.Fatal("a rule matched sent mail without naming Sent")
	}
	inSent := compileRule(t, models.MailRule{MatchAll: true, Conditions: []models.MailRuleCondition{
		condition("from", "contains", "bakery"),
		{Field: "folder", Operator: "is", Folder: &models.MailRuleFolderTarget{Role: "sent"}},
	}})
	if !inSent.Match(sent) {
		t.Fatal("a rule naming Sent did not match sent mail")
	}
}

func TestFolderConditions(t *testing.T) {
	spam := newsletter
	spam.Folders = []Folder{{ID: "acct_f_spam", Role: "spam"}}
	inJunk := compileRule(t, models.MailRule{MatchAll: true, Conditions: []models.MailRuleCondition{
		{Field: "folder", Operator: "is", Folder: &models.MailRuleFolderTarget{Role: "spam"}},
	}})
	if !inJunk.Match(spam) {
		t.Fatal("a junk condition did not match a spam folder")
	}
	notInbox := compileRule(t, models.MailRule{AccountID: "acct", MatchAll: true, Conditions: []models.MailRuleCondition{
		{Field: "folder", Operator: "is_not", Folder: &models.MailRuleFolderTarget{FolderID: "acct_f_inbox", Path: "INBOX"}},
	}})
	if notInbox.Match(newsletter) || !notInbox.Match(spam) {
		t.Fatal("a folder ID condition compared wrongly")
	}
}

func TestValidate(t *testing.T) {
	valid := func() models.MailRule {
		return models.MailRule{
			Name:       "  Bakery  ",
			Conditions: []models.MailRuleCondition{condition(" from ", " contains ", " bakery ")},
			Actions:    []models.MailRuleAction{{Kind: "archive", Label: "ignored"}},
		}
	}
	rule := valid()
	if err := Validate(&rule); err != nil {
		t.Fatal(err)
	}
	if rule.Name != "Bakery" || rule.Conditions[0].Field != "from" || rule.Conditions[0].Value != "bakery" || rule.Actions[0].Label != "" {
		t.Fatalf("rule not normalized: %+v", rule)
	}

	for _, tc := range []struct {
		name   string
		change func(*models.MailRule)
		want   string
	}{
		{"no name", func(r *models.MailRule) { r.Name = " " }, "name"},
		{"no conditions", func(r *models.MailRule) { r.Conditions = nil }, "condition"},
		{"no actions", func(r *models.MailRule) { r.Actions = nil }, "action"},
		{"empty value", func(r *models.MailRule) { r.Conditions[0].Value = "" }, "compare"},
		{"bad field", func(r *models.MailRule) { r.Conditions[0].Field = "body" }, "unknown condition"},
		{"bad operator", func(r *models.MailRule) { r.Conditions[0].Operator = "sounds_like" }, "unknown comparison"},
		{"bad pattern", func(r *models.MailRule) { r.Conditions[0].Operator, r.Conditions[0].Value = "matches", "(" }, "pattern"},
		{"two moves", func(r *models.MailRule) {
			r.Actions = append(r.Actions, models.MailRuleAction{Kind: "trash"})
		}, "one place"},
		{"same action twice", func(r *models.MailRule) {
			r.Actions = []models.MailRuleAction{{Kind: "star"}, {Kind: "star"}}
		}, "twice"},
		{"move without folder", func(r *models.MailRule) {
			r.Actions = []models.MailRuleAction{{Kind: "move"}}
		}, "folder"},
		{"move to sent", func(r *models.MailRule) {
			r.Actions = []models.MailRuleAction{{Kind: "move", Folder: &models.MailRuleFolderTarget{Role: "sent"}}}
		}, "Sent"},
		{"folder id on every account", func(r *models.MailRule) {
			r.Actions = []models.MailRuleAction{{Kind: "move", Folder: &models.MailRuleFolderTarget{FolderID: "acct_f_1"}}}
		}, "every account"},
		{"folder contains", func(r *models.MailRule) {
			r.Conditions = []models.MailRuleCondition{{Field: "folder", Operator: "contains", Folder: &models.MailRuleFolderTarget{Role: "inbox"}}}
		}, "folder condition"},
		{"empty label", func(r *models.MailRule) {
			r.Actions = []models.MailRuleAction{{Kind: "label", Label: " "}}
		}, "label"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rule := valid()
			tc.change(&rule)
			err := Validate(&rule)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want error with %q", err, tc.want)
			}
		})
	}

	labels := valid()
	labels.Actions = []models.MailRuleAction{{Kind: "label", Label: "Bread"}, {Kind: "label", Label: "Shops"}}
	if err := Validate(&labels); err != nil {
		t.Fatalf("two different labels refused: %v", err)
	}
}
