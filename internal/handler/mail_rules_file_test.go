package handler

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mailrules"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// uploadMailRulesFile sends a rules file to be previewed, as the import
// dialog's form does.
func uploadMailRulesFile(t *testing.T, h *Handler, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "gofer-rules.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/settings/rules/import/preview", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	h.handlePreviewMailRulesImport(rec, ownerRequest(req))
	return rec
}

func saveOwnerRule(t *testing.T, db *storage.DB, rule models.MailRule) models.MailRule {
	t.Helper()
	rule.MatchAll = true
	saved, err := db.SaveMailRuleGuarded(t.Context(), "owner", rule, nil)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func ownerRuleNames(t *testing.T, db *storage.DB) []string {
	t.Helper()
	rules, err := db.ListMailRules(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, rule := range rules {
		names = append(names, rule.Name)
	}
	return names
}

func TestExportMailRules(t *testing.T) {
	h, db := newMailRulesTestHandler(t)
	receipts := saveOwnerRule(t, db, models.MailRule{Name: "Receipts", Enabled: true, AccountsChosen: true, AccountIDs: []string{"victim-account"},
		Conditions: []models.MailRuleCondition{{Field: "from", Operator: "contains", Value: "orders@corner-shop.example"}},
		Actions:    []models.MailRuleAction{{Kind: "move", Folder: &models.MailRuleFolderTarget{FolderID: "owner-receipts"}}}})
	saveOwnerRule(t, db, models.MailRule{Name: "Archive offers", Enabled: true,
		Conditions: []models.MailRuleCondition{{Field: "subject", Operator: "contains", Value: "offer"}},
		Actions:    []models.MailRuleAction{{Kind: "archive"}}})

	rec := identityRequest(t, h, h.handleExportMailRules, http.MethodGet, "/api/settings/rules/export", nil, nil, true)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), `attachment; filename="gofer-rules-`) {
		t.Fatalf("export = %d %v", rec.Code, rec.Header())
	}
	file, err := mailrules.ParseFile(rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Rules) != 2 || file.Rules[0].Name != "Receipts" || file.Rules[0].Actions[0].Folder.Path != "Receipts" {
		t.Fatalf("file = %+v", file)
	}
	if body := rec.Body.String(); strings.Contains(body, "owner-receipts") || strings.Contains(body, "victim-account") {
		t.Fatalf("the file has IDs: %s", body)
	}

	rec = identityRequest(t, h, h.handleExportMailRules, http.MethodGet, "/api/settings/rules/export?rule_ids="+receipts.ID+",rul_gone", nil, nil, true)
	if file, err := mailrules.ParseFile(rec.Body.Bytes()); err != nil || len(file.Rules) != 1 || file.Rules[0].Name != "Receipts" {
		t.Fatalf("selected = %d %v %+v", rec.Code, err, file)
	}
	// Another user's rules aren't theirs to export.
	rec = identityRequest(t, h, h.handleExportMailRules, http.MethodGet, "/api/settings/rules/export?rule_ids="+receipts.ID, nil, nil, false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("another user's rule = %d %q", rec.Code, rec.Body.String())
	}
}

