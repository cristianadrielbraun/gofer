package models

// VirtualFolders lists views with fixed symbols, independent of provider folders.
func VirtualFolders() []Folder {
	return []Folder{
		{ID: "inbox", Name: "Inbox", Icon: "inbox", Role: "inbox", IsSystem: true, IsVirtual: true},
		{ID: "starred", Name: "Starred", Icon: "starred", Role: "starred", IsSystem: true, IsVirtual: true},
		{ID: "sent", Name: "Sent", Icon: "send", Role: "sent", IsSystem: true, IsVirtual: true},
		{ID: "drafts", Name: "Drafts", Icon: "file", Role: "drafts", IsSystem: true, IsVirtual: true},
		{ID: "archive", Name: "Archive", Icon: "archive", Role: "archive", IsSystem: true, IsVirtual: true},
		{ID: "spam", Name: "Spam", Icon: "alert-circle", Role: "spam", IsSystem: true, IsVirtual: true},
		{ID: "trash", Name: "Trash", Icon: "trash", Role: "trash", IsSystem: true, IsVirtual: true},
		{ID: "scheduled", Name: "Scheduled", Icon: "calendar-clock", Role: "scheduled", IsSystem: true, IsVirtual: true},
	}
}

func VirtualFolderColorKey(id string) string { return "virtual_folder_color:" + id }
func VirtualFolderStyleKey(id string) string { return "virtual_folder_style:" + id }

// VirtualFolderOrderKey keeps a unified folder's place among the unified
// folders the user put in order.
func VirtualFolderOrderKey(id string) string { return "virtual_folder_order:" + id }

// VirtualFolderAppearance resolves only known views and ignores invalid stored values.
func VirtualFolderAppearance(id string, settings map[string]string) (Folder, bool) {
	for _, folder := range VirtualFolders() {
		if folder.ID == id {
			color := settings[VirtualFolderColorKey(id)]
			if ValidFolderColor(color) {
				folder.Color = color
			}
			folder.IconStyle = FolderIconStyle(settings[VirtualFolderStyleKey(id)])
			return folder, true
		}
	}
	return Folder{}, false
}
