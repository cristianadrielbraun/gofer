package notifications

import (
	"context"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Keep this formatter in sync with newMailNotificationAppearance in app.js.
// Both write their text from the same js.notify.* messages, in ctx's
// language: the user's, for a push sent from the server.
func newMailNotificationAppearance(ctx context.Context, payload map[string]any, unreadCount int, settings map[string]string) (title, body, icon string) {
	title, body = newMailNotificationText(ctx, payload, unreadCount)
	switch settings["notification_title"] {
	case "sender":
		title, _ = newMailNotificationText(ctx, payload, 1)
	case "gofer":
		title = "Gofer"
	case "count":
		title = i18n.N(ctx, "js.notify.new_messages", unreadCount)
	default:
		if unreadCount > 1 {
			sender, _ := newMailNotificationText(ctx, payload, 1)
			title = i18n.T(ctx, "js.notify.title_more", "sender", sender, "count", unreadCount-1)
		}
	}
	switch settings["notification_content"] {
	case "hidden":
		body = ""
	case "preview":
		preview := i18n.Preview(ctx, strings.Join(strings.Fields(stringPayload(payload, "preview")), " "))
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

// newMailNotificationInitials are what the notification avatar shows when
// the sender has no picture; none when the Gofer logo is chosen.
func newMailNotificationInitials(payload map[string]any, settings map[string]string) string {
	if settings["notification_icon"] == "gofer" {
		return ""
	}
	sender := strings.TrimSpace(stringPayload(payload, "from_name"))
	if sender == "" {
		sender = strings.TrimSpace(stringPayload(payload, "from_email"))
	}
	if sender == "" {
		return ""
	}
	return storage.Initials(sender)
}

func notificationAppearanceExample(ctx context.Context) map[string]any {
	return map[string]any{
		"from_name":   "Alex Smith",
		"subject":     i18n.T(ctx, "js.notify.example_subject"),
		"preview":     i18n.T(ctx, "js.notify.example_preview"),
		"folder_name": i18n.T(ctx, "js.mail.inbox"),
		"avatar_url":  "/assets/notification-avatar.png",
	}
}
