package mailrules

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
)

// A rules file carries mail rules to another installation, or back to this
// one. It names accounts by their address and folders by their path in
// their account, which another installation can find again; folders with a
// role, such as Inbox, stand for that folder of any account, as in a rule.
const (
	FileFormat = "gofer-mail-rules"
	// FileVersion is the newest rules file this Gofer reads.
	FileVersion = 1
)

// Errors reading a rules file, said to the person importing it.
var (
	ErrNotRulesFile = errors.New("this file is not a Gofer rules file")
	ErrDamagedFile  = errors.New("this rules file is damaged")
	ErrNewerFile    = errors.New("this rules file was exported by a newer version of Gofer")
)

// File is a rules file: the rules in the order they run, and the accounts
// they name.
type File struct {
	Format     string        `json:"format"`
	Version    int           `json:"version"`
	ExportedAt string        `json:"exported_at,omitempty"`
	Accounts   []FileAccount `json:"accounts,omitempty"`
	Rules      []FileRule    `json:"rules"`
}

// FileAccount is an account the file's rules name, by Ref. An import finds
// it by its address, or Local folders by being Local folders. Name is only
// shown, for an account with no address.
type FileAccount struct {
	Ref      string `json:"ref"`
	Provider string `json:"provider,omitempty"`
	Email    string `json:"email_address,omitempty"`
	Name     string `json:"name,omitempty"`
}

// FileRule is one rule. AllAccounts is set on a rule for every account's
// mail; otherwise it is for the accounts in Accounts, by ref, which may be
// none.
type FileRule struct {
	Name           string          `json:"name"`
	Enabled        bool            `json:"enabled"`
	StopProcessing bool            `json:"stop_processing,omitempty"`
	Tags           []string        `json:"tags,omitempty"`
	AllAccounts    bool            `json:"all_accounts,omitempty"`
	Accounts       []string        `json:"accounts,omitempty"`
	Conditions     []FileCondition `json:"conditions"`
	Actions        []FileAction    `json:"actions"`
}

// FileCondition is one condition. Conditions with the same Group must all
// hold; a rule matches when one of its groups does.
type FileCondition struct {
	Group    int         `json:"group"`
	Field    string      `json:"field"`
	Operator string      `json:"operator"`
	Value    string      `json:"value,omitempty"`
	Folder   *FileFolder `json:"folder,omitempty"`
}

// FileAction is one action.
type FileAction struct {
	Kind    string      `json:"kind"`
	Folder  *FileFolder `json:"folder,omitempty"`
	Label   string      `json:"label,omitempty"`
	ForGood bool        `json:"for_good,omitempty"`
}

// FileFolder is a folder by Role, for whichever account the mail is in, or
// the folder at Path in the account Account names, by ref. A folder whose
// account isn't known has no Account.
type FileFolder struct {
	Role    string `json:"role,omitempty"`
	Account string `json:"account,omitempty"`
	Path    string `json:"path,omitempty"`
}

