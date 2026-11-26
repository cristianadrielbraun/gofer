package models

import "time"

// MailRule acts on mail as it arrives: when its conditions hold for a
// message, its actions are applied to it, as if the owner had done so.
type MailRule struct {
	ID string `json:"id"`
	// AccountsChosen limits the rule to the mail of AccountIDs, which can be
	// none once the accounts chosen are gone. Otherwise the rule runs on
	// every account's mail.
	AccountsChosen bool     `json:"accounts_chosen,omitempty"`
	AccountIDs     []string `json:"account_ids,omitempty"`
	Name           string   `json:"name"`
	Enabled        bool     `json:"enabled"`
	// MatchAll is set on every rule saved with condition groups. A rule
	// from before them without it matched when any one condition held; it
	// is read as one group per condition.
	MatchAll bool `json:"match_all"`
	// StopProcessing keeps later rules from acting on a message this rule
	// matched.
	StopProcessing bool                `json:"stop_processing"`
	Conditions     []MailRuleCondition `json:"conditions"`
	Actions        []MailRuleAction    `json:"actions"`
	SortOrder      int                 `json:"sort_order"`
	CreatedAt      time.Time           `json:"created_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
	// Tags say what the rule is for, such as "finance", to find and change
	// rules together; they don't change how it runs. They are in
	// alphabetical order, each spelled as the owner's other rules spell it.
	Tags []string `json:"tags,omitempty"`
}

// MailRuleCondition tests one part of a message: Field compared with Value
// by Operator. A folder condition names its folder in Folder instead.
// Conditions in one Group must all hold; a rule matches a message when one
// of its groups does.
type MailRuleCondition struct {
	Group    int                   `json:"group"`
	Field    string                `json:"field"`
	Operator string                `json:"operator"`
	Value    string                `json:"value,omitempty"`
	Folder   *MailRuleFolderTarget `json:"folder,omitempty"`
}

// MailRuleAction is one thing a rule does to a message it matched. Folder is
// set for MailRuleActionMove, Label for MailRuleActionLabel.
type MailRuleAction struct {
	Kind   string                `json:"kind"`
	Folder *MailRuleFolderTarget `json:"folder,omitempty"`
	Label  string                `json:"label,omitempty"`
	// ForGood, for a move to a folder of another account, deletes the
	// message from its own account for good once it's copied, rather than
	// putting it in that account's Trash.
	ForGood bool `json:"for_good,omitempty"`
}

// MailRuleFolderTarget names a folder a rule moves mail to or tests for. A
// role, such as "inbox", stands for that folder of whichever account the
// message is in. Otherwise FolderID names one folder, and Path is its name
// when the rule was saved, which finds the folder again if its ID changed.
type MailRuleFolderTarget struct {
	Role     string `json:"role,omitempty"`
	FolderID string `json:"folder_id,omitempty"`
	Path     string `json:"path,omitempty"`
	// AccountID is the account of the folder FolderID names. Rules saved
	// before a rule could name another account's folder leave it empty: the
	// folder is then in the account the message is in.
	AccountID string `json:"account_id,omitempty"`
	// AccountName is set, with no FolderID, on a folder an imported rule
	// names that wasn't found here: it is the address, or the name, of the
	// account the folder was in, which may not be here either (AccountID is
	// then empty). Such a folder is shown by Path and AccountName until
	// another is chosen; the rule doesn't run until then.
	AccountName string `json:"account_name,omitempty"`
	// TurnOnWhenFound is set, with AccountName, on a folder an import didn't
	// find yet in an account that is here, of a rule that was on where it
	// was exported: once its account has a folder at Path, and the rule
	// names no other folder that wasn't found, the rule is turned on again.
	// An account a profile import adds has no folders until it syncs.
	TurnOnWhenFound bool `json:"turn_on_when_found,omitempty"`
}

// Missing reports whether the target names a folder that an import didn't
// find: one with neither a role nor an ID.
func (t MailRuleFolderTarget) Missing() bool {
	return t.Role == "" && t.FolderID == ""
}

// MissingFolders reports whether a condition or an action of the rule names
// a folder that an import didn't find, which keeps the rule from running.
func (r MailRule) MissingFolders() bool {
	for _, condition := range r.Conditions {
		if condition.Folder != nil && condition.Folder.Missing() {
			return true
		}
	}
	for _, action := range r.Actions {
		if action.Folder != nil && action.Folder.Missing() {
			return true
		}
	}
	return false
}

// WatchesAccount reports whether the rule runs on an account's mail.
func (r MailRule) WatchesAccount(accountID string) bool {
	if !r.AccountsChosen {
		return true
	}
	for _, id := range r.AccountIDs {
		if id == accountID {
			return true
		}
	}
	return false
}

// The parts of a message a condition can test.
const (
	MailRuleFieldFrom       = "from"
	MailRuleFieldTo         = "to"
	MailRuleFieldCC         = "cc"
	MailRuleFieldRecipients = "recipients"
	MailRuleFieldSubject    = "subject"
	MailRuleFieldListID     = "list_id"
	MailRuleFieldFolder     = "folder"
)

// The ways a condition compares. Folder conditions take only is and is_not.
const (
	MailRuleOpContains    = "contains"
	MailRuleOpNotContains = "not_contains"
	MailRuleOpIs          = "is"
	MailRuleOpIsNot       = "is_not"
	MailRuleOpStartsWith  = "starts_with"
	MailRuleOpEndsWith    = "ends_with"
	MailRuleOpMatches     = "matches"
)

// The things a rule can do. Trash moves to the Trash folder; nothing a rule
// does deletes mail for good.
const (
	MailRuleActionMove     = "move"
	MailRuleActionArchive  = "archive"
	MailRuleActionTrash    = "trash"
	MailRuleActionSpam     = "spam"
	MailRuleActionMarkRead = "mark_read"
	MailRuleActionStar     = "star"
	MailRuleActionLabel    = "label"
)

// The outcomes recorded in a rule's activity.
const (
	MailRuleRunApplied = "applied"
	MailRuleRunSkipped = "skipped"
	MailRuleRunFailed  = "failed"
)

// The ways a rule comes to act on a message.
const (
	MailRuleTriggerArrival = "arrival"
	MailRuleTriggerManual  = "manual"
)

// MailRuleRun records what a rule did to one message.
type MailRuleRun struct {
	ID        int64     `json:"id"`
	RuleID    string    `json:"rule_id"`
	RuleName  string    `json:"rule_name"`
	AccountID string    `json:"account_id"`
	MessageID int64     `json:"message_id"`
	Subject   string    `json:"subject"`
	FromEmail string    `json:"from_email"`
	Trigger   string    `json:"trigger"`
	Status    string    `json:"status"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
	// FolderID is a folder the message shows in now, to open it from; empty
	// when it is gone, or the entry names no message.
	FolderID string `json:"folder_id,omitempty"`
}