func TestImportMailRulesFromFile(t *testing.T) {
	h, db := newMailRulesTestHandler(t)
	saveOwnerRule(t, db, models.MailRule{Name: "Receipts", Enabled: true, AccountsChosen: true, AccountIDs: []string{"victim-account"},
		Conditions: []models.MailRuleCondition{{Field: "from", Operator: "contains", Value: "orders@corner-shop.example"}},
		Actions:    []models.MailRuleAction{{Kind: "move", Folder: &models.MailRuleFolderTarget{FolderID: "owner-receipts"}}}})
	saveOwnerRule(t, db, models.MailRule{Name: "Archive offers", Enabled: true,
		Conditions: []models.MailRuleCondition{{Field: "subject", Operator: "contains", Value: "offer"}},
		Actions:    []models.MailRuleAction{{Kind: "archive"}}})

	// A file from another Gofer: the same Receipts rule, another rule
	// named Archive offers, one moving to a folder this account lacks, one
	// for an account not here, and one this Gofer can't run.
	file := &mailrules.File{Format: mailrules.FileFormat, Version: mailrules.FileVersion, ExportedAt: "2026-11-20T09:30:00Z",
		Accounts: []mailrules.FileAccount{
			{Ref: "account-1", Provider: "imap", Email: "Owner@Example.com"},
			{Ref: "account-2", Provider: "gmail", Email: "sky@example.net"},
		},
		Rules: []mailrules.FileRule{
			{Name: "Receipts", Enabled: true, Accounts: []string{"account-1"},
				Conditions: []mailrules.FileCondition{{Field: "from", Operator: "contains", Value: "orders@corner-shop.example"}},
				Actions:    []mailrules.FileAction{{Kind: "move", Folder: &mailrules.FileFolder{Account: "account-1", Path: "Receipts"}}}},
			{Name: "Archive offers", Enabled: true, AllAccounts: true,
				Conditions: []mailrules.FileCondition{{Field: "subject", Operator: "contains", Value: "sale"}},
				Actions:    []mailrules.FileAction{{Kind: "archive"}}},
			{Name: "Trips", Enabled: true, AllAccounts: true,
				Conditions: []mailrules.FileCondition{{Field: "subject", Operator: "contains", Value: "boarding pass"}},
				Actions:    []mailrules.FileAction{{Kind: "move", Folder: &mailrules.FileFolder{Account: "account-1", Path: "Trips"}}}},
			{Name: "Sky", Enabled: true, Accounts: []string{"account-2"},
				Conditions: []mailrules.FileCondition{{Field: "subject", Operator: "contains", Value: "Lunch on Friday?"}},
				Actions:    []mailrules.FileAction{{Kind: "star"}}},
			{Name: "Forward", Enabled: true, AllAccounts: true,
				Conditions: []mailrules.FileCondition{{Field: "subject", Operator: "contains", Value: "invoice"}},
				Actions:    []mailrules.FileAction{{Kind: "forward"}}},
		}}
	content, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}

	rec := uploadMailRulesFile(t, h, []byte("Lunch on Friday?"))
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "isn&#39;t a Gofer rules file") || !strings.Contains(body, `name="file"`) {
		t.Fatalf("not a rules file = %d %q", rec.Code, body)
	}

	rec = uploadMailRulesFile(t, h, content)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "data-mail-rules-import-preview") || strings.Count(body, "data-mail-rules-import-rule=") != 5 {
		t.Fatalf("preview = %d %q", rec.Code, body)
	}
	for _, want := range []string{
		"The same rule is already here.",
		"A rule with this name is already here.",
		"Comes in turned off, and turns on again once Gofer finds the folder “Trips” in Owner@Example.com.",
		"Not found here: the account sky@example.net.",
		"Can&#39;t be imported.",
		`name="rule_action:0" value="skip" checked`,
		`name="rule_action:1" value="add" checked`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview lacks %q", want)
		}
	}
	if strings.Contains(body, "rule_action:2") || strings.Contains(body, "rule_action:4") {
		t.Error("a rule whose name is free, or that can't be imported, has choices")
	}

	rec = identityRequest(t, h, h.handleImportMailRules, http.MethodPost, "/api/settings/rules/import", nil,
		url.Values{"file": {string(content)}, "rule_action:0": {"skip"}, "rule_action:1": {"add"}}, true)
	if body := rec.Body.String(); rec.Code != http.StatusOK || rec.Header().Get("HX-Trigger") != "mail-rule-dialog-done" ||
		!strings.Contains(body, "3 rules imported. 1 turns on again once Gofer finds the folders it&#39;s missing. 1 is turned off until you choose what it&#39;s missing. 2 skipped.") {
		t.Fatalf("import = %d %v %q", rec.Code, rec.Header(), body)
	}
	if got := ownerRuleNames(t, db); !slices.Equal(got, []string{"Receipts", "Archive offers", "Archive offers (2)", "Trips", "Sky"}) {
		t.Fatalf("rules = %v", got)
	}
	rules, err := db.ListMailRules(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if trips := rules[3]; trips.Enabled || !trips.MissingFolders() || trips.Actions[0].Folder.AccountID != "victim-account" || !trips.Actions[0].Folder.TurnOnWhenFound {
		t.Fatalf("trips = %+v, folder %+v", trips, trips.Actions[0].Folder)
	}
	if sky := rules[4]; sky.Enabled || !sky.AccountsChosen || len(sky.AccountIDs) != 0 {
		t.Fatalf("sky = %+v", sky)
	}
	if offers := rules[2]; !offers.Enabled {
		t.Fatalf("offers = %+v", offers)
	}

	// Another user's import is theirs alone: the owner's address isn't one
	// of their accounts.
	rec = identityRequest(t, h, h.handleImportMailRules, http.MethodPost, "/api/settings/rules/import", nil, url.Values{"file": {string(content)}}, false)
	if rec.Code != http.StatusOK || len(ownerRuleNames(t, db)) != 5 {
		t.Fatalf("another user's import = %d, owner has %v", rec.Code, ownerRuleNames(t, db))
	}
	theirs, err := db.ListMailRules(t.Context(), "attacker")
	if err != nil || len(theirs) != 4 || slices.ContainsFunc(theirs, func(rule models.MailRule) bool { return slices.Contains(rule.AccountIDs, "victim-account") }) {
		t.Fatalf("their rules = %+v, %v", theirs, err)
	}
}

