package views

import (
	"context"

	"github.com/a-h/templ"

	"github.com/cristianadrielbraun/gofer/components/icon"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// folderField is a form field for one folder: a button showing the folder
// chosen, which opens the folder picker in a dialog (folderFieldView).
// Picking a folder fills the field and closes the picker (folder-picker.js).
// It stands where a dropdown of folders was: in a rule, an import or a
// transfer, and the folder roles.
type folderField struct {
	// ID is the dialog's; Name the form field's, and Value what it sends.
	ID    string
	Name  string
	Value string
	// Label names the field; Title its picker.
	Label string
	Title string
	// Compact matches the smaller controls beside it in a rule or a list.
	Compact  bool
	Disabled bool
	// Attrs go on the form value, such as what redraws the form when it
	// changes.
	Attrs templ.Attributes
	// Extras are choices before the accounts. Under ExtrasTitle they are
	// searched like folders; without one they always show, as New folder
	// and Skip.
	Extras      []folderFieldExtra
	ExtrasTitle string
	Accounts    []models.Account
	// IDPrefix goes before a folder's ID in its value, as "id:" in rules.
	IDPrefix string
	// Usable says whether a folder can be chosen; nil allows them all.
	Usable func(models.Folder) bool
	// Makes offers new folders, in the accounts that have folders rather
	// than labels.
	Makes  bool
	Chosen folderFieldChosen
}

// folderFieldExtra is a choice that isn't one folder, such as a folder
// every account has, New folder or Automatic.
type folderFieldExtra struct {
	Value string
	Label string
	Icon  templ.Component
	// Note says, beside it in the field, what it stands for.
	Note string
}

// folderFieldChosen is what the field shows: an icon and name, the folders
// above it, and at the right its account, or a note.
type folderFieldChosen struct {
	Icon         templ.Component
	Name         string
	Parent       string
	Account      string
	AccountColor string
	Note         string
}

// Full is the whole place, for the tooltip when the field cuts it short.
func (c folderFieldChosen) Full() string {
	place := c.Name
	if c.Parent != "" {
		place = c.Parent + " / " + c.Name
	}
	switch {
	case c.Account != "":
		return c.Account + " · " + place
	case c.Note != "":
		return place + " · " + c.Note
	}
	return place
}

func (f folderField) usable(folder models.Folder) bool {
	return f.Usable == nil || f.Usable(folder)
}

func (f folderField) extraChosen(extra folderFieldExtra) folderFieldChosen {
	note := extra.Note
	if note == "" {
		note = f.ExtrasTitle
	}
	return folderFieldChosen{Icon: extra.Icon, Name: extra.Label, Note: note}
}

// entryChosen is how the field shows a folder. With one account to pick
// from, it doesn't name it.
func (f folderField) entryChosen(ctx context.Context, account models.Account, entry folderPickerEntry) folderFieldChosen {
	chosen := folderFieldChosen{Icon: FolderAppearanceIcon(entry.Folder), Name: i18n.FolderName(ctx, entry.Folder.Role, entry.Folder.Name), Parent: entry.Parent}
	if len(f.Accounts) > 1 {
		chosen.Account, chosen.AccountColor = accountTitle(account), account.Color
	}
	return chosen
}

// choose sets the field's value, or leaves it empty, asking for a folder
// again, when the value is nothing the picker offers any more.
func (f folderField) choose(ctx context.Context, value string) folderField {
	f.Value, f.Chosen = "", folderFieldChosen{}
	// An extra can stand for no folder, as Automatic does.
	for _, extra := range f.Extras {
		if extra.Value == value {
			f.Value, f.Chosen = value, f.extraChosen(extra)
			return f
		}
	}
	if value == "" {
		return f
	}
	for _, account := range f.Accounts {
		for _, entry := range folderPickerEntries(ctx, account, false) {
			if f.IDPrefix+entry.Folder.ID == value && f.usable(entry.Folder) {
				f.Value, f.Chosen = value, f.entryChosen(ctx, account, entry)
				return f
			}
		}
	}
	return f
}

// folderFieldButtonClass makes the field's button look like the dropdowns
// beside it, and cut its text short rather than grow.
func folderFieldButtonClass(f folderField) string {
	class := "flex w-full min-w-0 shrink overflow-hidden justify-between gap-2 rounded-md border border-input bg-background px-3 py-1 font-normal shadow-xs focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50"
	if f.Compact {
		return class + " h-8 md:text-xs"
	}
	return class + " h-9 text-base md:text-sm"
}

// mailFolderTargetField chooses where a set of messages goes, in an import
// or a transfer: one of the folders offered, a new folder, whose name the
// row asks for, or, with skip, nowhere.
func mailFolderTargetField(ctx context.Context, id, name, label string, account models.Account, offered []MailImportFolder, selected string, skip bool) folderField {
	usable := make(map[string]bool, len(offered))
	for _, folder := range offered {
		usable[folder.ID] = true
	}
	f := folderField{
		ID:       id,
		Name:     name,
		Label:    label,
		Title:    label,
		Compact:  true,
		Attrs:    templ.Attributes{"data-mail-import-target": ""},
		Accounts: []models.Account{account},
		Usable:   func(folder models.Folder) bool { return usable[folder.ID] },
		Extras:   []folderFieldExtra{{Value: "new", Label: i18n.T(ctx, "mail.folder_menu.new_folder"), Icon: icon.FolderPlus(icon.Props{Class: "size-4"})}},
	}
	if skip {
		f.Extras = append(f.Extras, folderFieldExtra{Value: "skip", Label: i18n.T(ctx, "transfer.import.skip"), Icon: icon.Ban(icon.Props{Class: "size-4"})})
	}
	return f.choose(ctx, selected)
}

// folderRoleField chooses the folder that has a role on an IMAP account, or
// Automatic, which leaves it to what the server says. Only the folders the
// server lets be opened can be chosen, never the Inbox.
func folderRoleField(ctx context.Context, data models.FolderRolesData, role models.FolderRole) folderField {
	usable := make(map[string]bool, len(data.Folders))
	for _, folder := range data.Folders {
		usable[folder.ID] = true
	}
	name := folderRoleName(ctx, role.Role)
	f := folderField{
		ID:       "folder-role-" + role.Role,
		Name:     "role_" + role.Role,
		Label:    name,
		Title:    name,
		Accounts: []models.Account{data.Account},
		Usable:   func(folder models.Folder) bool { return usable[folder.ID] },
		Extras:   []folderFieldExtra{{Value: "", Label: i18n.T(ctx, "settings.folder_roles.automatic"), Icon: icon.WandSparkles(icon.Props{Class: "size-4"})}},
	}
	return f.choose(ctx, role.Chosen)
}
