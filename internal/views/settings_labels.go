package views

import (
	"context"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/themebuilder"
)

// The labels below name the saved value of a settings picker, in the page's
// language.

func composeViewOptionLabel(ctx context.Context, view string) string {
	switch view {
	case "pane":
		return i18n.T(ctx, "settings.compose.view.pane")
	case "full":
		return i18n.T(ctx, "settings.compose.view.full")
	default:
		return i18n.T(ctx, "settings.compose.view.dialog")
	}
}

func autosaveConditionsLabel(ctx context.Context, settings map[string]string) string {
	value := uiSettingGet(settings, "compose_autosave_conditions", "chars,attachment")
	count := 0
	for _, part := range strings.Split(value, ",") {
		if strings.TrimSpace(part) != "" {
			count++
		}
	}
	if count == 0 {
		return i18n.T(ctx, "settings.compose.autosave.no_conditions")
	}
	return i18n.N(ctx, "settings.compose.autosave.conditions", count)
}

func autosaveDebounceLabel(ctx context.Context, value string) string {
	seconds := 5
	switch value {
	case "1", "3", "10", "15":
		seconds, _ = strconv.Atoi(value)
	}
	return i18n.N(ctx, "settings.compose.autosave.seconds", seconds)
}

func listNavigationLabel(ctx context.Context, value string) string {
	if mailListNavigationMode(value) == "pagination" {
		return i18n.T(ctx, "settings.list.navigation.pagination")
	}
	return i18n.T(ctx, "settings.list.navigation.infinite")
}

func senderDisplayLabel(ctx context.Context, mode string) string {
	switch mode {
	case "email":
		return i18n.T(ctx, "settings.list.sender.email")
	case "both":
		return i18n.T(ctx, "settings.list.sender.both")
	default:
		return i18n.T(ctx, "settings.list.sender.name")
	}
}

func autoMarkReadLabel(ctx context.Context, value string) string {
	switch value {
	case "2", "5", "10":
		seconds, _ := strconv.Atoi(value)
		return i18n.N(ctx, "settings.list.mark_read.after", seconds)
	case "never":
		return i18n.T(ctx, "settings.list.mark_read.never")
	default:
		return i18n.T(ctx, "settings.list.mark_read.immediately")
	}
}

func threadOrderLabel(ctx context.Context, value string) string {
	if value == "oldest" {
		return i18n.T(ctx, "settings.list.thread_order.oldest")
	}
	return i18n.T(ctx, "settings.list.thread_order.newest")
}

func translationProviderLabel(ctx context.Context, value string) string {
	if translationProviderSettingValue(value) == "google_web_basic" || strings.TrimSpace(value) == "" {
		return i18n.T(ctx, "settings.translation.google_basic")
	}
	return strings.TrimSpace(value)
}

func bodyStyleLabel(ctx context.Context, value string) string {
	switch value {
	case "dark", "light", "original":
		return i18n.T(ctx, "mail.body_style."+value)
	}
	return i18n.T(ctx, "settings.messages.style.theme")
}

func paneLayoutLabel(ctx context.Context, value string) string {
	switch mailPaneLayout(value) {
	case "stacked":
		return i18n.T(ctx, "settings.panes.stacked")
	case "list":
		return i18n.T(ctx, "settings.panes.list")
	}
	return i18n.T(ctx, "settings.panes.side")
}

func titleUnreadCountLabel(ctx context.Context, scope string) string {
	switch scope {
	case "all":
		return i18n.T(ctx, "settings.title_unread.all")
	case "off":
		return i18n.T(ctx, "settings.title_unread.off")
	default:
		return i18n.T(ctx, "settings.title_unread.inboxes")
	}
}

func notificationModeLabel(ctx context.Context, mode string) string {
	switch mode {
	case "web_push":
		return i18n.T(ctx, "settings.notifications.mode.web_push")
	case "browser_tab":
		return i18n.T(ctx, "settings.notifications.mode.browser_tab")
	case "off":
		return i18n.T(ctx, "settings.notifications.mode.off")
	default:
		return i18n.T(ctx, "settings.notifications.mode.auto")
	}
}

