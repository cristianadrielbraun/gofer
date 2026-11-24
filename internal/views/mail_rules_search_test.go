package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestMailRulesSearchLooksAtNamesConditionsAndFolders(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), "en")
	accounts := []models.MailRuleAccount{{ID: "acct", Email: "robin@example.test", Provider: "imap", Folders: []models.MailRuleFolderChoice{
		{ID: "receipts", Name: "Receipts", Path: "Finance / Receipts"},
	}}}
	receipts := models.MailRule{
		ID: "r1", Name: "Shop receipts", Enabled: true, MatchAll: true, AccountsChosen: true, AccountIDs: []string{"acct"},
		Conditions: []models.MailRuleCondition{{Field: models.MailRuleFieldFrom, Operator: models.MailRuleOpContains, Value: "Orders@Shop.example"}},
		Actions:    []models.MailRuleAction{{Kind: models.MailRuleActionMove, Folder: &models.MailRuleFolderTarget{FolderID: "receipts", AccountID: "acct", Path: "Finance / Receipts"}}},
	}
	digest := models.MailRule{
		ID: "r2", Name: "Weekly digest", Enabled: true, MatchAll: true,
		Conditions: []models.MailRuleCondition{
			{Field: models.MailRuleFieldSubject, Operator: models.MailRuleOpStartsWith, Value: "Lunch on Friday?"},
			{Field: models.MailRuleFieldFolder, Operator: models.MailRuleOpIs, Folder: &models.MailRuleFolderTarget{Role: "inbox"}},
		},
		Actions: []models.MailRuleAction{{Kind: models.MailRuleActionArchive}},
	}

	for rule, wants := range map[*models.MailRule][]string{
		&receipts: {"shop receipts", "orders@shop.example", "from", "contains", "finance / receipts", "robin@example.test"},
		&digest:   {"weekly digest", "lunch on friday?", "subject", "starts with", "inbox", "archive", "all accounts"},
	} {
		text := mailRuleSearchText(ctx, *rule, accounts)
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("search text of %s = %q, lacks %q", rule.Name, text, want)
			}
		}
		if text != strings.ToLower(text) {
			t.Errorf("search text of %s is not in lower case: %q", rule.Name, text)
		}
	}
	if text := mailRuleSearchText(ctx, digest, accounts); strings.Contains(text, "robin@example.test") {
		t.Errorf("a rule for every account names one: %q", text)
	}

	var out bytes.Buffer
	if err := MailRulesPanel(models.MailRulesData{Rules: []models.MailRule{receipts, digest}, Accounts: accounts}).Render(ctx, &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{`id="mail-rules-search" hx-preserve="true"`, "data-folder-picker-search", "data-mail-rules-search", `aria-label="Search rules"`, "data-folder-picker-account", "data-folder-picker-empty", ">No rules match.<"} {
		if !strings.Contains(html, want) {
			t.Errorf("rules panel lacks %s", want)
		}
	}
	if n := strings.Count(html, "data-folder-picker-row"); n != 2 {
		t.Errorf("rules marked as search rows = %d, want 2", n)
	}

	out.Reset()
	if err := MailRulesPanel(models.MailRulesData{Accounts: accounts}).Render(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "data-mail-rules-search") {
		t.Error("search shown with no rules to search")
	}
}
