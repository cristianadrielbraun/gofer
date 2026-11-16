package views

import (
	"encoding/json"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/sharedmailbox"
)

// sidebarSharedMailbox is one shared mailbox an account can open, with its
// folders, as the sidebar shows it under the account.
type sidebarSharedMailbox struct {
	Email   string
	Name    string
	Folders []models.Folder
	Unread  int
}

// sidebarSplitSharedFolders separates an account's own folders from those of
// the shared mailboxes it can open, which come grouped by mailbox in the
// order the mailboxes are first listed.
func sidebarSplitSharedFolders(folders []models.Folder) ([]models.Folder, []sidebarSharedMailbox) {
	own := make([]models.Folder, 0, len(folders))
	var shared []sidebarSharedMailbox
	index := map[string]int{}
	for _, folder := range folders {
		if folder.SharedMailbox == "" {
			own = append(own, folder)
			continue
		}
		i, ok := index[folder.SharedMailbox]
		if !ok {
			i = len(shared)
			index[folder.SharedMailbox] = i
			shared = append(shared, sidebarSharedMailbox{
				Email: folder.SharedMailbox,
				Name:  sharedmailbox.MailboxName(folder.SharedMailbox, folder.SharedMailboxName, ""),
			})
		}
		shared[i].Folders = append(shared[i].Folders, folder)
		shared[i].Unread += sidebarSharedUnread(folder)
	}
	return own, shared
}

// sidebarSharedUnread counts the unread mail that a shared mailbox's entry
// shows for a folder and its subfolders: none for Sent, Drafts, Junk and
// Trash, where unread mail is not news.
func sidebarSharedUnread(folder models.Folder) int {
	n := 0
	switch folder.SharedRole {
	case "sent", "drafts", "junk", "trash":
	default:
		n = folder.Unread
	}
	for _, child := range folder.Children {
		n += sidebarSharedUnread(child)
	}
	return n
}

func sidebarSharedMailboxesUnread(mailboxes []sidebarSharedMailbox) int {
	n := 0
	for _, m := range mailboxes {
		n += m.Unread
	}
	return n
}

func sidebarSharedMailboxesGroupID(accountID string) string {
	return sidebarFolderGroupID(accountID, "shared-mailboxes")
}

func sidebarSharedMailboxGroupID(accountID, email string) string {
	return sidebarFolderGroupID(accountID, "shared-mailbox:"+strings.ToLower(email))
}

func sidebarSharedFoldersHaveActive(folders []models.Folder, activeFolder string) bool {
	for _, folder := range folders {
		if folderHasActiveID(folder, activeFolder) {
			return true
		}
	}
	return false
}

func sidebarSharedMailboxesHaveActive(mailboxes []sidebarSharedMailbox, activeFolder string) bool {
	for _, m := range mailboxes {
		if sidebarSharedFoldersHaveActive(m.Folders, activeFolder) {
			return true
		}
	}
	return false
}

// sidebarGroupCollapsed reads a remembered collapsed state of a sidebar
// group; a group with the open folder in it is always open.
func sidebarGroupCollapsed(settings map[string]string, groupID string, active bool) bool {
	if active {
		return false
	}
	return sidebarFolderCollapsedState(settings)[groupID]
}

func sidebarShowSharedAddress(settings map[string]string) bool {
	return sharedmailbox.SettingsFrom(settings).ShowAddress
}

func sidebarFolderCollapsedState(settings map[string]string) map[string]bool {
	var state map[string]bool
	if err := json.Unmarshal([]byte(uiSettingGet(settings, "sidebar_folder_collapsed", "{}")), &state); err != nil {
		return nil
	}
	return state
}

// accountOpensSharedMailboxes reports whether an account's menu offers its
// shared mailboxes: IMAP lists them in its shared namespaces and JMAP as
// further accounts of the login. Gmail, Outlook and DAV accounts have none
// that Gofer reads.
func accountOpensSharedMailboxes(account models.Account) bool {
	return account.EmailSyncEnabled && !account.IsDeleting && (account.Provider == "imap" || account.Provider == providers.ProviderJMAP)
}
