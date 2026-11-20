package views

import "github.com/cristianadrielbraun/gofer/internal/models"

// sidebarFoldersForAccount applies local visibility preferences only to IMAP.
// The original account folder tree is not modified.
func sidebarFoldersForAccount(account models.Account) []models.Folder {
	if account.Provider != "imap" {
		return account.Folders
	}
	return filterSidebarFolders(account.Folders, false)
}

// A hidden custom folder suppresses ordinary descendants, but protected
// system folders are promoted to the nearest visible ancestor.
func filterSidebarFolders(folders []models.Folder, ancestorHidden bool) []models.Folder {
	result := make([]models.Folder, 0, len(folders))

	for _, folder := range folders {
		hidden := ancestorHidden || (folder.Role == "custom" && folder.Hidden)
		children := filterSidebarFolders(folder.Children, hidden)

		if folder.Role == "custom" && hidden {
			result = append(result, children...)
			continue
		}

		// Copy the folder before replacing its presentation-only children.
		folder.Children = children
		result = append(result, folder)
	}

	return result
}
