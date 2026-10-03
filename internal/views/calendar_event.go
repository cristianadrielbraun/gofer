package views

import (
	"net/url"
	"strings"
	"time"
)

type CalendarEventDetails struct {
	Event                 CalendarEvent
	Description           string
	Organizer             CalendarEventParticipant
	Attendees             []CalendarEventParticipant
	CanEdit               bool
	EditSeries            bool
	EditUnavailableReason string
	CanDelete             bool
	DeleteSeries          bool
	DeleteSeriesID        string
	DeleteVersion         string
}

type CalendarEventParticipant struct {
	Name           string
	Email          string
	ResponseStatus string
	Optional       bool
}

func calendarEventDetailsURL(eventID string) string {
	return "/api/calendar/events/" + url.PathEscape(eventID)
}

func calendarEventEditURL(details CalendarEventDetails) string {
	endpoint := calendarEventDetailsURL(details.Event.ID) + "/edit"
	if details.EditSeries {
		endpoint += "?scope=series"
	}
	return endpoint
}

func calendarEventDetailsDate(event CalendarEvent, location *time.Location) string {
	var start, end time.Time
	if event.AllDay {
		var err error
		start, err = time.Parse("2006-01-02", event.StartDate)
		if err != nil {
			return event.StartDate
		}
		// Provider all-day end dates are exclusive; display the final occupied day.
		if exclusiveEnd, err := time.Parse("2006-01-02", event.EndDate); err == nil && exclusiveEnd.After(start) {
			end = exclusiveEnd.AddDate(0, 0, -1)
		}
	} else if event.StartAt != nil {
		start = event.StartAt.In(location)
		if event.EndAt != nil {
			end = event.EndAt.In(location)
		}
	}
	if start.IsZero() {
		return "Date unavailable"
	}
	if end.IsZero() || sameCalendarDate(start, end) {
		return start.Format("Monday, January 2, 2006")
	}
	return start.Format("Mon, Jan 2, 2006") + " – " + end.Format("Mon, Jan 2, 2006")
}

func calendarEventDetailsTimeZone(event CalendarEvent, location *time.Location) string {
	if event.AllDay || event.StartAt == nil {
		return ""
	}
	startZone, _ := event.StartAt.In(location).Zone()
	zone := startZone
	if event.EndAt != nil {
		if endZone, _ := event.EndAt.In(location).Zone(); endZone != startZone {
			zone += " → " + endZone
		}
	}
	if name := location.String(); name != "Local" && name != zone {
		return name + " (" + zone + ")"
	}
	return zone
}

func calendarParticipantName(person CalendarEventParticipant) string {
	if name := strings.TrimSpace(person.Name); name != "" {
		return name
	}
	return strings.TrimSpace(person.Email)
}

func calendarParticipantResponse(person CalendarEventParticipant) string {
	switch strings.ToLower(strings.TrimSpace(person.ResponseStatus)) {
	case "accepted":
		return "Accepted"
	case "declined":
		return "Declined"
	case "tentative", "tentativelyaccepted":
		return "Tentative"
	case "organizer":
		return "Organizer"
	case "delegated":
		return "Delegated"
	default:
		return "No response"
	}
}

func calendarParticipantResponseClass(person CalendarEventParticipant) string {
	base := "shrink-0 rounded-full px-2 py-0.5 text-[10px] font-semibold"
	switch calendarParticipantResponse(person) {
	case "Accepted", "Organizer":
		return base + " bg-emerald-500/10 text-emerald-700 dark:text-emerald-300"
	case "Declined":
		return base + " bg-destructive/10 text-destructive"
	case "Tentative":
		return base + " bg-amber-500/10 text-amber-700 dark:text-amber-300"
	default:
		return base + " bg-muted text-muted-foreground"
	}
}
