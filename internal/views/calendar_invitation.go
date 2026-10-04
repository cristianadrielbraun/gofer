package views

import "strings"

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

func calendarEventResponseLabel(event CalendarEvent) string {
	switch calendarEventResponseState(event) {
	case "pending":
		return "Awaiting response"
	case "tentative":
		return "Maybe"
	case "declined":
		return "Declined"
	case "accepted":
		return "Accepted"
	default:
		return ""
	}
}

func calendarEventResponseSummary(event CalendarEvent) string {
	summary := calendarEventSummary(event)
	if response := calendarEventResponseLabel(event); response != "" {
		summary += " · " + response
	}
	return summary
}
