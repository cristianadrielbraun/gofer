package mailrules

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// Two installations of the same person: the accounts have the same
// addresses but other IDs, and the folders other IDs too.
var (
	homeAccounts = []models.MailRuleAccount{
		{ID: "acc-robin", Name: "Robin", Email: "robin@example.org", Provider: "imap", Folders: []models.MailRuleFolderChoice{
			{ID: "f-inbox", Name: "Inbox", Path: "INBOX", Role: "inbox"},
			{ID: "f-receipts", Name: "Receipts", Path: "Lists/Receipts", Role: "custom"},
		}},
		{ID: "acc-sky", Name: "Sky", Email: "sky@example.net", Provider: "gmail", Folders: []models.MailRuleFolderChoice{
			{ID: "f-sky-trips", Name: "Trips", Path: "Trips", Role: "custom"},
		}},
		{ID: "acc-local", Name: "Local folders", Provider: "local", Folders: []models.MailRuleFolderChoice{
			{ID: "f-local-keep", Name: "Keep", Path: "Keep", Role: "custom"},
		}},
	}
	workAccounts = []models.MailRuleAccount{
		{ID: "w-local", Name: "Local folders", Provider: "local", Folders: []models.MailRuleFolderChoice{
			{ID: "w-local-keep", Name: "Keep", Path: "Keep", Role: "custom"},
		}},
		{ID: "w-robin", Name: "Robin", Email: "Robin@Example.org", Provider: "imap", Folders: []models.MailRuleFolderChoice{
			{ID: "w-inbox", Name: "Inbox", Path: "INBOX", Role: "inbox"},
			{ID: "w-receipts", Name: "Receipts", Path: "Lists/Receipts", Role: "custom"},
		}},
	}
)

func homeRules() []models.MailRule {
	return []models.MailRule{
		{ID: "r1", Name: "Receipts", Enabled: true, MatchAll: true, StopProcessing: true, Tags: []string{"finance", "home"},
			AccountsChosen: true, AccountIDs: []string{"acc-robin"},
			Conditions: []models.MailRuleCondition{
				{Group: 0, Field: "from", Operator: "contains", Value: "harbor-bakery"},
				{Group: 0, Field: "subject", Operator: "contains", Value: "receipt"},
				{Group: 1, Field: "folder", Operator: "is", Folder: &models.MailRuleFolderTarget{FolderID: "f-inbox", AccountID: "acc-robin", Path: "INBOX"}},
			},
			Actions: []models.MailRuleAction{
				{Kind: "move", Folder: &models.MailRuleFolderTarget{FolderID: "f-receipts", AccountID: "acc-robin", Path: "Lists/Receipts"}},
				{Kind: "label", Label: "Bread"},
			}},
		{ID: "r2", Name: "Keep trips", Enabled: true, MatchAll: true,
			Conditions: []models.MailRuleCondition{{Field: "subject", Operator: "contains", Value: "boarding pass"}},
			Actions:    []models.MailRuleAction{{Kind: "move", ForGood: true, Folder: &models.MailRuleFolderTarget{FolderID: "f-local-keep", AccountID: "acc-local", Path: "Keep"}}}},
		{ID: "r3", Name: "Sky trips", Enabled: true, MatchAll: true, AccountsChosen: true, AccountIDs: []string{"acc-robin", "acc-sky"},
			Conditions: []models.MailRuleCondition{{Field: "subject", Operator: "contains", Value: "itinerary"}},
			Actions:    []models.MailRuleAction{{Kind: "move", Folder: &models.MailRuleFolderTarget{FolderID: "f-sky-trips", AccountID: "acc-sky", Path: "Trips"}}, {Kind: "star"}}},
		{ID: "r4", Name: "Trash old news", Enabled: false, MatchAll: true,
			Conditions: []models.MailRuleCondition{{Field: "list_id", Operator: "contains", Value: "news.example"}},
			Actions:    []models.MailRuleAction{{Kind: "trash"}}},
	}
}

