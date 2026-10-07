package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

type calendarCreateProviderError struct{ Status int }

type calendarCreateAuthError struct{ err error }

func (e calendarCreateAuthError) Error() string { return e.err.Error() }
func (e calendarCreateAuthError) Unwrap() error { return e.err }

func (e calendarCreateProviderError) Error() string {
	return fmt.Sprintf("Calendar provider returned HTTP %d", e.Status)
}

func calendarDraftHash(draft calendar.EventDraft) string {
	raw, _ := json.Marshal(draft)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func calendarCreateUncertain(err error) bool {
	var preflight calendarDeletePreflightError
	if errors.As(err, &preflight) {
		return false
	}
	var authorization calendarCreateAuthError
	if errors.As(err, &authorization) {
		return false
	}
	var provider calendarCreateProviderError
	if errors.As(err, &provider) {
		return provider.Status >= 500 || provider.Status == 408
	}
	return true // Transport, timeout, or malformed successful response: do not promise nothing was created.
}

func calendarCreateJSON(ctx context.Context, method, endpoint, token string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Prefer", `IdType="ImmutableId"`)
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := calendarProviderDo(client, req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return calendarCreateProviderError{response.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, calDAVResponseLimit+1))
	if err != nil {
		return err
	}
	if len(raw) > calDAVResponseLimit {
		return fmt.Errorf("calendar event response exceeds size limit")
	}
	return json.Unmarshal(raw, out)
}

func createGoogleCalendarEvent(ctx context.Context, token, remoteCalendarID string, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
	if draft.GoogleMeetMeeting {
		if draft.Recurrence != nil {
			return calendar.RemoteEvent{}, calendarDeletePreflightError{calendarUpdateUnsupported("Adding Google Meet to recurring events is not supported in Gofer yet.")}
		}
		supported, err := googleCalendarMeetSupported(ctx, token, remoteCalendarID)
		if err != nil {
			return calendar.RemoteEvent{}, calendarDeletePreflightError{err}
		}
		if !supported {
			return calendar.RemoteEvent{}, calendarDeletePreflightError{calendarUpdateUnsupported(calendarGoogleMeetUnavailableMessage)}
		}
	}
	id := strings.ReplaceAll(draft.RequestID, "-", "") // UUID hex is valid Calendar base32hex.
	start, end := map[string]string{}, map[string]string{}
	if draft.AllDay {
		start["date"], end["date"] = draft.StartDate, draft.EndDate
	} else {
		start["dateTime"], end["dateTime"] = draft.StartAt.Format(time.RFC3339), draft.EndAt.Format(time.RFC3339)
		start["timeZone"], end["timeZone"] = draft.TimeZone, draft.TimeZone
	}
	payload := map[string]any{"id": id, "summary": draft.Summary, "description": calendar.DraftDescription(draft), "location": draft.Location,
		"start": start, "end": end, "extendedProperties": map[string]any{"private": map[string]string{"goferCreateHash": calendarDraftHash(draft)}}}
	if draft.GoogleMeetMeeting {
		if len(draft.GoogleMeetConference) > 0 {
			payload["conferenceData"] = draft.GoogleMeetConference
		} else {
			payload["conferenceData"] = calendarGoogleMeetRequest(draft.RequestID)
		}
	}
	if draft.Recurrence != nil {
		payload["recurrence"] = []string{"RRULE:" + calendarRecurrenceRule(draft)}
	}
	if len(draft.Guests) > 0 {
		payload["attendees"] = calendarMeetingGoogleGuests(draft, nil)
	}
	endpoint := googleCalendarAPIBaseURL + "/calendars/" + url.PathEscape(remoteCalendarID) + "/events"
	var remote struct {
		googleCalendarEvent
		ExtendedProperties struct {
			Private map[string]string `json:"private"`
		} `json:"extendedProperties"`
	}
	writeEndpoint := endpoint
	params := url.Values{}
	if draft.GoogleMeetMeeting {
		params.Set("conferenceDataVersion", "1")
	}
	if len(draft.Guests) > 0 {
		params.Set("sendUpdates", "all")
	}
	if len(params) > 0 {
		writeEndpoint += "?" + params.Encode()
	}
	err := calendarCreateJSON(ctx, http.MethodPost, writeEndpoint, token, payload, &remote)
	var conflict calendarCreateProviderError
	if errors.As(err, &conflict) && conflict.Status == http.StatusConflict {
		// A previous attempt may have succeeded before its response was lost.
		err = calendarCreateJSON(ctx, http.MethodGet, endpoint+"/"+id, token, nil, &remote)
		if err == nil && remote.ExtendedProperties.Private["goferCreateHash"] != calendarDraftHash(draft) {
			return calendar.RemoteEvent{}, calendarCreateProviderError{http.StatusConflict}
		}
	}
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	if remote.ID != id {
		return calendar.RemoteEvent{}, fmt.Errorf("Google did not confirm the requested event identity")
	}
	if draft.GoogleMeetMeeting && !calendarGoogleMeetConfirmed(calendarGoogleMeetingJSON(remote.googleCalendarEvent)) {
		var confirmed googleCalendarEvent
		if err := calendarCreateJSON(ctx, http.MethodGet, endpoint+"/"+id, token, nil, &confirmed); err == nil {
			if confirmed.ID != id {
				return calendar.RemoteEvent{}, fmt.Errorf("Google returned a different meeting identity")
			}
			remote.googleCalendarEvent = confirmed
		}
	}
	if draft.GoogleMeetMeeting {
		confirmed, err := normalizeGoogleCalendarEvent(remote.googleCalendarEvent)
		if err != nil || !calendarUpdateMatchesDraft(confirmed, draft) || confirmed.Deleted {
			return calendar.RemoteEvent{}, fmt.Errorf("Google did not confirm the submitted event details")
		}
	}
	if draft.Recurrence != nil && len(remote.Recurrence) == 0 {
		return calendar.RemoteEvent{}, fmt.Errorf("Google did not confirm the recurring series")
	}
	if len(draft.Guests) > 0 && (calendarGoogleOrganizerStatus(remote.googleCalendarEvent) != "organizer" || !calendarMeetingGuestsMatch(remote.Attendees, draft.Guests, remote.Organizer.Email)) {
		return calendar.RemoteEvent{}, fmt.Errorf("Google did not confirm the invited guests and organizer")
	}
	if len(draft.GoogleMeetConference) > 0 && !calendarGoogleMeetIdentityMatches(draft.GoogleMeetConference, calendarGoogleMeetingJSON(remote.googleCalendarEvent)) {
		return calendar.RemoteEvent{}, fmt.Errorf("Google did not retain the prepared Meet link")
	}
	return normalizeGoogleCalendarEvent(remote.googleCalendarEvent)
}

func createOutlookCalendarEvent(ctx context.Context, token, remoteCalendarID string, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
	teamsMode := ""
	if draft.TeamsMeeting {
		if draft.Recurrence != nil {
			return calendar.RemoteEvent{}, calendarDeletePreflightError{calendarUpdateUnsupported("Adding Teams to recurring events is not supported in Gofer yet.")}
		}
		mode, err := outlookCalendarTeamsMode(ctx, token, remoteCalendarID)
		if err != nil {
			return calendar.RemoteEvent{}, calendarDeletePreflightError{err}
		}
		if mode != "available" && mode != "available-default" {
			return calendar.RemoteEvent{}, calendarDeletePreflightError{calendarUpdateUnsupported(calendarTeamsUnavailableMessage)}
		}
		teamsMode = mode
	}
	start, end := map[string]string{"timeZone": draft.TimeZone}, map[string]string{"timeZone": draft.TimeZone}
	if draft.AllDay {
		start["dateTime"], end["dateTime"] = draft.StartDate+"T00:00:00", draft.EndDate+"T00:00:00"
	} else {
		location, err := time.LoadLocation(draft.TimeZone)
		if err != nil {
			return calendar.RemoteEvent{}, err
		}
		start["dateTime"], end["dateTime"] = draft.StartAt.In(location).Format("2006-01-02T15:04:05"), draft.EndAt.In(location).Format("2006-01-02T15:04:05")
	}
	payload := map[string]any{"subject": draft.Summary, "body": calendarDraftOutlookBody(draft),
		"location": outlookCalendarLocation{DisplayName: draft.Location}, "start": start, "end": end, "isAllDay": draft.AllDay, "transactionId": draft.RequestID}
	if draft.TeamsMeeting {
		payload["isOnlineMeeting"] = true
		if teamsMode == "available" {
			payload["onlineMeetingProvider"] = "teamsForBusiness"
		}
	}
	if draft.Recurrence != nil {
		payload["recurrence"] = calendarOutlookRecurrence(draft)
	}
	if len(draft.Guests) > 0 {
		payload["attendees"] = calendarMeetingOutlookGuests(draft, nil)
		payload["responseRequested"] = true
	}
	var remote outlookCalendarEvent
	endpoint := outlookGraphBaseURL + "/me/calendars/" + url.PathEscape(remoteCalendarID) + "/events"
	if err := calendarCreateJSON(ctx, http.MethodPost, endpoint, token, payload, &remote); err != nil {
		return calendar.RemoteEvent{}, err
	}
	if strings.TrimSpace(remote.ID) == "" {
		return calendar.RemoteEvent{}, fmt.Errorf("Microsoft did not return the created event identity")
	}
	if draft.TeamsMeeting && !calendarOutlookTeamsConfirmed(remote) {
		createdID := remote.ID
		remote = outlookCalendarEvent{}
		if err := calendarCreateJSON(ctx, http.MethodGet, endpoint+"/"+url.PathEscape(createdID), token, nil, &remote); err != nil {
			return calendar.RemoteEvent{}, fmt.Errorf("could not confirm the Microsoft Teams meeting: %v", err)
		}
		if remote.ID != createdID {
			return calendar.RemoteEvent{}, fmt.Errorf("Microsoft returned a different meeting identity")
		}
	}
	if draft.TeamsMeeting && !calendarTeamsDescriptionConfirmed(remote.Body.Content, draft) {
		return calendar.RemoteEvent{}, fmt.Errorf("Microsoft did not confirm the submitted description")
	}
	if draft.Recurrence != nil && !calendarUpdateHasDetails(remote.Recurrence) {
		return calendar.RemoteEvent{}, fmt.Errorf("Microsoft did not confirm the recurring series")
	}
	if len(draft.Guests) > 0 && (calendarOutlookOrganizerStatus(remote) != "organizer" || !calendarMeetingGuestsMatch(remote.Attendees, draft.Guests, remote.Organizer.EmailAddress.Address)) {
		return calendar.RemoteEvent{}, fmt.Errorf("Microsoft did not confirm the invited guests and organizer")
	}
	event, err := normalizeOutlookCalendarEvent(remote)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	if draft.TeamsMeeting {
		confirmed := event
		confirmed.Description = calendar.DraftDescription(draft) // Provider may append its meeting block.
		if !calendarUpdateMatchesDraft(confirmed, draft) {
			return calendar.RemoteEvent{}, fmt.Errorf("Microsoft did not confirm the submitted event changes")
		}
	}
	return event, nil
}

func calendarCreateICS(draft calendar.EventDraft) (string, error) {
	cal := ical.NewCalendar()
	cal.Props.SetText("PRODID", "-//Gofer//Calendar//EN")
	cal.Props.SetText("VERSION", "2.0")
	event := ical.NewEvent()
	event.Props.SetText("UID", draft.RequestID+"@gofer")
	// iCalendar TEXT escapes LF; normalize Windows/macOS line endings first
	// so a pasted CR can neither break encoding nor inject another property.
	text := func(value string) string {
		return strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
	}
	event.Props.SetText("SUMMARY", text(draft.Summary))
	event.Props.SetText("DESCRIPTION", text(draft.Description))
	if draft.DescriptionHTML != nil && *draft.DescriptionHTML != "" {
		event.Props.SetText("X-ALT-DESC", *draft.DescriptionHTML)
		event.Props.Get("X-ALT-DESC").Params.Set("FMTTYPE", "text/html")
	}
	event.Props.SetText("LOCATION", text(draft.Location))
	event.Props.SetText("X-GOFER-CREATE-HASH", calendarDraftHash(draft))
	event.Props.SetText("X-GOFER-TIMEZONE", draft.TimeZone)
	event.Props.SetDateTime("DTSTAMP", time.Now().UTC())
	if len(draft.Guests) > 0 {
		if calendarReplyAddress("mailto:"+draft.OrganizerEmail) == "" {
			return "", fmt.Errorf("a verified organizer is required")
		}
		organizer := &ical.Prop{Name: "ORGANIZER", Value: "mailto:" + draft.OrganizerEmail, Params: ical.Params{}}
		if draft.OrganizerName != "" {
			organizer.Params.Set("CN", draft.OrganizerName)
		}
		if draft.ScheduleAgent != "" {
			organizer.Params.Set("SCHEDULE-AGENT", draft.ScheduleAgent)
		}
		event.Props.Set(organizer)
		event.Props.SetText("SEQUENCE", "0")
		for _, guest := range draft.Guests {
			attendee := ical.Prop{Name: "ATTENDEE", Value: "mailto:" + guest.Email, Params: ical.Params{}}
			attendee.Params.Set("CN", guest.Name)
			attendee.Params.Set("PARTSTAT", "NEEDS-ACTION")
			attendee.Params.Set("RSVP", "TRUE")
			role := "REQ-PARTICIPANT"
			if guest.Optional {
				role = "OPT-PARTICIPANT"
			}
			attendee.Params.Set("ROLE", role)
			if draft.ScheduleAgent != "" {
				attendee.Params.Set("SCHEDULE-AGENT", draft.ScheduleAgent)
			}
			event.Props["ATTENDEE"] = append(event.Props["ATTENDEE"], attendee)
		}
	}
	if draft.AllDay {
		start, err := time.Parse("2006-01-02", draft.StartDate)
		if err != nil {
			return "", err
		}
		end, err := time.Parse("2006-01-02", draft.EndDate)
		if err != nil {
			return "", err
		}
		event.Props.SetDate("DTSTART", start)
		event.Props.SetDate("DTEND", end)
	} else if draft.Recurrence != nil {
		zone, err := time.LoadLocation(draft.TimeZone)
		if err != nil {
			return "", err
		}
		event.Props.SetDateTime("DTSTART", draft.StartAt.In(zone))
		event.Props.SetDateTime("DTEND", draft.EndAt.In(zone))
		if zone != time.UTC {
			timezone, err := calendarRecurrenceTimezone(draft, zone)
			if err != nil {
				return "", err
			}
			cal.Children = append(cal.Children, timezone)
		}
	} else {
		event.Props.SetDateTime("DTSTART", draft.StartAt.UTC())
		event.Props.SetDateTime("DTEND", draft.EndAt.UTC())
	}
	if draft.Recurrence != nil {
		event.Props.Set(&ical.Prop{Name: "RRULE", Value: calendarRecurrenceRule(draft)})
	}
	cal.Children = append(cal.Children, event.Component)
	var output bytes.Buffer
	if err := ical.NewEncoder(&output).Encode(cal); err != nil {
		return "", err
	}
	return output.String(), nil
}

func createCalDAVCalendarEvent(ctx context.Context, source storage.CalendarSource, username, password string, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
	base, err := url.Parse(source.RemoteID)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return calendar.RemoteEvent{}, calendarCreateProviderError{http.StatusBadRequest}
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + draft.RequestID + ".ics"
	base.RawPath = ""
	endpoint := base.String()
	body, err := calendarCreateICS(draft)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	client := &http.Client{Transport: calDAVHTTPTransport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := newCardDAVRequest(ctx, http.MethodPut, endpoint, username, password, strings.NewReader(body))
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
	req.Header.Set("If-None-Match", "*") // Never overwrite a pre-existing event.
	response, err := calendarProviderDo(client, req)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	status, etag := response.StatusCode, response.Header.Get("ETag")
	response.Body.Close()
	if status == http.StatusPreconditionFailed {
		req, err := newCardDAVRequest(ctx, http.MethodGet, endpoint, username, password, nil)
		if err != nil {
			return calendar.RemoteEvent{}, err
		}
		response, err := calendarProviderDo(client, req)
		if err != nil {
			return calendar.RemoteEvent{}, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return calendar.RemoteEvent{}, calendarCreateProviderError{response.StatusCode}
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, calDAVResponseLimit+1))
		if err != nil || len(raw) > calDAVResponseLimit {
			return calendar.RemoteEvent{}, fmt.Errorf("could not confirm previous CalDAV creation")
		}
		decoded, err := ical.NewDecoder(bytes.NewReader(raw)).Decode()
		if err != nil || len(decoded.Events()) != 1 {
			return calendar.RemoteEvent{}, fmt.Errorf("could not confirm previous CalDAV creation")
		}
		hash, _ := decoded.Events()[0].Props.Text("X-GOFER-CREATE-HASH")
		if hash != calendarDraftHash(draft) {
			return calendar.RemoteEvent{}, calendarCreateProviderError{http.StatusConflict}
		}
		body, etag = string(raw), response.Header.Get("ETag")
	} else if status != http.StatusCreated && status != http.StatusNoContent {
		return calendar.RemoteEvent{}, calendarCreateProviderError{status}
	}
	if len(draft.Guests) > 0 {
		confirmed, headers, err := calendarUpdateCalDAVGet(ctx, client, endpoint, username, password)
		if err != nil {
			return calendar.RemoteEvent{}, fmt.Errorf("could not confirm the CalDAV meeting: %v", err)
		}
		body, err = encodeCalendarReply(confirmed)
		if err != nil {
			return calendar.RemoteEvent{}, err
		}
		etag = headers.Get("ETag")
		if !calendarUpdateValidETag(etag, false) {
			return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm a safe meeting version")
		}
	}
	location, err := time.LoadLocation(draft.TimeZone)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	// Creation confirms the series resource, not an expanded occurrence. The
	// normal REPORT read path remains strict about requiring server expansion.
	var recurrence []byte
	if draft.Recurrence != nil {
		decoded, decodeErr := ical.NewDecoder(strings.NewReader(body)).Decode()
		if decodeErr != nil || len(decoded.Events()) != 1 {
			return calendar.RemoteEvent{}, fmt.Errorf("could not confirm the CalDAV series")
		}
		component := decoded.Events()[0]
		rule := component.Props.Get("RRULE")
		if rule == nil || component.Props.Get("RECURRENCE-ID") != nil {
			return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm the recurring series")
		}
		recurrence, _ = json.Marshal([]string{"RRULE:" + rule.Value})
		delete(component.Props, "RRULE")
		var single bytes.Buffer
		if err := ical.NewEncoder(&single).Encode(decoded); err != nil {
			return calendar.RemoteEvent{}, err
		}
		body = single.String()
	}
	events, err := parseCalDAVEvents(endpoint, etag, body, location)
	if err != nil || len(events) != 1 {
		return calendar.RemoteEvent{}, fmt.Errorf("could not normalize created CalDAV event")
	}
	if events[0].ICalUID != draft.RequestID+"@gofer" {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV returned a different event identity")
	}
	if len(draft.Guests) > 0 {
		if !strings.EqualFold(events[0].OrganizerEmail, draft.OrganizerEmail) || !calendarMeetingGuestsMatch(events[0].Attendees, draft.Guests, draft.OrganizerEmail) || !calendarUpdateMatchesDraft(events[0], draft) {
			return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm the meeting and guests")
		}
		events[0].ResponseStatus = "organizer"
	}
	events[0].StartTimeZone, events[0].EndTimeZone = draft.TimeZone, draft.TimeZone
	if draft.Recurrence != nil {
		events[0].Recurrence = recurrence
	}
	return events[0], nil
}
