package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleCalendarEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	eventID := strings.TrimSpace(r.PathValue("id"))
	if eventID == "" {
		http.Error(w, "event id required", http.StatusBadRequest)
		return
	}
	event, err := h.db.GetCalendarEvent(ctx, userID, eventID)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not load calendar event", http.StatusInternalServerError)
		return
	}
	details := views.CalendarEventDetails{
		Event:       calendarViewEvent(event),
		Description: calendarDescriptionText(event.Description),
		Organizer: views.CalendarEventParticipant{
			Name: strings.TrimSpace(event.OrganizerName), Email: strings.TrimSpace(event.OrganizerEmail),
		},
		Attendees: calendarEventParticipants(event.AttendeesJSON),
	}
	_, reason, accessErr := h.calendarEventEditAccess(ctx, event)
	details.CanEdit = accessErr == nil && reason == ""
	details.EditSeries = details.CanEdit && calendarEventIsSeries(event)
	details.EditUnavailableReason = reason
	if accessErr != nil {
		details.EditUnavailableReason = "Calendar write access could not be verified."
	}
	_, deleteReason, deleteErr := h.calendarEventDeleteAccess(ctx, event)
	details.CanDelete = deleteErr == nil && deleteReason == ""
	details.DeleteSeries = details.CanDelete && calendarEventIsSeries(event)
	if details.CanDelete && !details.DeleteSeries {
		details.DeleteVersion = event.ETag
	}
	details.HasOccurrence = event.SeriesRemoteID != "" && calendarOccurrenceExistingRestriction(event) == nil
	if details.HasOccurrence {
		details.DeleteVersion = event.ETag
	}
	if event.ResponseStatus != "organizer" && (event.ResponseStatus != "" || len(details.Attendees) > 0) {
		if source, err := h.calendarResponseAccess(ctx, event); err == nil {
			data := calendarResponseData(event)
			if source.Provider == "caldav" {
				data.Ready = false
			}
			details.Response = &data
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	location := viewsCalendarLocation(h.db.GetUISettings(ctx, userID))
	if err := views.CalendarEventDialog(details, location).Render(ctx, w); err != nil {
		http.Error(w, "could not render calendar event", http.StatusInternalServerError)
	}
}

var calendarDescriptionMarkup = regexp.MustCompile(`(?i)</?(?:html|head|body|div|p|br|span|a|ul|ol|li|table|tr|td|th|h[1-6]|b|strong|em|i|u|blockquote|pre|script|style|img)(?:\s[^>]*|/?)>`)

func calendarDescriptionText(description string) string {
	description = strings.TrimSpace(description)
	if calendarDescriptionMarkup.MatchString(description) {
		// Never insert provider HTML into the app. Reuse the mail reader's complete
		// visible-text extraction, which also removes scripts and tracking images.
		return strings.Join(strings.Fields(message.TextFromHTML([]byte(description))), " ")
	}
	return description
}

func calendarEventParticipants(raw string) []views.CalendarEventParticipant {
	// Attendee JSON is intentionally retained in each provider's wire format.
	// Normalize Google's, Graph's, and CalDAV's shapes only at the view boundary.
	var attendees []struct {
		Name, DisplayName, Email, ResponseStatus, Type, Role string
		Optional                                             bool
		EmailAddress                                         struct{ Name, Address string }
		Status                                               json.RawMessage
	}
	if err := json.Unmarshal([]byte(raw), &attendees); err != nil {
		return nil
	}
	result := make([]views.CalendarEventParticipant, 0, len(attendees))
	for _, attendee := range attendees {
		name, email := attendee.Name, attendee.Email
		if name == "" {
			name = attendee.DisplayName
		}
		if name == "" {
			name = attendee.EmailAddress.Name
		}
		if email == "" {
			email = attendee.EmailAddress.Address
		}
		name, email = strings.TrimSpace(name), strings.TrimSpace(email)
		if name == "" && email == "" {
			continue
		}
		status := attendee.ResponseStatus
		if status == "" {
			if err := json.Unmarshal(attendee.Status, &status); err != nil {
				var graphStatus struct{ Response string }
				_ = json.Unmarshal(attendee.Status, &graphStatus)
				status = graphStatus.Response
			}
		}
		result = append(result, views.CalendarEventParticipant{
			Name: name, Email: email, ResponseStatus: status,
			Optional: attendee.Optional || strings.EqualFold(attendee.Type, "optional") || strings.EqualFold(attendee.Role, "OPT-PARTICIPANT"),
		})
	}
	return result
}