// exportAndRead writes rules to a file and reads the file back, as moving
// it to another installation does.
func exportAndRead(t *testing.T, rules []models.MailRule, accounts []models.MailRuleAccount) *File {
	t.Helper()
	data, err := json.Marshal(Export(rules, accounts, time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	file, err := ParseFile(data)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestExportNamesAccountsAndFoldersByAddressAndPath(t *testing.T) {
	file := Export(homeRules(), homeAccounts, time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC))
	if file.Format != FileFormat || file.Version != FileVersion || file.ExportedAt != "2026-11-20T09:30:00Z" || len(file.Rules) != 4 {
		t.Fatalf("file = %+v", file)
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"acc-robin", "f-receipts", "f-local-keep", "r1"} {
		if strings.Contains(string(data), id) {
			t.Errorf("the file has the ID %q, which means nothing elsewhere", id)
		}
	}
	receipts := file.Rules[0]
	if receipts.AllAccounts || len(receipts.Accounts) != 1 || receipts.Actions[0].Folder.Path != "Lists/Receipts" || receipts.Actions[0].Folder.Account != receipts.Accounts[0] {
		t.Fatalf("receipts = %+v", receipts)
	}
	if !file.Rules[1].AllAccounts || !file.Rules[3].AllAccounts || file.Rules[3].Enabled {
		t.Fatalf("rules for every account = %+v, %+v", file.Rules[1], file.Rules[3])
	}
	// Only the accounts the rules name, each once.
	if len(file.Accounts) != 3 {
		t.Fatalf("accounts = %+v", file.Accounts)
	}
}

func TestFlagColorRuleSurvivesPortableExportAndImport(t *testing.T) {
	rules := []models.MailRule{{
		Name: "Replace a blue flag", MatchAll: true, AccountsChosen: true, AccountIDs: []string{"acc-robin"},
		Conditions: []models.MailRuleCondition{{Field: models.MailRuleFieldFlagColor, Operator: models.MailRuleOpIs, Value: "blue"}},
		Actions: []models.MailRuleAction{
			{Kind: models.MailRuleActionRemoveFlagColor, FlagColor: "blue"},
			{Kind: models.MailRuleActionSetFlagColor, FlagColor: "green"},
		},
	}}
	file := exportAndRead(t, rules, homeAccounts)
	plan := PlanImport(file, workAccounts, nil)
	if len(plan) != 1 || plan[0].Err != nil {
		t.Fatalf("import plan: %+v", plan)
	}
	got := plan[0].Rule
	if !slices.Equal(got.AccountIDs, []string{"w-robin"}) ||
		!slices.Equal(got.Conditions, rules[0].Conditions) ||
		!slices.Equal(got.Actions, rules[0].Actions) {
		t.Fatalf("round-tripped rule = %+v", got)
	}
}

func TestImportFindsAccountsAndFoldersElsewhere(t *testing.T) {
	plans := PlanImport(exportAndRead(t, homeRules(), homeAccounts), workAccounts, nil)
	if len(plans) != 4 {
		t.Fatalf("plans = %+v", plans)
	}
	receipts := plans[0]
	if receipts.Err != nil || receipts.Missing() || !receipts.Rule.Enabled || !receipts.Rule.StopProcessing {
		t.Fatalf("receipts = %+v", receipts)
	}
	rule := receipts.Rule
	if !rule.AccountsChosen || !slices.Equal(rule.AccountIDs, []string{"w-robin"}) || !slices.Equal(rule.Tags, []string{"finance", "home"}) {
		t.Fatalf("receipts rule = %+v", rule)
	}
	if len(rule.Conditions) != 3 || rule.Conditions[1].Group != 0 || rule.Conditions[2].Group != 1 || rule.Conditions[2].Folder.FolderID != "w-inbox" {
		t.Fatalf("conditions = %+v", rule.Conditions)
	}
	if move := rule.Actions[0].Folder; move.FolderID != "w-receipts" || move.AccountID != "w-robin" || rule.Actions[1].Label != "Bread" {
		t.Fatalf("actions = %+v, folder %+v", rule.Actions, move)
	}
	// Local folders are found as Local folders.
	keep := plans[1]
	if keep.Missing() || keep.Err != nil || keep.Rule.AccountsChosen || keep.Rule.Actions[0].Folder.FolderID != "w-local-keep" || !keep.Rule.Actions[0].ForGood {
		t.Fatalf("keep trips = %+v", keep)
	}
	// A rule off stays off.
	if trash := plans[3]; trash.Missing() || trash.Err != nil || trash.Rule.Enabled {
		t.Fatalf("trash old news = %+v", trash)
	}
}

// What isn't found is said, and the rule comes in turned off: the account
// isn't here, so neither are its folders, and nothing is guessed.
func TestImportSaysWhatIsMissing(t *testing.T) {
	sky := PlanImport(exportAndRead(t, homeRules(), homeAccounts), workAccounts, nil)[2]
	if sky.Err != nil || sky.Rule.Enabled {
		t.Fatalf("sky trips = %+v", sky)
	}
	if !slices.Equal(sky.MissingAccounts, []string{"sky@example.net"}) || !slices.Equal(sky.Rule.AccountIDs, []string{"w-robin"}) {
		t.Fatalf("accounts: missing %v, kept %v", sky.MissingAccounts, sky.Rule.AccountIDs)
	}
	if !slices.Equal(sky.MissingFolders, []MissingFolder{{Path: "Trips", Account: "sky@example.net"}}) {
		t.Fatalf("missing folders = %+v", sky.MissingFolders)
	}
	if folder := sky.Rule.Actions[0].Folder; !folder.Missing() || folder.Path != "Trips" || folder.AccountID != "" || folder.AccountName != "sky@example.net" {
		t.Fatalf("missing folder = %+v", folder)
	}

	// The account is here, but not the folder, even with one of the same
	// name elsewhere.
	without := slices.Clone(workAccounts)
	without[1].Folders = []models.MailRuleFolderChoice{{ID: "w-inbox", Name: "Inbox", Path: "INBOX", Role: "inbox"}, {ID: "w-other", Name: "Receipts", Path: "Archive/Receipts"}}
	receipts := PlanImport(exportAndRead(t, homeRules()[:1], homeAccounts), without, nil)[0]
	if receipts.Rule.Enabled || len(receipts.MissingAccounts) != 0 || !slices.Equal(receipts.MissingFolders, []MissingFolder{{Path: "Lists/Receipts", Account: "robin@example.org"}}) {
		t.Fatalf("receipts = %+v", receipts)
	}
	if folder := receipts.Rule.Actions[0].Folder; folder.FolderID != "" || folder.AccountID != "w-robin" {
		t.Fatalf("missing folder = %+v", folder)
	}

	// With none of its accounts here, a rule is for no account.
	alone := PlanImport(exportAndRead(t, homeRules()[:1], homeAccounts), workAccounts[:1], nil)[0]
	if alone.Err != nil || !alone.Rule.AccountsChosen || len(alone.Rule.AccountIDs) != 0 || !slices.Equal(alone.MissingAccounts, []string{"robin@example.org"}) {
		t.Fatalf("alone = %+v", alone)
	}
}

// A rule that was on and misses only folders of accounts that are here, as
// the accounts a profile import adds before they sync, turns on again once
// they are found; one missing an account, or off where exported, doesn't.
func TestImportSaysWhichRulesTurnOnOnceFound(t *testing.T) {
	without := slices.Clone(workAccounts)
	without[1].Folders = nil
	rules := homeRules()
	off := rules[0]
	off.Name, off.Enabled = "Receipts, off", false
	plans := PlanImport(exportAndRead(t, append(rules, off), homeAccounts), without, nil)
	receipts, sky, offPlan := plans[0], plans[2], plans[4]
	if receipts.Err != nil || receipts.Rule.Enabled || !receipts.TurnsOn {
		t.Fatalf("receipts = %+v", receipts)
	}
	for _, target := range []*models.MailRuleFolderTarget{receipts.Rule.Conditions[2].Folder, receipts.Rule.Actions[0].Folder} {
		if !target.Missing() || target.AccountID != "w-robin" || !target.TurnOnWhenFound {
			t.Fatalf("receipts folder = %+v", target)
		}
	}
	if sky.TurnsOn || sky.Rule.Actions[0].Folder.TurnOnWhenFound {
		t.Fatalf("a rule missing an account turns on once found: %+v", sky)
	}
	if offPlan.TurnsOn || offPlan.Rule.Actions[0].Folder.TurnOnWhenFound {
		t.Fatalf("a rule off where exported turns on once found: %+v", offPlan)
	}

	// A folder condition can't wait for a folder of an account the rule
	// isn't for, as it couldn't name one.
	file := exportAndRead(t, rules[:1], homeAccounts)
	file.Rules[0].Accounts = nil
	file.Rules[0].AllAccounts = false
	if plan := PlanImport(file, without, nil)[0]; plan.Err == nil {
		t.Fatalf("a folder condition for an account the rule isn't for = %+v", plan)
	}
}

// A folder that wasn't found goes out again as it was known, so exporting
// an imported rule before fixing it loses nothing.
func TestExportKeepsMissingFolders(t *testing.T) {
	sky := PlanImport(exportAndRead(t, homeRules(), homeAccounts), workAccounts, nil)[2].Rule
	again := PlanImport(exportAndRead(t, []models.MailRule{sky}, workAccounts), homeAccounts, nil)[0]
	if again.Missing() || again.Rule.Actions[0].Folder.FolderID != "f-sky-trips" {
		t.Fatalf("exported again and imported at home = %+v, folder %+v", again, again.Rule.Actions[0].Folder)
	}
}

func TestImportComparesNames(t *testing.T) {
	existing := PlanImport(exportAndRead(t, homeRules(), homeAccounts), workAccounts, nil)
	for i := range existing {
		existing[i].Rule.ID = "w" + existing[i].Rule.Name
	}
	current := []models.MailRule{existing[0].Rule, existing[3].Rule}
	current[1].Name = "TRASH OLD NEWS"
	current[1].Actions = []models.MailRuleAction{{Kind: "archive"}}
	plans := PlanImport(exportAndRead(t, homeRules(), homeAccounts), workAccounts, current)
	if plans[0].SameName != current[0].ID || !plans[0].Same {
		t.Fatalf("the same rule = %+v", plans[0])
	}
	if plans[3].SameName != current[1].ID || plans[3].Same {
		t.Fatalf("another rule of the same name = %+v", plans[3])
	}
	if plans[1].SameName != "" || plans[2].SameName != "" {
		t.Fatalf("rules with names of their own = %+v, %+v", plans[1], plans[2])
	}
}

func TestImportRefusesRulesThatCantRun(t *testing.T) {
	file := exportAndRead(t, homeRules()[3:], homeAccounts)
	file.Rules[0].Actions = []FileAction{{Kind: "forward"}}
	file.Rules = append(file.Rules, FileRule{Name: "Bad pattern", AllAccounts: true,
		Conditions: []FileCondition{{Field: "subject", Operator: "matches", Value: "(unclosed"}}, Actions: []FileAction{{Kind: "star"}}})
	plans := PlanImport(file, workAccounts, nil)
	if plans[0].Err == nil || plans[1].Err == nil {
		t.Fatalf("plans = %+v", plans)
	}
}

func TestParseFileRefuses(t *testing.T) {
	valid, err := json.Marshal(Export(homeRules(), homeAccounts, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	change := func(edit func(map[string]any)) []byte {
		var file map[string]any
		if err := json.Unmarshal(valid, &file); err != nil {
			t.Fatal(err)
		}
		edit(file)
		data, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, check := range []struct {
		name string
		data []byte
		want error
	}{
		{"not JSON", []byte("Lunch on Friday?"), ErrNotRulesFile},
		{"a profile", []byte(`{"format":"gofer-profile","version":2}`), ErrNotRulesFile},
		{"a newer file", change(func(f map[string]any) { f["version"] = FileVersion + 1 }), ErrNewerFile},
		{"no version", change(func(f map[string]any) { delete(f, "version") }), ErrDamagedFile},
		{"an account named twice", change(func(f map[string]any) {
			accounts := f["accounts"].([]any)
			f["accounts"] = append(accounts, accounts[0])
		}), ErrDamagedFile},
		{"a rule for an account not in it", change(func(f map[string]any) {
			f["rules"].([]any)[0].(map[string]any)["accounts"] = []string{"account-99"}
		}), ErrDamagedFile},
		{"a folder of an account not in it", change(func(f map[string]any) {
			rule := f["rules"].([]any)[0].(map[string]any)
			rule["actions"].([]any)[0].(map[string]any)["folder"] = map[string]string{"account": "account-99", "path": "Trips"}
		}), ErrDamagedFile},
	} {
		if _, err := ParseFile(check.data); !errors.Is(err, check.want) {
			t.Errorf("%s: err = %v, want %v", check.name, err, check.want)
		}
	}
}

func TestUniqueName(t *testing.T) {
	taken := map[string]bool{"Receipts": true, "Receipts (2)": true}
	if got := UniqueName("Receipts", func(name string) bool { return taken[name] }); got != "Receipts (3)" {
		t.Fatalf("got %q", got)
	}
	if got := UniqueName(" Trips ", func(name string) bool { return taken[name] }); got != "Trips" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("é", MaxNameLength)
	got := UniqueName(long, func(name string) bool { return name == long })
	if !strings.HasSuffix(got, " (2)") || len([]rune(got)) != MaxNameLength {
		t.Fatalf("got %q (%d characters)", got, len([]rune(got)))
	}
}