// A rule that names a folder an import didn't find keeps showing it in its
// form until another is chosen, can't be saved with it, and can't be
// turned on.
func TestMailRuleWithMissingFolder(t *testing.T) {
	h, db := newMailRulesTestHandler(t)
	ctx := t.Context()
	missing := &models.MailRuleFolderTarget{Path: "Trips", AccountID: "victim-account", AccountName: "owner@example.com"}
	imported, err := db.ImportMailRulesGuarded(ctx, "owner", []models.MailRule{{Name: "Trips", MatchAll: true, AccountsChosen: true, AccountIDs: []string{"victim-account"},
		Conditions: []models.MailRuleCondition{{Field: "folder", Operator: "is", Folder: missing}},
		Actions:    []models.MailRuleAction{{Kind: "star"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := imported[0].ID
	value := mailrules.MissingFolderValue(*missing)

	rec := identityRequest(t, h, h.handleMailRuleDialog, http.MethodGet, "/api/settings/rules/dialog?edit="+id, nil, nil, true)
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, `value="`+value+`"`) || !strings.Contains(body, "Not found in owner@example.com") {
		t.Fatalf("edit = %d %q", rec.Code, body)
	}
	// Drawing the form again keeps it, rather than taking the Inbox for it.
	form := mailRuleForm(map[string]string{"id": id, "name": "Trips", "cond_field": "folder", "cond_op": "is", "cond_value": "", "cond_folder": value, "act_kind": "star", "op": "add_action"})
	rec = identityRequest(t, h, h.handleMailRuleForm, http.MethodPost, "/api/settings/rules/form", nil, form, true)
	if body := rec.Body.String(); !strings.Contains(body, `value="`+value+`"`) || strings.Contains(body, `name="cond_folder" value="role:inbox"`) {
		t.Fatalf("form drawn again = %q", body)
	}
	form.Del("op")
	rec = identityRequest(t, h, h.handleSaveMailRule, http.MethodPost, "/api/settings/rules", nil, form, true)
	if body := rec.Body.String(); !strings.Contains(body, "Choose a folder in place of “Trips”, which wasn’t found.") && !strings.Contains(body, "Choose a folder in place of “Trips”, which wasn&#39;t found.") {
		t.Fatalf("save = %q", body)
	}
	if rule, err := db.GetMailRule(ctx, "owner", id); err != nil || !rule.MissingFolders() {
		t.Fatalf("rule = %+v, %v", rule, err)
	}

	rec = identityRequest(t, h, h.handleMailRuleEnabled, http.MethodPost, "/api/settings/rules/"+id+"/enabled", map[string]string{"id": id}, url.Values{"enabled": {"true"}}, true)
	if body := rec.Body.String(); !strings.Contains(body, "Edit it to choose another before turning it on.") {
		t.Fatalf("turn on = %d %q", rec.Code, body)
	}
	rec = identityRequest(t, h, h.handleMailRulesEnabled, http.MethodPost, "/api/settings/rules/selected/enabled", nil, url.Values{"rule_ids": {id}, "enabled": {"1"}}, true)
	if body := rec.Body.String(); !strings.Contains(body, "0 rules turned on. 1 stays off until you choose the folder it&#39;s missing.") {
		t.Fatalf("turn on selected = %d %q", rec.Code, body)
	}
	if rule, err := db.GetMailRule(ctx, "owner", id); err != nil || rule.Enabled {
		t.Fatalf("rule = %+v, %v", rule, err)
	}
	// Choosing a folder in its place saves it.
	form.Set("cond_folder", "role:inbox")
	rec = identityRequest(t, h, h.handleSaveMailRule, http.MethodPost, "/api/settings/rules", nil, form, true)
	if rule, err := db.GetMailRule(ctx, "owner", id); err != nil || rule.MissingFolders() || !rule.Enabled {
		t.Fatalf("saved = %d, rule %+v, %v", rec.Code, rule, err)
	}
}
