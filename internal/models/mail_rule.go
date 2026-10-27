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
	// MatchAll makes every condition have to hold; otherwise any one does.
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
type MailRuleCondition struct {
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
}