// Export puts rules in a file, in the order given. accounts are the owner's
// accounts with their folders, as the Rules settings list them; the file
// names only the accounts its rules name. A folder that is gone is written
// as it was last known, to be shown as missing where the file is imported.
func Export(rules []models.MailRule, accounts []models.MailRuleAccount, now time.Time) *File {
	file := &File{Format: FileFormat, Version: FileVersion, ExportedAt: now.UTC().Format(time.RFC3339), Rules: []FileRule{}}
	refs := map[string]string{}
	// ref names an account in the file, adding it the first time. An
	// account that isn't here goes by its key, "name:" and what it was
	// known by.
	ref := func(key string, account FileAccount) string {
		if found, ok := refs[key]; ok {
			return found
		}
		account.Ref = "account-" + strconv.Itoa(len(file.Accounts)+1)
		refs[key] = account.Ref
		file.Accounts = append(file.Accounts, account)
		return account.Ref
	}
	accountRef := func(id string) (string, bool) {
		for _, account := range accounts {
			if account.ID == id {
				return ref("id:"+id, FileAccount{Provider: account.Provider, Email: account.Email, Name: account.Name}), true
			}
		}
		return "", false
	}
	folder := func(target *models.MailRuleFolderTarget) *FileFolder {
		if target == nil {
			return nil
		}
		if target.Role != "" {
			return &FileFolder{Role: target.Role}
		}
		if target.FolderID != "" {
			for _, account := range accounts {
				for _, choice := range account.Folders {
					if choice.ID == target.FolderID {
						found, _ := accountRef(account.ID)
						return &FileFolder{Account: found, Path: fileFolderPath(choice)}
					}
				}
			}
		}
		// The folder isn't here: a folder that is gone, or one an import
		// didn't find.
		exported := &FileFolder{Path: target.Path}
		if found, ok := accountRef(target.AccountID); ok {
			exported.Account = found
		} else if name := strings.TrimSpace(target.AccountName); name != "" {
			exported.Account = ref("name:"+strings.ToLower(name), FileAccount{Email: fileAccountAddress(name), Name: name})
		}
		return exported
	}
	for _, rule := range rules {
		rule.Conditions = slices.Clone(rule.Conditions)
		GroupConditions(&rule)
		exported := FileRule{
			Name: rule.Name, Enabled: rule.Enabled, StopProcessing: rule.StopProcessing, Tags: slices.Clone(rule.Tags),
			AllAccounts: !rule.AccountsChosen, Conditions: []FileCondition{}, Actions: []FileAction{},
		}
		if rule.AccountsChosen {
			for _, id := range rule.AccountIDs {
				if found, ok := accountRef(id); ok {
					exported.Accounts = append(exported.Accounts, found)
				}
			}
		}
		for _, condition := range rule.Conditions {
			exported.Conditions = append(exported.Conditions, FileCondition{
				Group: condition.Group, Field: condition.Field, Operator: condition.Operator, Value: condition.Value, Folder: folder(condition.Folder),
			})
		}
		for _, action := range rule.Actions {
			exported.Actions = append(exported.Actions, FileAction{Kind: action.Kind, Folder: folder(action.Folder), Label: action.Label, ForGood: action.ForGood})
		}
		file.Rules = append(file.Rules, exported)
	}
	return file
}

// fileFolderPath is a folder's path in its account: its name on the
// server, or its name in Gofer for a folder with none.
func fileFolderPath(folder models.MailRuleFolderChoice) string {
	if folder.Path != "" {
		return folder.Path
	}
	return folder.Name
}

// fileAccountAddress is name when it is an address, as an account an
// import didn't find is known by; otherwise it is "".
func fileAccountAddress(name string) string {
	if strings.Contains(name, "@") {
		return name
	}
	return ""
}

// ParseFile reads a rules file and checks that it holds together: every
// account a rule names is in it, once. It doesn't check the rules
// themselves; PlanImport says which can't be imported, and why.
func ParseFile(data []byte) (*File, error) {
	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, ErrNotRulesFile
	}
	if err := CheckFile(&file); err != nil {
		return nil, err
	}
	return &file, nil
}

// CheckFile checks a rules file read as part of another file, as ParseFile
// does.
func CheckFile(file *File) error {
	switch {
	case file.Format != FileFormat:
		return ErrNotRulesFile
	case file.Version < 1:
		return ErrDamagedFile
	case file.Version > FileVersion:
		return ErrNewerFile
	case len(file.Rules) > MaxRulesPerOwner:
		return ErrDamagedFile
	}
	refs := map[string]bool{}
	for _, account := range file.Accounts {
		if strings.TrimSpace(account.Ref) == "" || refs[account.Ref] {
			return ErrDamagedFile
		}
		if strings.TrimSpace(account.Email) == "" && strings.TrimSpace(account.Name) == "" && account.Provider != providers.ProviderLocal {
			return ErrDamagedFile
		}
		refs[account.Ref] = true
	}
	known := func(folder *FileFolder) bool {
		return folder == nil || folder.Account == "" || refs[folder.Account]
	}
	for _, rule := range file.Rules {
		for _, ref := range rule.Accounts {
			if !refs[ref] {
				return ErrDamagedFile
			}
		}
		for _, condition := range rule.Conditions {
			if !known(condition.Folder) {
				return ErrDamagedFile
			}
		}
		for _, action := range rule.Actions {
			if !known(action.Folder) {
				return ErrDamagedFile
			}
		}
	}
	return nil
}