func notificationAppearanceLabel(ctx context.Context, field, value string) string {
	switch field {
	case "title":
		switch value {
		case "sender", "gofer", "count":
			return i18n.T(ctx, "settings.notifications.title."+value)
		default:
			return i18n.T(ctx, "settings.notifications.title.auto")
		}
	case "content":
		switch value {
		case "preview", "hidden":
			return i18n.T(ctx, "settings.notifications.content."+value)
		default:
			return i18n.T(ctx, "settings.notifications.content.subject")
		}
	case "icon":
		if value == "gofer" {
			return i18n.T(ctx, "settings.notifications.icon.gofer")
		}
		return i18n.T(ctx, "settings.notifications.icon.avatar")
	}
	return ""
}

// settingsJoinAnd joins names as "A, B and C" in the page's language.
func settingsJoinAnd(ctx context.Context, items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return i18n.T(ctx, "settings.list_and", "first", settingsJoinComma(ctx, items[:len(items)-1]), "last", items[len(items)-1])
}

// settingsJoinComma joins names as the "A, B" of "A, B and C", with the
// page language's comma: Chinese writes "A、B".
func settingsJoinComma(ctx context.Context, items []string) string {
	if len(items) == 0 {
		return ""
	}
	joined := items[0]
	for _, item := range items[1:] {
		joined = i18n.T(ctx, "settings.list_comma", "first", joined, "next", item)
	}
	return joined
}

// folderRoleName names a special folder role, such as "junk", in the page's
// language.
func folderRoleName(ctx context.Context, role string) string {
	switch role {
	case "sent", "drafts", "trash", "archive":
		return i18n.T(ctx, "folder."+role)
	case "junk":
		return i18n.T(ctx, "folder.spam")
	}
	return role
}

// themeStyleName is a style's name as Settings shows it.
func themeStyleName(ctx context.Context, style string) string {
	switch style {
	case "minimal":
		return i18n.T(ctx, "settings.appearance.minimal")
	case "express":
		return i18n.T(ctx, "settings.appearance.express")
	case themebuilder.StyleName:
		return i18n.T(ctx, "settings.appearance.custom")
	default:
		return i18n.T(ctx, "settings.appearance.classic")
	}
}

// themeColorGroupName names a group of the color editor, such as "Reading
// pane", in the page's language.
func themeColorGroupName(ctx context.Context, label string) string {
	return i18n.T(ctx, "settings.theme.group."+strings.ReplaceAll(strings.ToLower(label), " ", "_"))
}

// signatureLabel names the chosen signature, or says there is none.
func signatureLabel(ctx context.Context, signatures []models.Signature, id string) string {
	for _, sig := range signatures {
		if sig.ID == id {
			return sig.Name
		}
	}
	return i18n.T(ctx, "settings.signatures.none")
}

func calendarSourceLabel(ctx context.Context, source models.CalendarSource) string {
	if name := strings.TrimSpace(source.Name); name != "" {
		return name
	}
	if source.IsPrimary {
		return i18n.T(ctx, "settings.calendar_sync.primary_calendar")
	}
	return i18n.T(ctx, "settings.calendar_sync.untitled_calendar")
}

func calendarSourceDetailText(ctx context.Context, source models.CalendarSource) string {
	details := make([]string, 0, 2)
	if timeZone := strings.TrimSpace(source.TimeZone); timeZone != "" {
		details = append(details, timeZone)
	}
	if accessRole := strings.TrimSpace(source.AccessRole); accessRole != "" {
		switch role := strings.ToLower(accessRole); role {
		case "owner", "writer", "reader", "freebusyreader":
			details = append(details, i18n.T(ctx, "settings.calendar_sync.access."+role))
		default:
			details = append(details, accessRole)
		}
	}
	return strings.Join(details, " · ")
}
