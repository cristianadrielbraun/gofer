package views

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cristianadrielbraun/gofer/internal/mailrules"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// mailRuleFields are the parts of a message a condition can test, in the
// order the form offers them.
var mailRuleFields = []struct{ value, label string }{
	{models.MailRuleFieldFrom, "From"},
	{models.MailRuleFieldTo, "To"},
	{models.MailRuleFieldCC, "Cc"},
	{models.MailRuleFieldRecipients, "To or Cc"},
	{models.MailRuleFieldSubject, "Subject"},
	{models.MailRuleFieldListID, "Mailing list"},
	{models.MailRuleFieldFolder, "Folder"},
}

// MailRuleTextOperators are the comparisons a condition on text can make.
var MailRuleTextOperators = []string{
	models.MailRuleOpContains, models.MailRuleOpNotContains, models.MailRuleOpIs, models.MailRuleOpIsNot,
	models.MailRuleOpStartsWith, models.MailRuleOpEndsWith, models.MailRuleOpMatches,
}

var mailRuleOperatorLabels = map[string]string{
	models.MailRuleOpContains:    "contains",
	models.MailRuleOpNotContains: "doesn't contain",
	models.MailRuleOpIs:          "is",
	models.MailRuleOpIsNot:       "isn't",
	models.MailRuleOpStartsWith:  "starts with",
	models.MailRuleOpEndsWith:    "ends with",
	models.MailRuleOpMatches:     "matches pattern",
}

// mailRuleActionKinds are the things a rule can do, in the order the form
// offers them.
var mailRuleActionKinds = []struct{ value, label string }{
	{models.MailRuleActionMove, "Move to folder"},
	{models.MailRuleActionArchive, "Archive"},
	{models.MailRuleActionTrash, "Move to Trash"},
	{models.MailRuleActionSpam, "Mark as spam"},
	{models.MailRuleActionMarkRead, "Mark as read"},
	{models.MailRuleActionStar, "Star"},
	{models.MailRuleActionLabel, "Add label"},
}

var mailRuleRoleLabels = map[string]string{
	"inbox": "Inbox", "archive": "Archive", "trash": "Trash", "junk": "Spam", "sent": "Sent", "drafts": "Drafts",
}

func mailRuleFieldLabel(field string) string {
	for _, f := range mailRuleFields {
		if f.value == field {
			return f.label
		}
	}
	return field
}

func mailRuleOperatorLabel(operator string) string {
	if label, ok := mailRuleOperatorLabels[operator]; ok {
		return label
	}
	return operator
}

func mailRuleActionLabel(kind string) string {
	for _, k := range mailRuleActionKinds {
		if k.value == kind {
			return k.label
		}
	}
	return kind
}

// mailRuleFolderValue is how the form names a folder choice.
func mailRuleFolderValue(target *models.MailRuleFolderTarget) string {
	switch {
	case target == nil:
		return ""
	case target.Role != "":
		return "role:" + target.Role
	case target.FolderID != "":
		return "id:" + target.FolderID
	}
	return ""
}

// mailRuleFolderName names a folder a rule points at.
func mailRuleFolderName(target *models.MailRuleFolderTarget, accounts []models.MailRuleAccount) string {
	if target == nil {
		return "a folder"
	}
	if target.Role != "" {
		return mailRuleRoleLabels[target.Role]
	}
	for _, account := range accounts {
		for _, folder := range account.Folders {
			if folder.ID == target.FolderID {
				return mailRuleFolderChoiceName(folder)
			}
		}
	}
	if target.Path != "" {
		return target.Path
	}
	return "a folder that is gone"
}

func mailRuleFolderChoiceName(folder models.MailRuleFolderChoice) string {
	if folder.Path != "" {
		return folder.Path
	}
	return folder.Name
}

