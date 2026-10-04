package handler

import (
	"encoding/json"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func calendarOutlookOnline(remote outlookCalendarUpdateEvent) bool {
	return calendarUpdateHasDetails(calendarOutlookMeetingJSON(remote.outlookCalendarEvent))
}

func calendarStoredOutlookOnline(event storage.CalendarEvent) bool {
	return event.SourceProvider == "outlook" && calendarUpdateHasDetails(calendarOutlookCachedMeetingJSON(event))
}

func calendarOutlookCachedMeetingJSON(event storage.CalendarEvent) json.RawMessage {
	raw := json.RawMessage(event.OnlineMeetingJSON)
	if event.SourceProvider != "outlook" || calendarUpdateHasDetails(raw) {
		return raw
	}
	return calendarOutlookMeetingJSON(outlookCalendarEvent{Body: outlookCalendarItemBody{Content: event.Description}})
}

func calendarOutlookMeetingJSON(remote outlookCalendarEvent) json.RawMessage {
	var meeting map[string]any
	_ = json.Unmarshal(remote.OnlineMeeting, &meeting)
	if meeting == nil {
		meeting = map[string]any{}
	}
	// Only use the body when structured metadata is missing for a consumer
	// event, or Teams is explicitly enabled but its join URL is still pending.
	consumerFallback := len(meeting) == 0 && remote.OnlineMeetingURL == "" && calendarConsumerMeetingProvider(remote.OnlineMeetingProvider)
	pendingTeams := remote.IsOnlineMeeting != nil && *remote.IsOnlineMeeting && remote.OnlineMeetingProvider == "teamsForBusiness" && calendar.MeetingJoinURL(string(remote.OnlineMeeting)) == "" && remote.OnlineMeetingURL == ""
	if remote.IsOnlineMeeting != nil && (*remote.IsOnlineMeeting || len(meeting) > 0 || remote.OnlineMeetingURL != "" || (remote.OnlineMeetingProvider != "" && remote.OnlineMeetingProvider != "unknown")) {
		meeting["isOnlineMeeting"] = *remote.IsOnlineMeeting
	}
	if remote.OnlineMeetingProvider != "" && remote.OnlineMeetingProvider != "unknown" {
		meeting["provider"] = remote.OnlineMeetingProvider
	}
	if value := strings.TrimSpace(remote.OnlineMeetingURL); value != "" {
		meeting["url"] = value
	}
	if consumerFallback || pendingTeams {
		body := remote.Body.Content
		if body == "" && remote.Body.ContentType == "" {
			body = remote.BodyPreview
		}
		if link := calendar.TeamsJoinURLFromDescription(body); link != "" {
			meeting["joinUrl"], meeting["linkSource"], meeting["isOnlineMeeting"] = link, "description", true
			if remote.IsOnlineMeeting != nil {
				meeting["graphIsOnlineMeeting"] = *remote.IsOnlineMeeting
			}
		}
	}
	raw, _ := json.Marshal(meeting)
	return raw
}
