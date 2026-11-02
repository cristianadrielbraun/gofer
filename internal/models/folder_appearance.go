package models

// ValidFolderColor allows an inherited color or an opaque six-digit CSS hex color.
func ValidFolderColor(color string) bool {
	if color == "" {
		return true
	}
	if len(color) != 7 || color[0] != '#' {
		return false
	}
	for _, c := range color[1:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// FolderIconChoice is a symbol available for a custom folder.
type FolderIconChoice struct {
	Name string
	Icon string
}

// FolderIconChoices is shared by the picker and server-side validation.
var FolderIconChoices = [...]FolderIconChoice{
	{Name: "Work", Icon: "briefcase-business"},
	{Name: "Home", Icon: "house"},
	{Name: "People", Icon: "users"},
	{Name: "Heart", Icon: "heart"},
	{Name: "Bookmark", Icon: "bookmark"},
	{Name: "Flag", Icon: "flag"},
	{Name: "Tag", Icon: "tag"},
	{Name: "Document", Icon: "file-text"},
	{Name: "Education", Icon: "graduation-cap"},
	{Name: "Wallet", Icon: "wallet"},
	{Name: "Receipt", Icon: "receipt"},
	{Name: "Shopping", Icon: "shopping-bag"},
	{Name: "Travel", Icon: "plane"},
	{Name: "Camera", Icon: "camera"},
	{Name: "Code", Icon: "code-xml"},
	{Name: "Tools", Icon: "wrench"},
	{Name: "Ideas", Icon: "lightbulb"},
}

// ValidFolderIcon allows the provider's default or one of the curated symbols.
func ValidFolderIcon(name string) bool {
	if name == "" {
		return true
	}
	for _, choice := range FolderIconChoices {
		if choice.Icon == name {
			return true
		}
	}
	return false
}

// CustomFolderIcon ignores overrides when a folder gains a reserved role.
func CustomFolderIcon(folder Folder) string {
	if folder.Role == "custom" && ValidFolderIcon(folder.IconOverride) {
		return folder.IconOverride
	}
	return ""
}

// ValidFolderIconStyle accepts the two available presentations of a symbol.
func ValidFolderIconStyle(style string) bool {
	return style == "outlined" || style == "filled"
}

// FolderIconStyle gives older folders and invalid preferences the default style.
func FolderIconStyle(style string) string {
	if style == "filled" {
		return style
	}
	return "outlined"
}
