package views

import "github.com/cristianadrielbraun/gofer/internal/models"

// sidebarFoldersForAccount is the account's folders as the sidebar shows
// them, without the ones the user hid. The account's own tree is left as it
// is, for the Folders dialog and the folder pickers.
func sidebarFoldersForAccount(account models.Account) []models.Folder {
	return filterSidebarFolders(account.Folders, false)
}

// filterSidebarFolders leaves out hidden folders and the folders inside
// them. A special folder, which can't be hidden, still shows when its
// parent is hidden: it takes the hidden parent's place.
func filterSidebarFolders(folders []models.Folder, ancestorHidden bool) []models.Folder {
	result := make([]models.Folder, 0, len(folders))
	for _, folder := range folders {
		hidden := ancestorHidden || (folder.Role == "custom" && folder.Hidden)
		children := filterSidebarFolders(folder.Children, hidden)
		if folder.Role == "custom" && hidden {
			result = append(result, children...)
			continue
		}
		// folder is a copy, so its children can change here.
		folder.Children = children
		result = append(result, folder)
	}
	return result
}

// sidebarLabels is an account's tags, or the tags of all accounts, as the
// sidebar shows them: without the ones the user hid.
func sidebarLabels(labels []models.Label) []models.Label {
	shown := make([]models.Label, 0, len(labels))
	for _, label := range labels {
		if !label.Hidden {
			shown = append(shown, label)
		}
	}
	return shown
}
