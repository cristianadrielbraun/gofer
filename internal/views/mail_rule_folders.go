package views

import (
	"context"
	"fmt"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// mailRuleFolderPicker is a rule's folder field: a button showing the
// folder chosen, which opens the folder picker in a dialog. The form sends
// the choice as before: "role:inbox" for that folder of each account, or
// "id:" and a folder's ID.
type mailRuleFolderPicker struct {
	// ID is the dialog's; Name the form field's.
	ID    string
	Name  string
	Value string
	// Label names the field; Title its picker.
	Label string
	Title string
	// Compact is the height of the controls beside it in a rule's form.
	Compact bool
	// Roles are the folders every account has, listed first under
	// RolesTitle; they stand for that folder of the message's account.
	Roles      []mailRuleRoleChoice
	RolesTitle string
	Accounts   []models.MailRuleAccount
	// OwnMail lets Sent and Drafts be chosen; Makes offers new folders, in
	// the accounts that have folders rather than labels.
	OwnMail  bool
	Makes    bool
	Rerender bool
	Disabled bool
	Chosen   mailRuleChosenFolder
}

type mailRuleRoleChoice struct {
	Value  string
	Label  string
	Folder models.Folder
}

// mailRuleChosenFolder is what the field shows: the folder's icon and
// name, the folders above it, and at the right its account, or for a
// folder every account has, a note saying so.
type mailRuleChosenFolder struct {
	Folder       models.Folder
	Name         string
	Parent       string
	Account      string
	AccountColor string
	Note         string
}

// Full is the whole place, for the tooltip when the field cuts it short.
func (c mailRuleChosenFolder) Full() string {
	if c.Account == "" {
		return c.Name + " · " + c.Note
	}
	if c.Parent == "" {
		return c.Account + " · " + c.Name
	}
	return c.Account + " · " + c.Parent + " / " + c.Name
}

var mailRuleRoleIcons = map[string]string{
	"inbox": "inbox", "archive": "archive", "trash": "trash", "junk": "alert-circle", "sent": "send", "drafts": "file",
}

func mailRuleRoleChoices(ctx context.Context, roles ...string) []mailRuleRoleChoice {
	choices := make([]mailRuleRoleChoice, 0, len(roles))
	for _, role := range roles {
		choices = append(choices, mailRuleRoleChoice{Value: "role:" + role, Label: mailRuleRoleLabel(ctx, role), Folder: models.Folder{Icon: mailRuleRoleIcons[role]}})
	}
	return choices
}

// mailRulePickerAccount is a rule's account as the folder picker takes it.
func mailRulePickerAccount(account models.MailRuleAccount) models.Account {
	return models.Account{ID: account.ID, Name: account.Name, Email: account.Email, Color: account.Color, Provider: account.Provider, Folders: account.Tree}
}

// mailRuleTargetValue is a folder target as the form sends it. A folder
// that isn't offered any more leaves the field empty, so the rule asks
// for one again.
func mailRuleTargetValue(ctx context.Context, target *models.MailRuleFolderTarget, roles []mailRuleRoleChoice, accounts []models.MailRuleAccount) string {
	if target == nil {
		return ""
	}
	if target.Role != "" {
		for _, role := range roles {
			if role.Value == "role:"+target.Role {
				return role.Value
			}
		}
		return ""
	}
	if _, ok := mailRuleFindFolder(ctx, "id:"+target.FolderID, accounts); ok {
		return "id:" + target.FolderID
	}
	return ""
}

func mailRuleFindFolder(ctx context.Context, value string, accounts []models.MailRuleAccount) (mailRuleChosenFolder, bool) {
	for _, account := range accounts {
		picker := mailRulePickerAccount(account)
		for _, entry := range folderPickerEntries(ctx, picker, false) {
			if "id:"+entry.Folder.ID == value {
				return mailRuleEntryChosen(ctx, picker, entry), true
			}
		}
	}
	return mailRuleChosenFolder{}, false
}

// mailRuleEntryChosen is how the field shows a folder of the picker.
func mailRuleEntryChosen(ctx context.Context, account models.Account, entry folderPickerEntry) mailRuleChosenFolder {
	return mailRuleChosenFolder{Folder: entry.Folder, Name: i18n.FolderName(ctx, entry.Folder.Role, entry.Folder.Name), Parent: entry.Parent, Account: accountTitle(account), AccountColor: account.Color}
}

func (p mailRuleFolderPicker) withChosen(ctx context.Context) mailRuleFolderPicker {
	for _, role := range p.Roles {
		if role.Value == p.Value {
			p.Chosen = mailRuleChosenFolder{Folder: role.Folder, Name: role.Label, Note: p.RolesTitle}
			return p
		}
	}
	p.Chosen, _ = mailRuleFindFolder(ctx, p.Value, p.Accounts)
	return p
}

// mailRuleConditionFolders is the folder a condition tests: any account's
// mail can be tested, but Local folders get none.
func mailRuleConditionFolders(ctx context.Context, index int, target *models.MailRuleFolderTarget, accounts []models.MailRuleAccount) mailRuleFolderPicker {
	var watchable []models.MailRuleAccount
	for _, account := range accounts {
		if mailRuleWatchable(account) {
			watchable = append(watchable, account)
		}
	}
	p := mailRuleFolderPicker{
		ID:         fmt.Sprintf("mail-rule-cond-folder-%d", index),
		Name:       "cond_folder",
		Label:      i18n.T(ctx, "rules.field.folder"),
		Title:      i18n.T(ctx, "transfer.choose_folder"),
		Compact:    true,
		Roles:      mailRuleRoleChoices(ctx, "inbox", "archive", "trash", "junk", "sent", "drafts"),
		RolesTitle: i18n.T(ctx, "rules.in_message_account"),
		Accounts:   watchable,
		OwnMail:    true,
	}
	p.Value = mailRuleTargetValue(ctx, target, p.Roles, p.Accounts)
	return p.withChosen(ctx)
}

// mailRuleMoveFolders is the folder a Move action moves mail to. A hidden
// account is offered only when the rule already moves mail there.
func mailRuleMoveFolders(ctx context.Context, index int, target *models.MailRuleFolderTarget, accounts []models.MailRuleAccount) mailRuleFolderPicker {
	var offered []models.MailRuleAccount
	for _, account := range accounts {
		if !account.Hidden || mailRuleTargetIn(target, account) {
			offered = append(offered, account)
		}
	}
	p := mailRuleFolderPicker{
		ID:         fmt.Sprintf("mail-rule-act-folder-%d", index),
		Name:       "act_folder",
		Label:      i18n.T(ctx, "rules.field.folder"),
		Title:      i18n.T(ctx, "transfer.choose_folder"),
		Compact:    true,
		Roles:      mailRuleRoleChoices(ctx, "inbox", "archive", "trash", "junk"),
		RolesTitle: i18n.T(ctx, "rules.in_message_account"),
		Accounts:   offered,
		Makes:      true,
		Rerender:   true,
	}
	p.Value = mailRuleTargetValue(ctx, target, p.Roles, p.Accounts)
	return p.withChosen(ctx)
}

// mailRuleRunFolders is the folder a run by hand looks in: a folder every
// account has, in each account the rule watches, or any folder of one.
func mailRuleRunFolders(ctx context.Context, run *models.MailRuleRunData, accounts []models.MailRuleAccount) mailRuleFolderPicker {
	p := mailRuleFolderPicker{
		ID:         "mail-rule-run-folder",
		Name:       "scope",
		Value:      run.Scope,
		Label:      i18n.T(ctx, "rules.run.scope_label"),
		Title:      i18n.T(ctx, "rules.run.scope_label"),
		Roles:      mailRuleRunScopes(ctx, run.Rule, accounts),
		RolesTitle: i18n.T(ctx, "rules.in_message_account"),
		OwnMail:    true,
		Disabled:   !mailRuleRunScopeOpen(run),
	}
	for _, account := range accounts {
		if mailRuleWatchable(account) && run.Rule.WatchesAccount(account.ID) {
			p.Accounts = append(p.Accounts, account)
		}
	}
	return p.withChosen(ctx)
}

// mailRuleFolderButtonClass makes the field's button look like the
// dropdowns beside it.
func mailRuleFolderButtonClass(p mailRuleFolderPicker) string {
	class := "flex w-full min-w-0 shrink overflow-hidden justify-between gap-2 rounded-md border border-input bg-background px-3 py-1 font-normal shadow-xs focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50"
	if p.Compact {
		return class + " h-8 md:text-xs"
	}
	return class + " h-9 text-base md:text-sm"
}
