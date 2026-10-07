package handler

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// The same owned capability reader serves both legacy option URLs. Source and
// optional event IDs are resolved from the authenticated owner's private store.
func (h *Handler) handleUserCalendarMeetingOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["source_id"]) != 1 || query.Get("source_id") == "" || len(query["event_id"]) > 1 {
		http.Error(w, "Choose a calendar before checking meeting availability.", http.StatusBadRequest)
		return
	}
	for key := range query {
		if key != "source_id" && key != "event_id" {
			http.Error(w, "Invalid meeting availability request.", http.StatusBadRequest)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var data views.CalendarTeamsData
	var empty bool
	err = h.userIMAP.RunUserServiceWork(ctx, h.userID(ctx), func(ctx context.Context) error {
		sourceSnapshot, err := h.userAccounts.SnapshotCalendarSource(ctx, h.userID(ctx), query.Get("source_id"))
		if err != nil {
			return err
		}
		p := &userCalendarRequest{h: h, source: sourceSnapshot}
		source := sourceSnapshot.Source()
		if source.Provider != "gmail" && source.Provider != "outlook" {
			empty = true
			return p.validate(ctx)
		}
		data = views.CalendarTeamsData{State: "unsupported", SourceID: source.ID, EventID: query.Get("event_id")}
		if source.Provider == "gmail" {
			data.Provider, data.EventID = "gmail", ""
		}
		if eventID := query.Get("event_id"); eventID != "" {
			eventSnapshot, err := h.userAccounts.SnapshotCalendarEvent(ctx, h.userID(ctx), eventID)
			if err != nil {
				return err
			}
			if eventSnapshot.Source().ID != source.ID || eventSnapshot.Source().AccountID != source.AccountID {
				return userCalendarFormFailure{http.StatusNotFound, "This event is unavailable. Refresh the calendar."}
			}
			if err := p.validate(ctx); err != nil {
				return err
			}
			p = &userCalendarRequest{h: h, event: eventSnapshot}
			event, selected := eventSnapshot.Event(), eventSnapshot.Source()
			details := views.CalendarEventDetails{}
			userCalendarDetailAccess(&details, event, selected, h.userCalendarWriteAuthorized(ctx, selected))
			if !details.CanEdit {
				data.State = "unavailable"
			} else if selected.Provider == "gmail" && calendarStoredGoogleMeet(event) {
				data.State, data.JoinURL = "existing", calendar.MeetingJoinURL(event.OnlineMeetingJSON)
			} else if selected.Provider == "outlook" && calendarStoredOutlookOnline(event) {
				data.State = "other-online"
				if calendarOutlookTeamsJSON(calendarOutlookCachedMeetingJSON(event)) {
					data.State = "existing"
				}
			} else if calendarEventIsSeries(event) || event.SeriesRemoteID != "" {
				data.State = "recurring"
			}
			source = selected
		}
		if data.State == "unsupported" {
			if !calendarSourceWritable(source) || !h.userCalendarWriteAuthorized(ctx, source) {
				data.State = "read-only"
			} else {
				err := p.readMeetingCapability(ctx, &data)
				if err != nil {
					data.State = "error"
				}
			}
		}
		return p.validate(ctx)
	})
	if err != nil {
		userCalendarFormError(w, r, err)
		return
	}
	w.Header().Set("X-Gofer-Calendar-Source", query.Get("source_id"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if empty {
		return
	}
	if err := views.CalendarTeamsOption(data).Render(ctx, w); err != nil {
		http.Error(w, "Could not show meeting availability.", http.StatusInternalServerError)
	}
}

func (p *userCalendarRequest) readMeetingCapability(ctx context.Context, data *views.CalendarTeamsData) error {
	source := p.actionSource()
	return p.h.userIMAP.RunAccountService(ctx, source.UserID, source.AccountID, mail.AccountServiceCalendar, 8*time.Second, func(operation context.Context) error {
		unlock, err := p.h.lockCalendarCreate(operation, source)
		if err != nil {
			return err
		}
		defer unlock()
		operation, err = p.actionContext(operation, true)
		if err != nil {
			return err
		}
		if source.Provider == "gmail" {
			supported, err := googleCalendarMeetSupported(operation, p.token, source.RemoteID)
			if err != nil {
				return err
			}
			if supported {
				data.State = "available"
			}
		} else {
			data.State, err = outlookCalendarTeamsMode(operation, p.token, source.RemoteID)
			if err != nil {
				return err
			}
		}
		return p.validate(operation)
	})
}
