package views

import (
	"context"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

// A missing response is unknown, not an unanswered invitation. In particular,
// an organizer's event must not inherit any guest's response styling.
func calendarEventResponseState(event CalendarEvent) string {
	switch strings.ToLower(strings.TrimSpace(event.ResponseStatus)) {
	case "needsaction", "needs-action", "notresponded":
		return "pending"
	case "tentative", "tentativelyaccepted":
		return "tentative"
	case "accepted", "declined":
		return strings.ToLower(strings.TrimSpace(event.ResponseStatus))
	default:
		return ""
	}
}

func calendarEventResponseLabel(ctx context.Context, event CalendarEvent) string {
	switch calendarEventResponseState(event) {
	case "pending":
		return i18n.T(ctx, "calendar.response.awaiting")
	case "tentative":
		return i18n.T(ctx, "calendar.response.maybe")
	case "declined":
		return i18n.T(ctx, "calendar.response.declined")
	case "accepted":
		return i18n.T(ctx, "calendar.response.accepted")
	default:
		return ""
	}
}

func calendarEventResponseSummary(ctx context.Context, event CalendarEvent) string {
	summary := calendarEventSummary(ctx, event)
	if response := calendarEventResponseLabel(ctx, event); response != "" {
		summary += " · " + response
	}
	return summary
}
