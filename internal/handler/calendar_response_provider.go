package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var errCalendarNotInvitation = errors.New("this event is not an invitation for this calendar")

func calendarResponseStatus(value string) string {
	switch value {
	case "accepted", "tentative", "declined":
		return value
	case "tentativelyAccepted":
		return "tentative"
	case "needsAction", "notResponded", "none":
		return "needsAction"
	default:
		return ""
	}
}

func googleCalendarSelfResponse(event googleCalendarEvent) (string, string) {
	if event.Status == "cancelled" || event.Organizer.Email == "" || (event.Organizer.Self != nil && *event.Organizer.Self) {
		return "", ""
	}
	var attendees []struct {
		Email, ResponseStatus string
		Self, Organizer       bool
	}
	if json.Unmarshal(event.Attendees, &attendees) != nil {
		return "", ""
	}
	email, response, count := "", "", 0
	for _, person := range attendees {
		if !person.Self {
			continue
		}
		count++
		if person.Organizer || strings.EqualFold(person.Email, event.Organizer.Email) {
			return "", ""
		}
		email, response = strings.TrimSpace(person.Email), calendarResponseStatus(person.ResponseStatus)
	}
	if count != 1 || email == "" {
		return "", ""
	}
	return email, response
}

type calendarResponseTarget struct {
	Event                                                          calendar.RemoteEvent
	Scope, Endpoint, Token, HTTPETag, SelfEmail, OccurrenceVersion string
	Google                                                         *googleCalendarUpdateEvent
	Outlook                                                        *outlookCalendarUpdateEvent
	CalDAV                                                         *calDAVResponseTarget
}

type calendarResponseResult struct {
	Event      calendar.RemoteEvent
	Pending    bool   // Acknowledged, but the new response has not been read back yet.
	Delivery   string // "email" when queued through the durable outgoing worker.
	DeliveryID string
}

// RSVP never shares the ordinary edit restrictions: invitations and online
// meetings are its purpose. Identity, scope and organizer checks are separate.
func readCalendarResponseTarget(ctx context.Context, source storage.CalendarSource, cached storage.CalendarEvent, scope, token string) (calendarResponseTarget, error) {
	read := func(id, selectedScope string) (calendarResponseTarget, error) {
		target := calendarResponseTarget{Scope: selectedScope, Token: token}
		if source.Provider == providers.ProviderGmail {
			target.Endpoint = googleCalendarAPIBaseURL + "/calendars/" + url.PathEscape(source.RemoteID) + "/events/" + url.PathEscape(id)
			target.Google = &googleCalendarUpdateEvent{}
			if err := calendarCreateJSON(ctx, http.MethodGet, target.Endpoint, token, nil, target.Google); err != nil {
				return target, err
			}
			remote := target.Google
			target.SelfEmail, _ = googleCalendarSelfResponse(remote.googleCalendarEvent)
			target.HTTPETag = remote.ETag
			var err error
			target.Event, err = normalizeGoogleCalendarEvent(remote.googleCalendarEvent)
			if err != nil {
				return target, err
			}
			if remote.Locked || (remote.EventType != "" && remote.EventType != "default") || !calendarUpdateValidETag(remote.ETag, false) {
				return target, fmt.Errorf("this Google invitation cannot be answered safely")
			}
			if selectedScope == "occurrence" && (remote.OriginalStartTime == nil || len(remote.Recurrence) != 0) {
				return target, fmt.Errorf("Google did not confirm the occurrence identity")
			}
			if selectedScope == "occurrence" {
				original := remote.OriginalStartTime
				if original.Date != "" {
					if _, err := time.Parse("2006-01-02", original.Date); err != nil || original.DateTime != "" {
						return target, fmt.Errorf("invalid occurrence date")
					}
				} else if _, err := time.Parse(time.RFC3339Nano, original.DateTime); err != nil {
					return target, fmt.Errorf("invalid occurrence time")
				}
			}
			if selectedScope != "occurrence" && remote.OriginalStartTime != nil {
				return target, fmt.Errorf("Google returned an occurrence for a different scope")
			}
		} else if source.Provider == providers.ProviderOutlook {
			target.Endpoint = outlookGraphBaseURL + "/me/calendars/" + url.PathEscape(source.RemoteID) + "/events/" + url.PathEscape(id)
			target.Outlook = &outlookCalendarUpdateEvent{}
			if err := calendarCreateJSON(ctx, http.MethodGet, target.Endpoint, token, nil, target.Outlook); err != nil {
				return target, err
			}
			remote := target.Outlook
			target.HTTPETag = remote.ODataETag
			var err error
			target.Event, err = normalizeOutlookCalendarEvent(remote.outlookCalendarEvent)
			if err != nil {
				return target, err
			}
			if !calendarUpdateValidETag(remote.ODataETag, true) || remote.ChangeKey == "" {
				return target, fmt.Errorf("Microsoft did not return the invitation version")
			}
			validType := (selectedScope == "series" && remote.Type == "seriesMaster") || (selectedScope == "event" && remote.Type == "singleInstance") || (selectedScope == "occurrence" && (remote.Type == "occurrence" || remote.Type == "exception"))
			if !validType {
				return target, fmt.Errorf("Microsoft did not confirm the selected scope")
			}
		} else {
			return target, fmt.Errorf("this provider does not support invitation responses")
		}
		event := target.Event
		if event.RemoteID != id || event.Deleted {
			return target, fmt.Errorf("this is not the selected active invitation")
		}
		if calendarResponseStatus(event.ResponseStatus) == "" {
			return target, errCalendarNotInvitation
		}
		if selectedScope == "occurrence" {
			if event.SeriesRemoteID != cached.SeriesRemoteID || event.SeriesRemoteID == "" || id == event.SeriesRemoteID {
				return target, fmt.Errorf("the invitation's series changed")
			}
		} else if event.SeriesRemoteID != "" || (selectedScope == "series") != calendarUpdateHasDetails(event.Recurrence) {
			return target, fmt.Errorf("the invitation's repeat settings changed")
		}
		return target, nil
	}
	// Reconfirm the relationship before following a cached parent ID. This also
	// rejects an occurrence that moved to a different series since the last sync.
	occurrenceVersion := ""
	if scope == "series" && cached.SeriesRemoteID != "" {
		occurrence, err := read(cached.RemoteID, "occurrence")
		if err != nil {
			return occurrence, err
		}
		if cached.ICalUID != "" && occurrence.Event.ICalUID != cached.ICalUID {
			return occurrence, errCalendarUpdateConflict
		}
		occurrenceVersion = occurrence.Event.ETag
	}
	id := cached.RemoteID
	if scope == "series" {
		id = calendarSeriesID(cached)
	}
	target, err := read(id, scope)
	target.OccurrenceVersion = occurrenceVersion
	if err == nil && scope != "series" && cached.ICalUID != "" && target.Event.ICalUID != cached.ICalUID {
		return target, errCalendarUpdateConflict
	}
	return target, err
}