// MailRuleActivityData is what the rules' activity shows: entries newest
// first, as filtered.
type MailRuleActivityData struct {
	Runs  []MailRuleRun
	Rules []MailRule
	// RuleID shows one rule's entries only; FailedOnly those that failed.
	RuleID     string
	FailedOnly bool
	// Limit is how many entries are shown, and More whether there are others.
	Limit int
	More  bool
	Error string
}

// MailRulesData is what the Rules settings show: the owner's rules, and
// while one is added, edited or run, that rule.
type MailRulesData struct {
	Rules    []MailRule
	Accounts []MailRuleAccount
	// Editing is the rule in the form while one is added or edited.
	Editing *MailRule
	// Run is the rule being run on mail already there.
	Run    *MailRuleRunData
	Error  string
	Notice string
	// Destinations are the folders each rule moves mail into, by rule ID,
	// as storage's MailRuleDestinations finds them. Only the list has them.
	Destinations map[string][]string
	// FolderFilter is the folder whose rules the list shows: only those that
	// move mail into it, as a folder's menu in the sidebar asks.
	FolderFilter string
	// TagFilter are the tags (categories) the list shows the rules of: those
	// with any of them. It holds only tags some rule has.
	TagFilter []string
	// AccountFilter are the accounts the list shows the rules of: rules that
	// run on any of them, which a rule for every account does.
	AccountFilter []string
	// StatusFilter is "on" or "off" for the list to show only the rules
	// turned on, or off, and "" to show both.
	StatusFilter string
	// ActionFilter are the kinds of action the list shows the rules of:
	// rules that do any of them.
	ActionFilter []string
	// Tags are the tags of all the owner's rules, which the rule form
	// suggests (storage's ListMailRuleTags). People see them as categories.
	Tags []string
	// TagChange is the dialog that adds tags to the rules selected in the
	// list, or takes some off them.
	TagChange *MailRuleTagChange
	// Import is the dialog that imports rules from a file.
	Import *MailRuleImport
}

