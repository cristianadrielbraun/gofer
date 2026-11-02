package notifications

import (
	"strconv"
	"strings"
)

// Keep this formatter in sync with newMailNotificationAppearance in app.js.
func newMailNotificationAppearance(payload map[string]any, unreadCount int, settings map[string]string) (title, body, icon string) {
	title, body = newMailNotificationText(payload, unreadCount)
	switch settings["notification_title"] {
	case "sender":
		title, _ = newMailNotificationText(payload, 1)
	case "gofer":
		title = "Gofer"
	case "count":
		if unreadCount == 1 {
			title = "1 new message"
		} else {
			title, _ = newMailNotificationText(payload, unreadCount)
		}
	default:
		if unreadCount > 1 {
			title, _ = newMailNotificationText(payload, 1)
			title += " · +" + strconv.Itoa(unreadCount-1) + " more"
		}
	}
	switch settings["notification_content"] {
	case "hidden":
		body = ""
	case "preview":
		preview := strings.Join(strings.Fields(stringPayload(payload, "preview")), " ")
		if chars := []rune(preview); len(chars) > 180 {
			preview = string(chars[:180]) + "…"
		}
		if preview != "" {
			body += "\n" + preview
		}
	}
	icon = "/assets/logo.png"
	if settings["notification_icon"] != "gofer" {
		if senderIcon := newMailNotificationIcon(payload); senderIcon != "" {
			icon = senderIcon
		}
	}
	return title, body, icon
}

func notificationAppearanceExample() map[string]any {
	return map[string]any{"from_name": "Alex Smith", "subject": "Tomorrow's meeting", "preview": "Hi! Shall we meet at 10 tomorrow?", "folder_name": "Inbox", "avatar_url": "/assets/notification-avatar.png"}
}
