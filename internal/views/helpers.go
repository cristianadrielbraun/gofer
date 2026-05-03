package views

func folderDisplayName(folderID string) string {
	names := map[string]string{
		"inbox":   "Inbox",
		"starred": "Starred",
		"sent":    "Sent",
		"drafts":  "Drafts",
		"archive": "Archive",
		"spam":    "Spam",
		"trash":   "Trash",
	}
	if name, ok := names[folderID]; ok {
		return name
	}
	return "Inbox"
}