// MailRuleImport is the dialog that imports rules from a rules file: first
// the file to choose, then, once File holds it, what importing it adds.
type MailRuleImport struct {
	// File is the file chosen, sent back with what to do with each rule.
	File       string
	ExportedAt string
	Rules      []MailRuleImportRule
}

// MailRuleImportRule is one rule of a file being imported.
type MailRuleImportRule struct {
	// Index is the rule's place in the file.
	Index int
	// Rule is the rule as it would be added, turned off when something it
	// names wasn't found.
	Rule MailRule
	// MissingAccounts and MissingFolders are what it names that wasn't
	// found, accounts by address or name.
	MissingAccounts []string
	MissingFolders  []MailRuleMissingFolder
	// Problem says why the rule can't be imported, when it can't.
	Problem string
	// TurnsOn is set when the rule turns on again once the folders it is
	// missing are found in their accounts, which are here.
	TurnsOn bool
	// SameName is set when a rule here has the same name, and Same when it
	// also does the same. Choice is then what to do: skip the rule from the
	// file, or add it beside the other, under a name of its own.
	SameName bool
	Same     bool
	Choice   string
}

// What to do with a rule from a file when one here has its name.
const (
	MailRuleImportSkip = "skip"
	MailRuleImportAdd  = "add"
)

// MailRuleMissingFolder is a folder an imported rule names that wasn't
// found: its path, and the address or name of its account, when known.
type MailRuleMissingFolder struct {
	Path    string
	Account string
}

// MailRuleTagChange is the dialog that adds tags to the rules selected in
// the list, or with Remove set, takes some off them.
type MailRuleTagChange struct {
	Remove  bool
	RuleIDs []string
	// Tags are the tags typed to add, or chosen to take off.
	Tags []string
	// Present are the tags the selected rules have, with how many of them
	// have each, to choose from when taking tags off.
	Present []MailRuleTagCount
}

// MailRuleTagCount is a tag and how many of the selected rules have it.
type MailRuleTagCount struct {
	Tag   string
	Rules int
}

// MailRuleAccount is an account a rule can be limited to, with the folders
// of its own a rule can name.
type MailRuleAccount struct {
	ID       string
	Name     string
	Email    string
	Color    string
	Provider string
	// Hidden is set for an account the user hid, which isn't offered as a
	// place to move mail unless a rule already moves mail there.
	Hidden  bool
	Folders []MailRuleFolderChoice
	// Tree is the account's folders as the Folders dialog shows them.
	Tree []Folder
}

// MailRuleFolderChoice is a folder of one account.
type MailRuleFolderChoice struct {
	ID   string
	Name string
	Path string
	Role string
}

// MailRuleRunData is a rule being run by hand on the mail in a folder: first
// counting what it would change, then changing it.
type MailRuleRunData struct {
	Rule  MailRule
	JobID string
	// Scope is the folder chosen: "role:inbox", or "id:" and a folder ID;
	// ScopeName names it.
	Scope     string
	ScopeName string
	State     string
	// Total is how many messages the folder holds, Scanned how many were
	// looked at so far, and Matches how many the rule would change.
	Total   int
	Scanned int
	Matches int
	// Processed counts the matches gone through so far, of which Changed
	// were changed, Unchanged needed nothing anymore, and Failed failed.
	Processed int
	Changed   int
	Unchanged int
	Failed    int
	Error     string
	// Seq counts the run's changes, so a page can tell a newer report of
	// it from an older one.
	Seq int
}

// The states of a rule run by hand.
const (
	MailRuleRunCounting = "counting"
	MailRuleRunCounted  = "counted"
	MailRuleRunRunning  = "running"
	MailRuleRunDone     = "done"
	MailRuleRunStopped  = "stopped"
	MailRuleRunError    = "error"
)

// Finished reports whether a run by hand has ended.
func (d MailRuleRunData) Finished() bool {
	return d.State == MailRuleRunDone || d.State == MailRuleRunStopped || d.State == MailRuleRunError
}
