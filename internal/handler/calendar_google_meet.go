package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const calendarGoogleMeetUnavailableMessage = "Google Meet creation is not available for this calendar."

func googleCalendarMeetSupported(ctx context.Context, token, id string) (bool, error) {
	var remote struct {
		ID                   string `json:"id"`
		AccessRole           string `json:"accessRole"`
		ConferenceProperties struct {
			Allowed []string `json:"allowedConferenceSolutionTypes"`
		} `json:"conferenceProperties"`
	}
	endpoint := googleCalendarAPIBaseURL + "/users/me/calendarList/" + url.PathEscape(id)
	if err := calendarCreateJSON(ctx, http.MethodGet, endpoint, token, nil, &remote); err != nil {
		return false, err
	}
	if remote.ID != id || remote.AccessRole == "" {
		return false, fmt.Errorf("Google did not confirm this calendar's identity and write capability")
	}
	if remote.AccessRole != "owner" && remote.AccessRole != "writer" {
		return false, nil
	}
	for _, allowed := range remote.ConferenceProperties.Allowed {
		if allowed == "hangoutsMeet" {
			return true, nil
		}
	}
	return false, nil
}

func calendarGoogleMeetRequest(requestID string) map[string]any {
	return map[string]any{"createRequest": map[string]any{
		"requestId":             requestID,
		"conferenceSolutionKey": map[string]string{"type": "hangoutsMeet"},
	}}
}

func calendarGoogleMeetJSON(raw json.RawMessage) bool {
	var meeting struct {
		ConferenceSolution struct{ Key struct{ Type string } }
		CreateRequest      struct {
			ConferenceSolutionKey struct{ Type string }
		}
	}
	if json.Unmarshal(raw, &meeting) != nil {
		return false
	}
	return meeting.ConferenceSolution.Key.Type == "hangoutsMeet" ||
		meeting.CreateRequest.ConferenceSolutionKey.Type == "hangoutsMeet" ||
		calendar.GoogleMeetJoinURL(calendar.MeetingJoinURL(string(raw))) != ""
}

func calendarGoogleMeetConfirmed(raw json.RawMessage) bool {
	return calendarGoogleMeetJSON(raw) && calendar.GoogleMeetJoinURL(calendar.MeetingJoinURL(string(raw))) != ""
}

func calendarGoogleMeetIdentityMatches(before, after json.RawMessage) bool {
	var old, saved struct {
		ConferenceID  string `json:"conferenceId"`
		CreateRequest struct {
			RequestID string `json:"requestId"`
		} `json:"createRequest"`
	}
	if json.Unmarshal(before, &old) != nil || json.Unmarshal(after, &saved) != nil {
		return false
	}
	if old.ConferenceID != "" && old.ConferenceID != saved.ConferenceID {
		return false
	}
	link := calendar.MeetingJoinURL(string(before))
	if old.ConferenceID == "" && link == "" && old.CreateRequest.RequestID != "" && old.CreateRequest.RequestID != saved.CreateRequest.RequestID {
		return false
	}
	return link == "" || link == calendar.MeetingJoinURL(string(after))
}

func calendarStoredGoogleMeet(event storage.CalendarEvent) bool {
	return event.SourceProvider == "gmail" && calendarGoogleMeetJSON(json.RawMessage(event.OnlineMeetingJSON))
}

func calendarStoredEditableOnline(event storage.CalendarEvent) bool {
	return calendarStoredOutlookOnline(event) || calendarStoredGoogleMeet(event)
}

func calendarGoogleMeetingJSON(remote googleCalendarEvent) json.RawMessage {
	var meeting map[string]any
	if len(remote.ConferenceData) > 0 {
		if err := json.Unmarshal(remote.ConferenceData, &meeting); err != nil {
			return remote.ConferenceData
		}
	}
	if meeting == nil {
		meeting = map[string]any{}
	}
	if calendar.MeetingJoinURL(string(remote.ConferenceData)) == "" && calendar.SafeMeetingURL(remote.HangoutLink) != "" {
		meeting["url"] = calendar.SafeMeetingURL(remote.HangoutLink)
	}
	raw, _ := json.Marshal(meeting)
	return raw
}

func (h *Handler) renderCalendarGoogleMeetOptions(w http.ResponseWriter, r *http.Request, source storage.CalendarSource) {
	ctx := r.Context()
	data := views.CalendarTeamsData{State: "unsupported", SourceID: source.ID, Provider: "gmail"}
	if eventID := r.URL.Query().Get("event_id"); eventID != "" {
		event, err := h.db.GetCalendarEvent(ctx, h.userID(ctx), eventID)
		if err != nil || event.SourceID != source.ID {
			http.NotFound(w, r)
			return
		}
		if _, reason, err := h.calendarEventEditAccess(ctx, event); err != nil || reason != "" {
			data.State = "unavailable"
		} else if calendarStoredGoogleMeet(event) {
			data.State = "existing"
			data.JoinURL = calendar.MeetingJoinURL(event.OnlineMeetingJSON)
		} else if calendarEventIsSeries(event) || event.SeriesRemoteID != "" {
			data.State = "recurring"
		}
		if data.State != "unsupported" {
			_ = views.CalendarTeamsOption(data).Render(ctx, w)
			return
		}
	}
	if calendarSourceWritable(source) && (h.calendarWriteAuthorized(ctx, source) || h.calendarUpdateEvent != nil) {
		var supported bool
		var err error
		if h.calendarMeetSupported != nil {
			supported, err = h.calendarMeetSupported(ctx, source)
		} else {
			credentials := h.calendarUpdateCredentials(ctx, source)
			err = credentials.err
			if err == nil {
				supported, err = googleCalendarMeetSupported(ctx, credentials.token, source.RemoteID)
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
