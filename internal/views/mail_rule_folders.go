package views

import (
	"context"
	"fmt"
	"strings"

	"github.com/a-h/templ"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// A rule's folder fields (folderField) send their choice as before:
// "role:inbox" for that folder of the message's account, or "id:" and a
// folder's ID. The folders every account has come first.

var mailRuleRoleIcons = map[string]string{
	"inbox": "inbox", "archive": "archive", "trash": "trash", "junk": "alert-circle", "sent": "send", "drafts": "file",
}

func mailRuleRoleChoices(ctx context.Context, roles ...string) []folderFieldExtra {
	choices := make([]folderFieldExtra, 0, len(roles))
	for _, role := range roles {
		choices = append(choices, folderFieldExtra{Value: "role:" + role, Label: mailRuleRoleLabel(ctx, role), Icon: FolderAppearanceIcon(models.Folder{Icon: mailRuleRoleIcons[role]})})
	}
	return choices
}

// mailRuleSingleAccount is the one account a rule is for, when it is for
// just one that gets mail. The folders every account has would then repeat
// that account's own, so its pickers list only the account's folders.
func mailRuleSingleAccount(rule models.MailRule, accounts []models.MailRuleAccount) (models.MailRuleAccount, bool) {
	var single models.MailRuleAccount
	if !rule.AccountsChosen {
		return single, false
	}
	watched := 0
	for _, account := range accounts {
		if mailRuleWatchable(account) && rule.WatchesAccount(account.ID) {
			single = account
			watched++
		}
	}
	return single, watched == 1
}

// mailRuleOwnValue is how a rule for one account shows a folder every
// account has, such as "role:inbox": as that account's own folder, the one
// the rule finds for it when it runs (storage's findFolder: the lowest ID
// when several share the role, and Spam named either way). Any other value,
// or one the account has no folder for, stays as it is.
func mailRuleOwnValue(account models.MailRuleAccount, value string) string {
	role, ok := strings.CutPrefix(value, "role:")
	if !ok || role == "" {
		return value
	}
	roles := []string{role}
	if role == "junk" {
		roles = append(roles, "spam")
	}
	for _, role := range roles {
		found := ""
		for _, folder := range account.Folders {
			if folder.Role == role && (found == "" || folder.ID < found) {
				found = folder.ID
			}
		}
		if found != "" {
			return "id:" + found
		}
	}
	return value
}

// mailRuleChooseFolder fills a rule's folder field with value. The folders
// every account has (shared) come first, except for a rule on one account,
// where they would repeat that account's own: the field then lists only
// folders, and shows such a choice as the account's own folder. One the
// account has no folder for keeps them, so it still shows.
func mailRuleChooseFolder(ctx context.Context, f folderField, rule models.MailRule, accounts []models.MailRuleAccount, value string, shared []folderFieldExtra) folderField {
	if account, single := mailRuleSingleAccount(rule, accounts); single {
		if value = mailRuleOwnValue(account, value); !strings.HasPrefix(value, "role:") {
			return f.choose(ctx, value)
		}
	}
	f.Extras = shared
	return f.choose(ctx, value)
}

// mailRuleMovesWithinAccount reports whether a Move action keeps mail in
// its own account: the rule is for one account and the folder is that
// account's. What happens to mail from another account doesn't come up.
func mailRuleMovesWithinAccount(rule models.MailRule, target *models.MailRuleFolderTarget, accounts []models.MailRuleAccount) bool {
	account, single := mailRuleSingleAccount(rule, accounts)
	return single && mailRuleTargetIn(target, account)
}

// mailRulePickerAccounts are a rule's accounts as the folder picker takes
// them.
func mailRulePickerAccounts(accounts []models.MailRuleAccount) []models.Account {
	picked := make([]models.Account, 0, len(accounts))
	for _, account := range accounts {
		picked = append(picked, models.Account{ID: account.ID, Name: account.Name, Email: account.Email, Color: account.Color, Provider: account.Provider, Folders: account.Tree})
	}
	return picked
}

// mailRuleTargetValue is a folder target as the form sends it.
func mailRuleTargetValue(target *models.MailRuleFolderTarget) string {
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

func mailRuleFolderField(ctx context.Context, id, name string, accounts []models.MailRuleAccount) folderField {
	return folderField{
		ID:          id,
		Name:        name,
		Label:       i18n.T(ctx, "rules.field.folder"),
		Title:       i18n.T(ctx, "transfer.choose_folder"),
		Compact:     true,
		ExtrasTitle: i18n.T(ctx, "rules.in_message_account"),
		Accounts:    mailRulePickerAccounts(accounts),
		IDPrefix:    "id:",
	}
}

// mailRuleConditionFolders is the folder a condition tests. A rule only
// sees the mail of the accounts it is for, so only their folders are
// offered, and saving refuses any other (storage.ErrMailRuleFolder). Local
// folders get no mail and are never offered. An account the rule is no
// longer for stays while the condition names one of its folders: the field
// keeps showing that folder, rather than dropping it unseen, and saving
// says to choose another.
func mailRuleConditionFolders(ctx context.Context, index int, rule models.MailRule, target *models.MailRuleFolderTarget, accounts []models.MailRuleAccount) folderField {
	var watched []models.MailRuleAccount
	for _, account := range accounts {
		if mailRuleWatchable(account) && (rule.WatchesAccount(account.ID) || mailRuleTargetIn(target, account)) {
			watched = append(watched, account)
		}
	}
	f := mailRuleFolderField(ctx, fmt.Sprintf("mail-rule-cond-folder-%d", index), "cond_folder", watched)
	return mailRuleChooseFolder(ctx, f, rule, accounts, mailRuleTargetValue(target), mailRuleRoleChoices(ctx, "inbox", "archive", "trash", "junk", "sent", "drafts"))
}

// mailRuleMoveFolders is the folder a Move action moves mail to, never Sent
// or Drafts. Every account is offered, since mail can move to another
// account, but a hidden one only when the rule already moves mail there.
// Choosing draws the form again, to ask what happens to mail moved from
// another account.
func mailRuleMoveFolders(ctx context.Context, index int, rule models.MailRule, target *models.MailRuleFolderTarget, accounts []models.MailRuleAccount) folderField {
	var offered []models.MailRuleAccount
	for _, account := range accounts {
		if !account.Hidden || mailRuleTargetIn(target, account) {
			offered = append(offered, account)
		}
	}
	f := mailRuleFolderField(ctx, fmt.Sprintf("mail-rule-act-folder-%d", index), "act_folder", offered)
	f.Usable = func(folder models.Folder) bool { return folder.Role != "sent" && folder.Role != "drafts" }
	f.Makes = true
	f.Attrs = templ.Attributes{"data-mail-rule-rerender": ""}
	return mailRuleChooseFolder(ctx, f, rule, accounts, mailRuleTargetValue(target), mailRuleRoleChoices(ctx, "inbox", "archive", "trash", "junk"))
}

// mailRuleRunFolders is the folder a run by hand looks in: a folder every
// account has, in each account the rule watches, or any folder of one. A
// rule for one account lists only that account's folders, and a folder
// every account has, such as the Inbox a run starts with, shows as that
// account's own.
func mailRuleRunFolders(ctx context.Context, run *models.MailRuleRunData, accounts []models.MailRuleAccount) folderField {
	var watched []models.MailRuleAccount
	for _, account := range accounts {
		if mailRuleWatchable(account) && run.Rule.WatchesAccount(account.ID) {
			watched = append(watched, account)
		}
	}
	f := mailRuleFolderField(ctx, "mail-rule-run-folder", "scope", watched)
	f.Label = i18n.T(ctx, "rules.run.scope_label")
	f.Title = f.Label
	f.Compact = false
	f.Disabled = !mailRuleRunScopeOpen(run)
	return mailRuleChooseFolder(ctx, f, run.Rule, accounts, run.Scope, mailRuleRunScopes(ctx, run.Rule, accounts))
}