// ImportRule is one rule of a file as importing it would add it.
type ImportRule struct {
	// Index is the rule's place in the file.
	Index int
	// Rule is the rule to add, with the accounts and folders found here.
	// It is turned off when something it names wasn't found.
	Rule models.MailRule
	// MissingAccounts name the accounts the rule was for that aren't here,
	// by address, or name for one with none. The rule is left for the
	// others; with none left, it is for no account until it's given one.
	MissingAccounts []string
	// MissingFolders are the folders the rule names that weren't found.
	MissingFolders []MissingFolder
	// TurnsOn is set on a rule that was on and is missing only folders of
	// accounts that are here: it turns on again once they are found there,
	// as when such an account first syncs its folders.
	TurnsOn bool
	// Err says why the rule can't be imported: it isn't one this Gofer can
	// run. It is shown to the person importing.
	Err error
	// SameName is the ID of a rule here with the same name, but for its
	// capitals; Same is set when that rule also does the same.
	SameName string
	Same     bool
}

// MissingFolder is a folder an imported rule names that wasn't found.
type MissingFolder = models.MailRuleMissingFolder

// MissingFolderValue is how a rule's form sends a folder an import didn't
// find, while no other is chosen: "missing:" and its account's ID, its
// account's name and its path, each escaped, separated by colons.
func MissingFolderValue(target models.MailRuleFolderTarget) string {
	return "missing:" + url.QueryEscape(target.AccountID) + ":" + url.QueryEscape(target.AccountName) + ":" + url.QueryEscape(target.Path)
}

// ParseMissingFolderValue reads a MissingFolderValue; ok is false for any
// other value.
func ParseMissingFolderValue(value string) (target *models.MailRuleFolderTarget, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(value), "missing:")
	parts := strings.Split(rest, ":")
	if !found || len(parts) != 3 {
		return nil, false
	}
	var fields [3]string
	for i, part := range parts {
		unescaped, err := url.QueryUnescape(part)
		if err != nil {
			return nil, false
		}
		fields[i] = unescaped
	}
	if fields[2] == "" {
		return nil, false
	}
	return &models.MailRuleFolderTarget{AccountID: fields[0], AccountName: fields[1], Path: fields[2]}, true
}

// Missing reports whether something the rule names wasn't found.
func (r ImportRule) Missing() bool {
	return len(r.MissingAccounts) > 0 || len(r.MissingFolders) > 0
}

