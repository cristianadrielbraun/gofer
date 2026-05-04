package views

import "gofer.email/internal/models"

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

func composeDefaultAccountID(accounts []models.Account) string {
	if len(accounts) > 0 {
		return accounts[0].ID
	}
	return ""
}
