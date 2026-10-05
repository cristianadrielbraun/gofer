package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func outlookCalendarTeamsMode(ctx context.Context, token, id string) (string, error) {
	var remote struct {
		ID              string   `json:"id"`
		CanEdit         *bool    `json:"canEdit"`
		Providers       []string `json:"allowedOnlineMeetingProviders"`
		DefaultProvider string   `json:"defaultOnlineMeetingProvider"`
	}
	endpoint := outlookGraphBaseURL + "/me/calendars/" + url.PathEscape(id) + "?$select=id,canEdit,allowedOnlineMeetingProviders,defaultOnlineMeetingProvider"
	if err := calendarCreateJSON(ctx, http.MethodGet, endpoint, token, nil, &remote); err != nil {
		return "", err
	}
	if remote.ID != id {
		return "", fmt.Errorf("Microsoft returned a different calendar")
	}
	if remote.CanEdit == nil {
		return "", fmt.Errorf("Microsoft did not return calendar write capability")
	}
	if !*remote.CanEdit {
		return "unsupported", nil
	}
	// Graph exposes two capability fields. A Teams default is also an explicit
	// capability signal; do not reject it just because the allowed list is empty.
	if remote.DefaultProvider == "teamsForBusiness" {
		return "available", nil
	}
	for _, provider := range remote.Providers {
		if provider == "teamsForBusiness" {
			return "available", nil
		}
	}
	// Personal Outlook calendars can report unknown even though requesting an
	// online meeting without a business provider generates a consumer Teams link.
	// Offer this as best-effort, not as an advertised capability. Do not fall back
	// when Graph explicitly selects another business provider. Older personal
	// calendars can still advertise the legacy skypeForConsumer value.
	for _, provider := range append(remote.Providers, remote.DefaultProvider) {
		if !calendarConsumerMeetingProvider(provider) {
			return "unsupported", nil
		}
	}
	return "available-default", nil
}

const calendarTeamsUnavailableMessage = "Microsoft Graph does not advertise Teams creation for this calendar. Teams may still be available in Outlook."

func calendarOutlookTeamsJSON(raw json.RawMessage) bool {
	var meeting struct {
		Provider        string
		IsOnlineMeeting *bool
	}
	if json.Unmarshal(raw, &meeting) != nil || (meeting.IsOnlineMeeting != nil && !*meeting.IsOnlineMeeting) {
		return false
	}
	return meeting.Provider == "teamsForBusiness" || (calendarConsumerMeetingProvider(meeting.Provider) && calendarConsumerTeamsURL(calendar.MeetingJoinURL(string(raw))))
}

func calendarOutlookTeamsConfirmed(remote outlookCalendarEvent) bool {
	return calendarTeamsLinkConfirmed(calendarOutlookMeetingJSON(remote))
}

func calendarConsumerMeetingProvider(provider string) bool {
	return provider == "" || provider == "unknown" || provider == "skypeForConsumer"
}

func calendarConsumerTeamsURL(raw string) bool {
	return calendar.TeamsJoinURL(raw) != ""
}

func calendarTeamsLinkConfirmed(raw json.RawMessage) bool {
	return calendarOutlookTeamsJSON(raw) && calendar.MeetingJoinURL(string(raw)) != ""
}

// Microsoft appends its generated meeting information when enabling Teams.
// Check our submitted content without mistaking that addition for a bad save.
func calendarTeamsDescriptionConfirmed(actual string, draft calendar.EventDraft) bool {
	submitted := calendar.DescriptionHTML(draft.Description)
	if draft.DescriptionHTML != nil {
		submitted = *draft.DescriptionHTML
	}
	return submitted == "" || strings.Contains(calendar.DescriptionHTML(actual), submitted)
}

func (h *Handler) handleCalendarTeamsOptions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	userID := h.userID(ctx)
	sources, err := h.db.ListSelectedCalendarSources(ctx, userID)
	if err != nil {
		http.Error(w, "Could not check this calendar.", 500)
		return
	}
	var source storage.CalendarSource
	for _, candidate := range sources {
		if candidate.ID == r.URL.Query().Get("source_id") {
			source = candidate
			break
		}
	}
	if source.ID == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("X-Gofer-Calendar-Source", source.ID)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if source.Provider == "gmail" {
		h.renderCalendarGoogleMeetOptions(w, r.WithContext(ctx), source)
		return
	}
	if source.Provider != "outlook" {
		return
	}
	data := views.CalendarTeamsData{State: "unsupported", SourceID: source.ID, EventID: r.URL.Query().Get("event_id")}
	if eventID := r.URL.Query().Get("event_id"); eventID != "" {
		event, err := h.db.GetCalendarEvent(ctx, userID, eventID)
		if err != nil || event.SourceID != source.ID {
			http.NotFound(w, r)
			return
		}
		if _, reason, err := h.calendarEventEditAccess(ctx, event); err != nil || reason != "" {
			data.State = "unavailable"
			_ = views.CalendarTeamsOption(data).Render(ctx, w)
			return
		}
		if calendarStoredOutlookOnline(event) {
			data.State = "other-online"
			if calendarOutlookTeamsJSON(calendarOutlookCachedMeetingJSON(event)) {
				data.State = "existing"
			}
			_ = views.CalendarTeamsOption(data).Render(ctx, w)
			return
		}
		if calendarEventIsSeries(event) || event.SeriesRemoteID != "" {
			data.State = "recurring"
			_ = views.CalendarTeamsOption(data).Render(ctx, w)
			return
		}
	}
	if calendarSourceWritable(source) && (h.calendarWriteAuthorized(ctx, source) || h.calendarUpdateEvent != nil) {
		var supported bool
		if h.calendarTeamsSupported != nil {
			supported, err = h.calendarTeamsSupported(ctx, source)
		} else {
			credentials := h.calendarUpdateCredentials(ctx, source)
			err = credentials.err
			if err == nil {
				data.State, err = outlookCalendarTeamsMode(ctx, credentials.token, source.RemoteID)
			}
		}
		if err != nil {
			data.State = "error"
		} else if supported {
			data.State = "available"
		}
	} else {
		data.State = "read-only"
	}
	_ = views.CalendarTeamsOption(data).Render(ctx, w)
}