// mailRuleOwnFolders are the folders of an account a rule can name by
// itself, beside the ones every account has.
func mailRuleOwnFolders(accounts []models.MailRuleAccount, accountID string) []models.MailRuleFolderChoice {
	var folders []models.MailRuleFolderChoice
	for _, account := range accounts {
		if account.ID != accountID {
			continue
		}
		for _, folder := range account.Folders {
			if _, common := mailRuleRoleLabels[folder.Role]; !common && folder.Role != "spam" {
				folders = append(folders, folder)
			}
		}
	}
	return folders
}

// mailRuleConditionText says what a condition tests, in words.
func mailRuleConditionText(condition models.MailRuleCondition, accounts []models.MailRuleAccount) string {
	if condition.Field == models.MailRuleFieldFolder {
		return "Folder " + mailRuleOperatorLabel(condition.Operator) + " " + mailRuleFolderName(condition.Folder, accounts)
	}
	return mailRuleFieldLabel(condition.Field) + " " + mailRuleOperatorLabel(condition.Operator) + " “" + condition.Value + "”"
}

// mailRuleRunSummary is the rule's summary in its run dialog.
func mailRuleRunSummary(rule models.MailRule, accounts []models.MailRuleAccount) string {
	return mailRuleListSummary(rule, accounts)
}

// mailRuleActionText says what an action does, in words.
func mailRuleActionText(action models.MailRuleAction, accounts []models.MailRuleAccount) string {
	switch action.Kind {
	case models.MailRuleActionMove:
		return "Move to " + mailRuleFolderName(action.Folder, accounts)
	case models.MailRuleActionLabel:
		return "Add label “" + action.Label + "”"
	}
	return mailRuleActionLabel(action.Kind)
}

type mailRuleIndexedCondition struct {
	index     int
	condition models.MailRuleCondition
}

// mailRuleConditionGroups splits a rule's conditions, which come grouped,
// into their groups, keeping each one's place in the form.
func mailRuleConditionGroups(rule models.MailRule) [][]mailRuleIndexedCondition {
	var groups [][]mailRuleIndexedCondition
	for i, condition := range rule.Conditions {
		if i == 0 || condition.Group != rule.Conditions[i-1].Group {
			groups = append(groups, nil)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], mailRuleIndexedCondition{i, condition})
	}
	return groups
}

// mailRuleCompactControl keeps a condition's fields low, so a rule with many
// conditions stays short.
const mailRuleCompactControl = "bg-background h-8 md:text-xs"

// mailRuleListSummary says in a line who a rule is for, how much it tests
// and what it does: "All accounts · 3 conditions in 2 groups · Archive".
func mailRuleListSummary(rule models.MailRule, accounts []models.MailRuleAccount) string {
	return mailRuleAccountName(rule.AccountID, accounts) + " · " + mailRuleConditionsCount(rule) + " · " + mailRuleActionsText(rule, accounts)
}

// mailRuleConditionsCount counts a rule's conditions, and its groups when
// it has more than one: "1 condition", "3 conditions in 2 groups".
func mailRuleConditionsCount(rule models.MailRule) string {
	count := fmt.Sprintf("%d conditions", len(rule.Conditions))
	if len(rule.Conditions) == 1 {
		count = "1 condition"
	}
	if groups := len(mailRuleConditionGroups(rule)); groups > 1 {
		count += fmt.Sprintf(" in %d groups", groups)
	}
	return count
}

func mailRuleActionsText(rule models.MailRule, accounts []models.MailRuleAccount) string {
	actions := make([]string, 0, len(rule.Actions))
	for _, action := range rule.Actions {
		actions = append(actions, mailRuleActionText(action, accounts))
	}
	return strings.Join(actions, ", ")
}

// mailRuleAccountName says which accounts a rule is for.
func mailRuleAccountName(accountID string, accounts []models.MailRuleAccount) string {
	if accountID == "" {
		return "All accounts"
	}
	for _, account := range accounts {
		if account.ID == accountID {
			return account.Email
		}
	}
	return "An account that is gone"
}

func mailRuleAccountColor(accountID string, accounts []models.MailRuleAccount) string {
	for _, account := range accounts {
		if account.ID == accountID {
			return account.Color
		}
	}
	return ""
}

