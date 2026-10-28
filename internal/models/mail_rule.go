package models

import "time"

// MailRule acts on mail as it arrives: when its conditions hold for a
// message, its actions are applied to it, as if the owner had done so.
type MailRule struct {
	ID string `json:"id"`
	// AccountID limits the rule to one account. Empty means every account.
	AccountID string `json:"account_id,omitempty"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
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
}

// MailRuleFolderTarget names a folder a rule moves mail to or tests for. A
// role, such as "inbox", stands for that folder of whichever account the
// message is in. Otherwise FolderID names one folder, and Path is its name
// when the rule was saved, which finds the folder again if its ID changed.
type MailRuleFolderTarget struct {
	Role     string `json:"role,omitempty"`
	FolderID string `json:"folder_id,omitempty"`
	Path     string `json:"path,omitempty"`
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
}

// MailRuleAccount is an account a rule can be limited to, with the folders
// of its own a rule can name.
type MailRuleAccount struct {
	ID       string
	Name     string
	Email    string
	Color    string
	Provider string
	Folders  []MailRuleFolderChoice
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