// PlanImport works out what importing a file would add: each rule with the
// accounts and folders it names found among the owner's (accounts, with
// their folders), and how it compares with the owner's rules (existing).
// An account is found by its address, Local folders by being Local
// folders, and a folder by its path in its account; nothing is guessed.
func PlanImport(file *File, accounts []models.MailRuleAccount, existing []models.MailRule) []ImportRule {
	fileAccounts := map[string]FileAccount{}
	found := map[string]*models.MailRuleAccount{}
	for _, fileAccount := range file.Accounts {
		fileAccounts[fileAccount.Ref] = fileAccount
		if account, ok := findFileAccount(fileAccount, accounts); ok {
			found[fileAccount.Ref] = account
		}
	}
	accountName := func(ref string) string {
		account := fileAccounts[ref]
		if email := strings.TrimSpace(account.Email); email != "" {
			return email
		}
		return strings.TrimSpace(account.Name)
	}
	plans := make([]ImportRule, 0, len(file.Rules))
	for index, fileRule := range file.Rules {
		plan := ImportRule{Index: index}
		rule := models.MailRule{
			Name: fileRule.Name, Enabled: fileRule.Enabled, MatchAll: true, StopProcessing: fileRule.StopProcessing,
			Tags: slices.Clone(fileRule.Tags), AccountsChosen: !fileRule.AllAccounts,
		}
		if rule.AccountsChosen {
			for _, ref := range fileRule.Accounts {
				if account := found[ref]; account != nil && account.Provider != providers.ProviderLocal {
					rule.AccountIDs = append(rule.AccountIDs, account.ID)
				} else if name := accountName(ref); !slices.Contains(plan.MissingAccounts, name) {
					plan.MissingAccounts = append(plan.MissingAccounts, name)
				}
			}
		}
		folder := func(exported *FileFolder) *models.MailRuleFolderTarget {
			if exported == nil {
				return nil
			}
			if exported.Role != "" {
				return &models.MailRuleFolderTarget{Role: exported.Role}
			}
			target := &models.MailRuleFolderTarget{Path: exported.Path}
			if account := found[exported.Account]; account != nil {
				target.AccountID = account.ID
				for _, choice := range account.Folders {
					if exported.Path != "" && fileFolderPath(choice) == exported.Path {
						target.FolderID = choice.ID
						return target
					}
				}
			}
			target.AccountName = accountName(exported.Account)
			missing := MissingFolder{Path: exported.Path, Account: target.AccountName}
			if !slices.Contains(plan.MissingFolders, missing) {
				plan.MissingFolders = append(plan.MissingFolders, missing)
			}
			return target
		}
		for _, condition := range fileRule.Conditions {
			rule.Conditions = append(rule.Conditions, models.MailRuleCondition{
				Group: condition.Group, Field: condition.Field, Operator: condition.Operator, Value: condition.Value, Folder: folder(condition.Folder),
			})
		}
		for _, action := range fileRule.Actions {
			rule.Actions = append(rule.Actions, models.MailRuleAction{Kind: action.Kind, Folder: folder(action.Folder), Label: action.Label, ForGood: action.ForGood})
		}
		if plan.Missing() {
			rule.Enabled = false
			plan.TurnsOn = fileRule.Enabled && len(plan.MissingAccounts) == 0 && everyMissingFolderInAccount(rule)
			if plan.TurnsOn {
				markTurnOnWhenFound(&rule)
			}
		}
		plan.Err = ValidateImported(&rule)
		if plan.Err == nil {
			plan.Err = checkImportedFolderConditions(rule)
		}
		plan.Rule = rule
		for _, current := range existing {
			if strings.EqualFold(strings.TrimSpace(current.Name), rule.Name) {
				plan.SameName = current.ID
				if sameRule(current, rule) {
					plan.Same = true
					break
				}
			}
		}
		plans = append(plans, plan)
	}
	return plans
}

// findFileAccount finds the account here that a file's account is.
func findFileAccount(fileAccount FileAccount, accounts []models.MailRuleAccount) (*models.MailRuleAccount, bool) {
	email := strings.ToLower(strings.TrimSpace(fileAccount.Email))
	for i := range accounts {
		account := &accounts[i]
		if fileAccount.Provider == providers.ProviderLocal {
			if account.Provider == providers.ProviderLocal {
				return account, true
			}
			continue
		}
		if email != "" && account.Provider != providers.ProviderLocal && strings.ToLower(strings.TrimSpace(account.Email)) == email {
			return account, true
		}
	}
	return nil, false
}

// missingTargets are the folders a rule names that an import didn't find.
func missingTargets(rule *models.MailRule) []*models.MailRuleFolderTarget {
	var targets []*models.MailRuleFolderTarget
	for _, condition := range rule.Conditions {
		if condition.Folder != nil && condition.Folder.Missing() {
			targets = append(targets, condition.Folder)
		}
	}
	for _, action := range rule.Actions {
		if action.Folder != nil && action.Folder.Missing() {
			targets = append(targets, action.Folder)
		}
	}
	return targets
}