// mailRuleRunScopes are the folders a rule can be run on: a role in every
// account it is for, then each account's own folders.
func mailRuleRunScopes(rule models.MailRule, accounts []models.MailRuleAccount) (roles []struct{ value, label string }, own []models.MailRuleAccount) {
	every := ""
	if rule.AccountID == "" && len(accounts) > 1 {
		every = " (every account)"
	}
	for _, role := range []string{"inbox", "archive", "junk", "trash"} {
		roles = append(roles, struct{ value, label string }{"role:" + role, mailRuleRoleLabels[role] + every})
	}
	for _, account := range accounts {
		if rule.AccountID != "" && account.ID != rule.AccountID {
			continue
		}
		if folders := mailRuleOwnFolders(accounts, account.ID); len(folders) > 0 {
			own = append(own, models.MailRuleAccount{ID: account.ID, Name: account.Name, Email: account.Email, Folders: folders})
		}
	}
	return roles, own
}

// MailRuleRunScopeName names the folder chosen for a run.
func MailRuleRunScopeName(scope string, accounts []models.MailRuleAccount) string {
	kind, rest, _ := strings.Cut(scope, ":")
	if kind == "role" {
		return mailRuleRoleLabels[rest]
	}
	return mailRuleFolderName(&models.MailRuleFolderTarget{FolderID: rest}, accounts)
}

// mailRuleCount writes a count with its thousands apart: 12,345.
func mailRuleCount(n int) string {
	digits := strconv.Itoa(n)
	if n < 0 || len(digits) <= 3 {
		return digits
	}
	var b strings.Builder
	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(digit)
	}
	return b.String()
}

// MailRuleMessages counts messages in words: "1 message", "2,500 messages".
func MailRuleMessages(n int) string {
	if n == 1 {
		return "1 message"
	}
	return mailRuleCount(n) + " messages"
}

// mailRuleMessagesWord is "message" or "messages", for a count shown
// apart.
func mailRuleMessagesWord(n int) string {
	if n == 1 {
		return "message"
	}
	return "messages"
}

// mailRuleRunScopeOpen reports whether the folder of a run can still be
// changed: not while it is changing mail.
func mailRuleRunScopeOpen(run *models.MailRuleRunData) bool {
	return run.State != models.MailRuleRunRunning
}

// mailRuleDialogWidth is the rule dialog's width: wide for a rule's form,
// narrower for a run, which only counts.
func mailRuleDialogWidth(data models.MailRulesData) string {
	if data.Run != nil {
		return "sm:max-w-xl"
	}
	return "sm:max-w-3xl"
}

// MailRuleRunDoneNotice says what a finished run by hand did.
func MailRuleRunDoneNotice(run models.MailRuleRunData) string {
	if run.Changed == 0 {
		return "Nothing changed: the messages had moved, or no longer needed the rule."
	}
	notice := "Changed " + MailRuleMessages(run.Changed) + " in " + run.ScopeName + "."
	if run.Unchanged > 0 {
		notice += " " + MailRuleMessages(run.Unchanged) + " had moved, or no longer needed it."
	}
	if run.Failed > 0 {
		notice += " " + MailRuleMessages(run.Failed) + " couldn't be changed; the rule's activity says why."
	}
	return notice
}

func mailRuleAccountTree(accounts []models.MailRuleAccount, accountID string) []models.Folder {
	for _, account := range accounts {
		if account.ID == accountID {
			return account.Tree
		}
	}
	return nil
}

// mailRuleTreeRole is the role a folder of the tree stands for in a rule,
// so that the rule follows the role should the folder change; empty for a
// folder without one.
func mailRuleTreeRole(folder models.Folder) string {
	role := folder.Role
	if role == "spam" {
		role = "junk"
	}
	if _, ok := mailRuleRoleLabels[role]; ok {
		return role
	}
	return ""
}

// mailRuleTreeItemPadding leaves room at a folder row's right for its
// checkmark, and for the new subfolder button beside it.
func mailRuleTreeItemPadding(subfolder bool) string {
	if subfolder {
		return "pr-16"
	}
	return "pr-8"
}

