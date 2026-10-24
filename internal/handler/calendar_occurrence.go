package handler

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var errCalendarOccurrenceBoundary = errors.New("Outlook cannot move this occurrence to or past a neighboring occurrence's day. Choose a date between the previous and next occurrences.")

// An occurrence scope is explicit at the HTTP boundary. Never interpret a
// missing scope as permission to change either a series or one of its members.
func calendarOccurrenceScope(values []bool) bool { return len(values) == 1 && values[0] }

func calendarOccurrenceRestriction(event calendar.RemoteEvent, seriesID string) string {
	if seriesID == "" || event.SeriesRemoteID != seriesID || event.RemoteID == seriesID {
		return "The provider did not confirm the selected occurrence and its series."
	}
	// Expanded CalDAV instances retain their RECURRENCE-ID in this field.
	if calendarUpdateHasDetails(event.Recurrence) {
		var lines []string
		if json.Unmarshal(event.Recurrence, &lines) != nil || len(lines) != 1 || !strings.HasPrefix(lines[0], "RECURRENCE-ID:") {
			return "An individual occurrence cannot change the series' repeat settings."
		}
	}
	event.SeriesRemoteID, event.Recurrence = "", nil
	return calendarUpdateRestriction(event)
}

func calendarOccurrenceExistingRestriction(existing storage.CalendarEvent) error {
	if calendarStoredEditableOnline(existing) {
		existing.OnlineMeetingJSON = "{}" // A date keeps its series' Meet or Teams meeting.
	}
	if reason := calendarOccurrenceRestriction(calendar.RemoteEvent{
		RemoteID: existing.RemoteID, SeriesRemoteID: existing.SeriesRemoteID, Status: existing.Status,
		Deleted: existing.IsDeleted, Recurrence: json.RawMessage(existing.RecurrenceJSON),
		Attendees: json.RawMessage(existing.AttendeesJSON), OnlineMeeting: json.RawMessage(existing.OnlineMeetingJSON),
		ResponseStatus: calendarStoredOrganizerStatus(existing),
	}, existing.SeriesRemoteID); reason != "" {
		return calendarUpdateUnsupported(reason)
	}
	// CalDAV marks the organizer by address, not by a reply status.
	existing.SeriesRemoteID, existing.RecurrenceJSON = "", "[]"
	existing.ResponseStatus = calendarStoredOrganizerStatus(existing)
	return calendarUpdateExistingRestriction(existing)
}

func (remote googleCalendarUpdateEvent) occurrenceRestriction(seriesID string) string {
	if remote.Organizer.Self != nil && !*remote.Organizer.Self {
		return "Invitations cannot be edited in Gofer yet."
	}
	if seriesID == "" || remote.RecurringEventID != seriesID || remote.ID == seriesID || len(remote.Recurrence) != 0 || remote.OriginalStartTime == nil {
		return "Google did not return the selected occurrence and its series."
	}
	original := remote.OriginalStartTime
	if original.Date != "" {
		if _, err := time.Parse("2006-01-02", original.Date); err != nil || original.DateTime != "" {
			return "Google did not return a valid occurrence identity."
		}
	} else if _, err := time.Parse(time.RFC3339Nano, original.DateTime); err != nil {
		return "Google did not return a valid occurrence identity."
	}
	remote.RecurringEventID, remote.OriginalStartTime = "", nil
	return remote.restriction()
}

func (remote outlookCalendarUpdateEvent) occurrenceRestriction(seriesID string) string {
	if remote.IsOrganizer != nil && !*remote.IsOrganizer {
		return "Invitations cannot be edited in Gofer yet."
	}
	if seriesID == "" || remote.SeriesMasterID != seriesID || remote.ID == seriesID || (remote.Type != "occurrence" && remote.Type != "exception") || calendarUpdateHasDetails(remote.Recurrence) {
		return "Microsoft did not return the selected occurrence and its series."
	}
	remote.SeriesMasterID, remote.Type = "", "singleInstance"
	return remote.restriction()
}

// A CalDAV server lists a series' dates in UTC, without the series' own time
// zone. Editing one date shows it in the series' zone, read from the series.
func calendarOccurrenceNeedsSeriesZone(event storage.CalendarEvent, source storage.CalendarSource) bool {
	return storage.CalendarSourceMailServer(source.Provider) && !event.AllDay && (event.StartTimeZone == "" || event.StartTimeZone == "UTC")
}

func calendarOccurrenceUseSeriesZone(event *storage.CalendarEvent, master storage.CalendarEvent) {
	if master.StartTimeZone == "" || master.StartTimeZone == "UTC" {
		return
	}
	if _, err := time.LoadLocation(master.StartTimeZone); err == nil {
		event.StartTimeZone, event.EndTimeZone = master.StartTimeZone, master.StartTimeZone
	}
}