// everyMissingFolderInAccount reports whether each folder a rule names that
// wasn't found is in an account that is here, where it can still be found.
func everyMissingFolderInAccount(rule models.MailRule) bool {
	for _, target := range missingTargets(&rule) {
		if target.AccountID == "" {
			return false
		}
	}
	return true
}

// markTurnOnWhenFound marks the folders a rule is missing to turn the rule
// on once they are found.
func markTurnOnWhenFound(rule *models.MailRule) {
	for _, target := range missingTargets(rule) {
		target.TurnOnWhenFound = true
	}
}

// checkImportedFolderConditions refuses a rule whose folder condition names
// a folder of an account the rule isn't for, as saving a rule does; a
// folder not found yet counts too, as it can be found later.
func checkImportedFolderConditions(rule models.MailRule) error {
	for _, condition := range rule.Conditions {
		if target := condition.Folder; target != nil && target.AccountID != "" && !rule.WatchesAccount(target.AccountID) {
			return errors.New("a folder condition can only name a folder of an account the rule is for")
		}
	}
	return nil
}

// sameRule reports whether two rules do the same: on the same accounts,
// with the same conditions, actions and categories, and stopping later
// rules alike. Their names, whether they are on, and their order don't
// count.
func sameRule(a, b models.MailRule) bool {
	return ruleKey(a) == ruleKey(b)
}

func ruleKey(rule models.MailRule) string {
	rule.Conditions = slices.Clone(rule.Conditions)
	GroupConditions(&rule)
	type target struct{ Role, FolderID, Path, AccountName string }
	key := func(t *models.MailRuleFolderTarget) *target {
		if t == nil {
			return nil
		}
		if t.FolderID != "" {
			return &target{FolderID: t.FolderID}
		}
		return &target{Role: t.Role, Path: t.Path, AccountName: strings.ToLower(t.AccountName)}
	}
	type condition struct {
		Group                  int
		Field, Operator, Value string
		Folder                 *target
	}
	type action struct {
		Kind, Label string
		Folder      *target
		ForGood     bool
	}
	accounts := slices.Clone(rule.AccountIDs)
	slices.Sort(accounts)
	tags := make([]string, 0, len(rule.Tags))
	for _, tag := range rule.Tags {
		tags = append(tags, strings.ToLower(CleanTag(tag)))
	}
	slices.Sort(tags)
	var conditions []condition
	for _, c := range rule.Conditions {
		value := strings.TrimSpace(c.Value)
		conditions = append(conditions, condition{c.Group, strings.TrimSpace(c.Field), strings.TrimSpace(c.Operator), value, key(c.Folder)})
	}
	var actions []action
	for _, a := range rule.Actions {
		actions = append(actions, action{strings.TrimSpace(a.Kind), strings.ToLower(strings.TrimSpace(a.Label)), key(a.Folder), a.ForGood})
	}
	encoded, _ := json.Marshal(struct {
		Chosen     bool
		Accounts   []string
		Stop       bool
		Tags       []string
		Conditions []condition
		Actions    []action
	}{rule.AccountsChosen, accounts, rule.StopProcessing, tags, conditions, actions})
	return string(encoded)
}

// UniqueName is name with a number after it, "Name (2)", when taken says
// another rule has it; the name is shortened to keep within MaxNameLength.
func UniqueName(name string, taken func(string) bool) string {
	name = strings.TrimSpace(name)
	if !taken(name) {
		return name
	}
	for n := 2; ; n++ {
		suffix := fmt.Sprintf(" (%d)", n)
		base := name
		for utf8.RuneCountInString(base)+utf8.RuneCountInString(suffix) > MaxNameLength {
			_, size := utf8.DecodeLastRuneInString(base)
			base = base[:len(base)-size]
		}
		if candidate := strings.TrimSpace(base) + suffix; !taken(candidate) {
			return candidate
		}
	}
}