func sendCalendarResponse(ctx context.Context, source storage.CalendarSource, cached storage.CalendarEvent, target calendarResponseTarget, response string) (calendarResponseResult, error) {
	if response != "accepted" && response != "tentative" && response != "declined" {
		return calendarResponseResult{}, fmt.Errorf("invalid response")
	}
	if source.Provider == providers.ProviderGmail {
		// attendeesOmitted is specifically documented for updating just the
		// participant's response. Never PATCH a reconstructed attendee roster.
		// https://developers.google.com/workspace/calendar/api/v3/reference/events
		payload := map[string]any{"attendeesOmitted": true, "attendees": []map[string]string{{"email": target.SelfEmail, "responseStatus": response}}}
		var saved googleCalendarUpdateEvent
		if err := calendarUpdateJSON(ctx, target.Endpoint+"?sendUpdates=all", target.Token, target.HTTPETag, payload, &saved); err != nil {
			return calendarResponseResult{}, err
		}
	} else {
		// Graph response actions acknowledge with 202 and send no event body.
		// A read-back, not that acknowledgement alone, confirms the new status.
		// https://learn.microsoft.com/graph/api/event-accept?view=graph-rest-1.0
		action := map[string]string{"accepted": "accept", "tentative": "tentativelyAccept", "declined": "decline"}[response]
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.Endpoint+"/"+action, bytes.NewBufferString(`{"sendResponse":true}`))
		if err != nil {
			return calendarResponseResult{}, err
		}
		req.Header.Set("Authorization", "Bearer "+target.Token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Prefer", `IdType="ImmutableId"`)
		// Keep the freshly read validator, but do not assume action endpoints
		// guarantee PATCH-style concurrency. Never retry a response action.
		req.Header.Set("If-Match", target.HTTPETag)
		client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		result, err := client.Do(req)
		if err != nil {
			return calendarResponseResult{}, err
		}
		result.Body.Close()
		if result.StatusCode < 200 || result.StatusCode >= 300 {
			return calendarResponseResult{}, calendarUpdateHTTPError(calendarCreateProviderError{result.StatusCode})
		}
		if result.StatusCode != http.StatusAccepted {
			return calendarResponseResult{}, fmt.Errorf("Microsoft did not acknowledge the response")
		}
	}
	saved, err := readCalendarResponseTarget(ctx, source, cached, target.Scope, target.Token)
	if err != nil || saved.Event.RemoteID != target.Event.RemoteID || saved.Event.ResponseStatus != response {
		return calendarResponseResult{Pending: true}, nil
	}
	if target.SelfEmail != "" && !strings.EqualFold(saved.SelfEmail, target.SelfEmail) {
		return calendarResponseResult{Pending: true}, nil
	}
	return calendarResponseResult{Event: saved.Event}, nil
}