func mailRuleTreeValue(folder models.Folder) string {
	if role := mailRuleTreeRole(folder); role != "" {
		return "role:" + role
	}
	return "id:" + folder.ID
}

func mailRuleTreeSelected(folder models.Folder, target *models.MailRuleFolderTarget) bool {
	if target == nil {
		return false
	}
	if role := mailRuleTreeRole(folder); role != "" {
		return target.Role == role
	}
	return target.FolderID == folder.ID
}

// MailRuleActionOffered reports whether the action at index may become kind:
// mail goes to one place only, so once another action moves it, no further
// move is offered, and an action already there isn't offered twice. Labels
// can be added more than once.
func MailRuleActionOffered(actions []models.MailRuleAction, index int, kind string) bool {
	for i, other := range actions {
		if i == index {
			continue
		}
		if mailrules.MovesMessage(kind) && mailrules.MovesMessage(other.Kind) {
			return false
		}
		if kind != models.MailRuleActionLabel && other.Kind == kind {
			return false
		}
	}
	return true
}

// mailRuleAccountMakesFolders reports whether a rule's form can offer a
// new folder in the account: Gmail has labels instead.
func mailRuleAccountMakesFolders(accounts []models.MailRuleAccount, accountID string) bool {
	for _, account := range accounts {
		if account.ID == accountID {
			return account.Provider != "gmail"
		}
	}
	return false
}

func mailRuleCanAddCondition(rule *models.MailRule) bool {
	return len(rule.Conditions) < mailrules.MaxConditions
}

func mailRuleCanAddAction(rule *models.MailRule) bool {
	return len(rule.Actions) < mailrules.MaxActions
}

// mailRuleSentence starts a text of the activity with a capital.
func mailRuleSentence(text string) string {
	if text == "" {
		return text
	}
	first, size := utf8.DecodeRuneInString(text)
	return string(unicode.ToUpper(first)) + text[size:]
}

// mailRuleActivityDetail says, under an entry, which rule it was and what
// it did: "Newsletters · From news@example.com · Archived".
func mailRuleActivityDetail(run models.MailRuleRun) string {
	parts := []string{run.RuleName}
	if run.MessageID == 0 {
		if run.Trigger == models.MailRuleTriggerManual {
			parts = append(parts, "Run by hand")
		}
		return strings.Join(parts, " · ")
	}
	if run.FromEmail != "" {
		parts = append(parts, "From "+run.FromEmail)
	}
	if run.Trigger == models.MailRuleTriggerManual {
		parts = append(parts, "Run by hand")
	}
	if run.Detail != "" {
		parts = append(parts, mailRuleSentence(run.Detail))
	}
	return strings.Join(parts, " · ")
}

// mailRuleActivityTime says when an entry happened: the time today, the
// day this year, or the date.
func mailRuleActivityTime(at, now time.Time) string {
	now = now.In(at.Location())
	switch {
	case at.Year() == now.Year() && at.YearDay() == now.YearDay():
		return at.Format("15:04")
	case at.Year() == now.Year():
		return at.Format("Jan 2, 15:04")
	}
	return at.Format("Jan 2, 2006")
}

// mailRuleActivityMessageURL opens the message an entry names, in a folder
// it shows in now.
func mailRuleActivityMessageURL(run models.MailRuleRun) string {
	return "/folder/" + url.PathEscape(run.FolderID) + "/" + strconv.FormatInt(run.MessageID, 10)
}

// mailRuleActivityMorePath shows the activity with more entries, filtered
// as it is.
func mailRuleActivityMorePath(data models.MailRuleActivityData) string {
	query := url.Values{"limit": {strconv.Itoa(data.Limit + 50)}}
	if data.RuleID != "" {
		query.Set("rule", data.RuleID)
	}
	if data.FailedOnly {
		query.Set("failed", "1")
	}
	return mailRulePath("/activity/list?" + query.Encode())
}
