package views

import (
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type CalendarEventDetails struct {
	Event                 CalendarEvent
	Description           string
	DescriptionHTML       string
	JoinURL               string
	Organizer             CalendarEventParticipant
	Attendees             []CalendarEventParticipant
	CanEdit               bool
	EditSeries            bool
	HasOccurrence         bool
	EditUnavailableReason string
	CanDelete             bool
	DeleteSeries          bool
	DeleteOccurrence      bool
	DeleteSeriesID        string
	DeleteVersion         string
	Response              *CalendarResponseData
	Delivery              *CalendarDeliveryData
}

type CalendarDeliveryData struct {
	EventID, Error, EmptyNote string
	HasEmailDelivery          bool
	Rows                      []CalendarDeliveryRow
}

type CalendarDeliveryRow struct {
	ID, Label, Recipient, State, Note string
	CanRetry                          bool
}

type CalendarResponseData struct {
	EventID, Status, Version, Scope string
	HasOccurrence                   bool
	Ready                           bool
	Delivery                        string
}

type CalendarReplyData struct {
	ID, EventID, Scope, State, SendStatus, Note string
}

func calendarResponseLabel(status string) string {
	switch status {
	case "accepted":
		return "Accepted"
	case "tentative":
		return "Maybe"
	case "declined":
		return "Declined"
	default:
		return "Not answered"
	}
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

// calendarParticipantBadgeClass colors the small status disc on a guest's avatar.
func calendarParticipantBadgeClass(person CalendarEventParticipant) string {
	base := "absolute -right-0.5 -bottom-0.5 flex size-3.5 items-center justify-center rounded-full text-[9px] font-bold leading-none ring-2 ring-card"
	switch calendarParticipantResponse(person) {
	case "Accepted", "Organizer":
		return base + " bg-emerald-600 text-white dark:bg-emerald-500"
	case "Declined":
		return base + " bg-destructive text-white"
	case "Tentative":
		return base + " bg-amber-500 text-white"
	default:
		return base + " bg-muted-foreground/60 text-card"
	}
}

func calendarParticipantInitials(person CalendarEventParticipant) string {
	name := strings.TrimSpace(person.Name)
	if name == "" {
		name, _, _ = strings.Cut(strings.TrimSpace(person.Email), "@")
	}
	var initials []rune
	for _, word := range strings.FieldsFunc(name, func(r rune) bool { return r == ' ' || r == '.' || r == '_' || r == '-' }) {
		if r := []rune(word); len(r) > 0 {
			initials = append(initials, unicode.ToUpper(r[0]))
		}
		if len(initials) == 2 {
			break
		}
	}
	if len(initials) == 0 {
		return "?"
	}
	return string(initials)
}

func calendarSameParticipant(a, b CalendarEventParticipant) bool {
	if a.Email != "" || b.Email != "" {
		return strings.EqualFold(strings.TrimSpace(a.Email), strings.TrimSpace(b.Email))
	}
	return calendarParticipantName(a) != "" && calendarParticipantName(a) == calendarParticipantName(b)
}

// calendarOrganizerListed reports whether the organizer already appears in the
// guest list, where they are tagged instead of repeated in a separate row.
func calendarOrganizerListed(details CalendarEventDetails) bool {
	for _, person := range details.Attendees {
		if calendarSameParticipant(person, details.Organizer) {
			return true
		}
	}
	return false
}

// calendarGuestSummary tallies responses, e.g. "2 yes · 1 maybe · 1 awaiting".
func calendarGuestSummary(attendees []CalendarEventParticipant) string {
	counts := map[string]int{}
	for _, person := range attendees {
		counts[calendarParticipantResponse(person)]++
	}
	var parts []string
	for _, tally := range [][2]string{{"Accepted", "yes"}, {"Organizer", "yes"}, {"Tentative", "maybe"}, {"Declined", "no"}, {"Delegated", "delegated"}, {"No response", "awaiting"}} {
		if n := counts[tally[0]]; n > 0 {
			if tally[0] == "Organizer" && counts["Accepted"] > 0 {
				continue
			}
			if tally[0] == "Accepted" {
				n += counts["Organizer"]
			}
			parts = append(parts, strconv.Itoa(n)+" "+tally[1])
		}
	}
	return strings.Join(parts, " · ")
}
