package views

import (
	"context"
	"fmt"

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

// mailRuleConditionFolders is the folder a condition tests: any account's
// mail can be tested, but Local folders get none.
func mailRuleConditionFolders(ctx context.Context, index int, target *models.MailRuleFolderTarget, accounts []models.MailRuleAccount) folderField {
	var watchable []models.MailRuleAccount
	for _, account := range accounts {
		if mailRuleWatchable(account) {
			watchable = append(watchable, account)
		}
	}
	f := mailRuleFolderField(ctx, fmt.Sprintf("mail-rule-cond-folder-%d", index), "cond_folder", watchable)
	f.Extras = mailRuleRoleChoices(ctx, "inbox", "archive", "trash", "junk", "sent", "drafts")
	return f.choose(ctx, mailRuleTargetValue(target))
}

// mailRuleMoveFolders is the folder a Move action moves mail to, never Sent
// or Drafts. A hidden account is offered only when the rule already moves
// mail there. Choosing draws the form again, to ask what happens to mail
// moved from another account.
func mailRuleMoveFolders(ctx context.Context, index int, target *models.MailRuleFolderTarget, accounts []models.MailRuleAccount) folderField {
	var offered []models.MailRuleAccount
	for _, account := range accounts {
		if !account.Hidden || mailRuleTargetIn(target, account) {
			offered = append(offered, account)
		}
	}
	f := mailRuleFolderField(ctx, fmt.Sprintf("mail-rule-act-folder-%d", index), "act_folder", offered)
	f.Extras = mailRuleRoleChoices(ctx, "inbox", "archive", "trash", "junk")
	f.Usable = func(folder models.Folder) bool { return folder.Role != "sent" && folder.Role != "drafts" }
	f.Makes = true
	f.Attrs = templ.Attributes{"data-mail-rule-rerender": ""}
	return f.choose(ctx, mailRuleTargetValue(target))
}

// mailRuleRunFolders is the folder a run by hand looks in: a folder every
// account has, in each account the rule watches, or any folder of one.
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
	f.Extras = mailRuleRunScopes(ctx, run.Rule, accounts)
	return f.choose(ctx, run.Scope)
}
