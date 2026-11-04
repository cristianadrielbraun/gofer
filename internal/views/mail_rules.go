package views

import (
	"context"
	"github.com/a-h/templ"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/mailrules"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// mailRuleFields are the parts of a message a condition can test, in the
// order the form offers them.
// Each one's label is the message "rules.field.<value>".
var mailRuleFields = []string{
	models.MailRuleFieldFrom,
	models.MailRuleFieldTo,
	models.MailRuleFieldCC,
	models.MailRuleFieldRecipients,
	models.MailRuleFieldSubject,
	models.MailRuleFieldListID,
	models.MailRuleFieldFolder,
}

// MailRuleTextOperators are the comparisons a condition on text can make.
var MailRuleTextOperators = []string{
	models.MailRuleOpContains, models.MailRuleOpNotContains, models.MailRuleOpIs, models.MailRuleOpIsNot,
	models.MailRuleOpStartsWith, models.MailRuleOpEndsWith, models.MailRuleOpMatches,
}

// mailRuleActionKinds are the things a rule can do, in the order the form
// offers them. Each one's label is the message "rules.action.<kind>".
var mailRuleActionKinds = []string{
	models.MailRuleActionMove,
	models.MailRuleActionArchive,
	models.MailRuleActionTrash,
	models.MailRuleActionSpam,
	models.MailRuleActionMarkRead,
	models.MailRuleActionStar,
	models.MailRuleActionLabel,
}

// mailRuleRoleLabels holds the message naming each folder every account
// has.
var mailRuleRoleLabels = map[string]string{
	"inbox": "folder.inbox", "archive": "folder.archive", "trash": "folder.trash", "junk": "folder.spam", "sent": "folder.sent", "drafts": "folder.drafts",
}

func mailRuleRoleLabel(ctx context.Context, role string) string {
	if key, ok := mailRuleRoleLabels[role]; ok {
		return i18n.T(ctx, key)
	}
	return role
}

func mailRuleFieldLabel(ctx context.Context, field string) string {
	if slices.Contains(mailRuleFields, field) {
		return i18n.T(ctx, "rules.field."+field)
	}
	return field
}

func mailRuleOperatorLabel(ctx context.Context, operator string) string {
	if slices.Contains(MailRuleTextOperators, operator) {
		return i18n.T(ctx, "rules.op."+operator)
	}
	return operator
}

func mailRuleActionLabel(ctx context.Context, kind string) string {
	if slices.Contains(mailRuleActionKinds, kind) {
		return i18n.T(ctx, "rules.action."+kind)
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
func mailRuleFolderName(ctx context.Context, target *models.MailRuleFolderTarget, accounts []models.MailRuleAccount) string {
	if target == nil {
		return i18n.T(ctx, "rules.folder.unknown")
	}
	if target.Role != "" {
		return mailRuleRoleLabel(ctx, target.Role)
	}
	for _, account := range accounts {
		for _, folder := range account.Folders {
			if folder.ID == target.FolderID {
				return i18n.T(ctx, "rules.folder.in_account", "folder", mailRuleFolderChoiceName(folder), "account", mailRuleAccountTitle(account))
			}
		}
	}
	if target.Path != "" {
		return target.Path
	}
	return i18n.T(ctx, "rules.folder.gone")
}

// mailRuleAccountTitle names an account: its address, or its name when it
// has none, as Local folders.
func mailRuleAccountTitle(account models.MailRuleAccount) string {
	if account.Email != "" {
		return account.Email
	}
	return account.Name
}

// mailRuleWatchable reports whether a rule can run on an account's mail.
// Mail never arrives in Local folders, so rules only move mail there.
func mailRuleWatchable(account models.MailRuleAccount) bool {
	return account.Provider != "local"
}

// mailRuleAccountsValue is the account picker's value: "all", or the IDs
// of the accounts chosen.
func mailRuleAccountsValue(rule models.MailRule) string {
	if !rule.AccountsChosen {
		return "all"
	}
	return strings.Join(rule.AccountIDs, ",")
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

// mailRuleRunSummary is the rule's summary in its run dialog.
func mailRuleRunSummary(ctx context.Context, rule models.MailRule, accounts []models.MailRuleAccount) string {
	return mailRuleListSummary(ctx, rule, accounts)
}

// mailRuleActionText says what an action does, in words.
func mailRuleActionText(ctx context.Context, action models.MailRuleAction, accounts []models.MailRuleAccount) string {
	switch action.Kind {
	case models.MailRuleActionMove:
		return i18n.T(ctx, "rules.action_text.move", "folder", mailRuleFolderName(ctx, action.Folder, accounts))
	case models.MailRuleActionLabel:
		return i18n.T(ctx, "rules.action_text.label", "label", action.Label)
	}
	return mailRuleActionLabel(ctx, action.Kind)
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
func mailRuleListSummary(ctx context.Context, rule models.MailRule, accounts []models.MailRuleAccount) string {
	return mailRuleAccountName(ctx, rule, accounts) + " · " + mailRuleConditionsCount(ctx, rule) + " · " + mailRuleActionsText(ctx, rule, accounts)
}

// mailRuleConditionsCount counts a rule's conditions, and its groups when
// it has more than one: "1 condition", "3 conditions in 2 groups".
func mailRuleConditionsCount(ctx context.Context, rule models.MailRule) string {
	count := i18n.N(ctx, "rules.conditions", len(rule.Conditions))
	if groups := len(mailRuleConditionGroups(rule)); groups > 1 {
		return i18n.T(ctx, "rules.conditions_in_groups", "conditions", count, "groups", groups)
	}
	return count
}

func mailRuleActionsText(ctx context.Context, rule models.MailRule, accounts []models.MailRuleAccount) string {
	actions := make([]string, 0, len(rule.Actions))
	for _, action := range rule.Actions {
		actions = append(actions, mailRuleActionText(ctx, action, accounts))
	}
	return strings.Join(actions, ", ")
}

// mailRuleAccountName says which accounts a rule is for.
func mailRuleAccountName(ctx context.Context, rule models.MailRule, accounts []models.MailRuleAccount) string {
	if !rule.AccountsChosen {
		return i18n.T(ctx, "rules.accounts.all")
	}
	var names []string
	for _, account := range accounts {
		if slices.Contains(rule.AccountIDs, account.ID) {
			names = append(names, mailRuleAccountTitle(account))
		}
	}
	switch len(names) {
	case 0:
		return i18n.T(ctx, "rules.accounts.none")
	case 1:
		return names[0]
	case 2:
		return i18n.T(ctx, "rules.accounts.two", "first", names[0], "second", names[1])
	}
	return i18n.T(ctx, "rules.accounts.many", "count", len(names))
}

// mailRuleAccountColor is the color of the one account a rule is for, or
// "" when it is for more than one.
func mailRuleAccountColor(rule models.MailRule, accounts []models.MailRuleAccount) string {
	if !rule.AccountsChosen || len(rule.AccountIDs) != 1 {
		return ""
	}
	for _, account := range accounts {
		if account.ID == rule.AccountIDs[0] {
			return account.Color
		}
	}
	return ""
}

// mailRuleRunScopes are the folders a rule can be run on: a role in every
// account it is for, then each account's own folders.
func mailRuleRunScopes(ctx context.Context, rule models.MailRule, accounts []models.MailRuleAccount) (roles []struct{ value, label string }, own []models.MailRuleAccount) {
	watched := 0
	for _, account := range accounts {
		if mailRuleWatchable(account) && rule.WatchesAccount(account.ID) {
			watched++
		}
	}
	for _, role := range []string{"inbox", "archive", "junk", "trash"} {
		label := mailRuleRoleLabel(ctx, role)
		if watched > 1 {
			label = i18n.T(ctx, "rules.run.each_account", "folder", label)
		}
		roles = append(roles, struct{ value, label string }{"role:" + role, label})
	}
	for _, account := range accounts {
		if !mailRuleWatchable(account) || !rule.WatchesAccount(account.ID) {
			continue
		}
		if folders := mailRuleOwnFolders(accounts, account.ID); len(folders) > 0 {
			own = append(own, models.MailRuleAccount{ID: account.ID, Name: account.Name, Email: account.Email, Folders: folders})
		}
	}
	return roles, own
}

// MailRuleRunScopeName names the folder chosen for a run.
func MailRuleRunScopeName(ctx context.Context, scope string, accounts []models.MailRuleAccount) string {
	kind, rest, _ := strings.Cut(scope, ":")
	if kind == "role" {
		return mailRuleRoleLabel(ctx, rest)
	}
	return mailRuleFolderName(ctx, &models.MailRuleFolderTarget{FolderID: rest}, accounts)
}

// mailRuleCount writes a count with the language's digit grouping: 12,345.
func mailRuleCount(ctx context.Context, n int) string {
	return i18n.Number(ctx, n)
}

// MailRuleMessages counts messages in words: "1 message", "2,500 messages".
func MailRuleMessages(ctx context.Context, n int) string {
	return mailRuleCountOf(ctx, "rules.messages", n)
}

// mailRuleCountOf fills a counted message with n, written with the
// language's digit grouping.
func mailRuleCountOf(ctx context.Context, key string, n int, args ...any) string {
	return i18n.N(ctx, key, n, append([]any{"count", mailRuleCount(ctx, n)}, args...)...)
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
func MailRuleRunDoneNotice(ctx context.Context, run models.MailRuleRunData) string {
	if run.Changed == 0 {
		return i18n.T(ctx, "rules.run.done.nothing")
	}
	notice := mailRuleCountOf(ctx, "rules.run.done.changed", run.Changed, "scope", run.ScopeName)
	if run.Unchanged > 0 {
		notice += " " + mailRuleCountOf(ctx, "rules.run.done.unchanged", run.Unchanged)
	}
	if run.Failed > 0 {
		notice += " " + mailRuleCountOf(ctx, "rules.run.done.failed", run.Failed)
	}
	return notice
}

// mailRuleTreeItemPadding leaves room at a folder row's right for its
// checkmark, and for the new subfolder button beside it.
func mailRuleTreeItemPadding(subfolder bool) string {
	if subfolder {
		return "pr-16"
	}
	return "pr-8"
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

// mailRuleActivityRuleName names an entry's rule; a rule deleted before a
// run by hand finished has no name stored.
func mailRuleActivityRuleName(ctx context.Context, run models.MailRuleRun) string {
	if run.RuleName == "" {
		return i18n.T(ctx, "rules.activity.deleted_rule")
	}
	return run.RuleName
}

// mailRuleActivityDetail says, under an entry, which rule it was and what
// it did: "Newsletters · From news@example.com · Archived".
func mailRuleActivityDetail(ctx context.Context, run models.MailRuleRun) string {
	parts := []string{mailRuleActivityRuleName(ctx, run)}
	if run.MessageID == 0 {
		if run.Trigger == models.MailRuleTriggerManual {
			parts = append(parts, i18n.T(ctx, "rules.activity.by_hand"))
		}
		return strings.Join(parts, " · ")
	}
	if run.FromEmail != "" {
		parts = append(parts, i18n.T(ctx, "rules.activity.from", "email", run.FromEmail))
	}
	if run.Trigger == models.MailRuleTriggerManual {
		parts = append(parts, i18n.T(ctx, "rules.activity.by_hand"))
	}
	if run.Detail != "" {
		parts = append(parts, mailRuleSentence(mailrules.DetailText(ctx, run.Detail)))
	}
	return strings.Join(parts, " · ")
}

// mailRuleActivityTime says when an entry happened: the time today, the
// day this year, or the date.
func mailRuleActivityTime(ctx context.Context, at, now time.Time) string {
	now = now.In(at.Location())
	dates := i18n.Dates(ctx)
	switch {
	case at.Year() == now.Year() && at.YearDay() == now.YearDay():
		return dates.Time(at)
	case at.Year() == now.Year():
		return dates.ShortDateTime(at)
	}
	return dates.Date(at)
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

// mailRuleFolderTriggerAttributes names the folder picker for screen readers.
// A move's picker draws the form again when it changes, to offer what
// happens to mail moved from another account.
func mailRuleFolderTriggerAttributes(ctx context.Context, forMove bool) templ.Attributes {
	attributes := templ.Attributes{"aria-label": i18n.T(ctx, "rules.field.folder")}
	if forMove {
		attributes["data-mail-rule-rerender"] = ""
	}
	return attributes
}

// mailRuleForGoodValue is the form's value for whether mail moved from
// another account is deleted there for good.
func mailRuleForGoodValue(forGood bool) string {
	if forGood {
		return "1"
	}
	return "0"
}

// mailRuleTargetIn reports whether a rule's folder is one of the account's.
func mailRuleTargetIn(target *models.MailRuleFolderTarget, account models.MailRuleAccount) bool {
	if target == nil || target.FolderID == "" {
		return false
	}
	if target.AccountID == account.ID {
		return true
	}
	for _, folder := range account.Folders {
		if folder.ID == target.FolderID {
			return true
		}
	}
	return false
}
