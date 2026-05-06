package views

import (
	"encoding/json"
	"gofer.email/internal/models"
)

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

func composeDefaultEmail(accounts []models.Account) string {
	if len(accounts) > 0 {
		return accounts[0].Email
	}
	return ""
}

func composeDefaultName(accounts []models.Account) string {
	if len(accounts) > 0 {
		return accounts[0].Name
	}
	return ""
}

func uiSettingsJSON(settings map[string]string) string {
	b, _ := json.Marshal(settings)
	return string(b)
}

func uiSettingGet(settings map[string]string, key, fallback string) string {
	if v, ok := settings[key]; ok && v != "" {
		return v
	}
	return fallback
}

func themeClass(settings map[string]string) string {
	if uiSettingGet(settings, "theme", "dark") == "dark" {
		return "dark"
	}
	return ""
}

func themeStyle(settings map[string]string) string {
	return uiSettingGet(settings, "theme_style", "classic")
}
