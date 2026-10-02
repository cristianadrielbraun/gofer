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
	response, err := client.Do(req)
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
	id := strings.ReplaceAll(draft.RequestID, "-", "") // UUID hex is valid Calendar base32hex.
	start, end := map[string]string{}, map[string]string{}
	if draft.AllDay {
		start["date"], end["date"] = draft.StartDate, draft.EndDate
	} else {
		start["dateTime"], end["dateTime"] = draft.StartAt.Format(time.RFC3339), draft.EndAt.Format(time.RFC3339)
		start["timeZone"], end["timeZone"] = draft.TimeZone, draft.TimeZone
	}
	payload := map[string]any{"id": id, "summary": draft.Summary, "description": draft.Description, "location": draft.Location,
		"start": start, "end": end, "extendedProperties": map[string]any{"private": map[string]string{"goferCreateHash": calendarDraftHash(draft)}}}
	endpoint := googleCalendarAPIBaseURL + "/calendars/" + url.PathEscape(remoteCalendarID) + "/events"
	var remote struct {
		googleCalendarEvent
		ExtendedProperties struct {
			Private map[string]string `json:"private"`
		} `json:"extendedProperties"`
	}
	err := calendarCreateJSON(ctx, http.MethodPost, endpoint, token, payload, &remote)
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
	return normalizeGoogleCalendarEvent(remote.googleCalendarEvent)
}

func createOutlookCalendarEvent(ctx context.Context, token, remoteCalendarID string, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
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
	payload := map[string]any{"subject": draft.Summary, "body": outlookCalendarItemBody{ContentType: "text", Content: draft.Description},
		"location": outlookCalendarLocation{DisplayName: draft.Location}, "start": start, "end": end, "isAllDay": draft.AllDay, "transactionId": draft.RequestID}
	var remote outlookCalendarEvent
	endpoint := outlookGraphBaseURL + "/me/calendars/" + url.PathEscape(remoteCalendarID) + "/events"
	if err := calendarCreateJSON(ctx, http.MethodPost, endpoint, token, payload, &remote); err != nil {
		return calendar.RemoteEvent{}, err
	}
	if strings.TrimSpace(remote.ID) == "" {
		return calendar.RemoteEvent{}, fmt.Errorf("Microsoft did not return the created event identity")
	}
	return normalizeOutlookCalendarEvent(remote)
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
	event.Props.SetText("LOCATION", text(draft.Location))
	event.Props.SetText("X-GOFER-CREATE-HASH", calendarDraftHash(draft))
	event.Props.SetText("X-GOFER-TIMEZONE", draft.TimeZone)
	event.Props.SetDateTime("DTSTAMP", time.Now().UTC())
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
	} else {
		event.Props.SetDateTime("DTSTART", draft.StartAt.UTC())
		event.Props.SetDateTime("DTEND", draft.EndAt.UTC())
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
	response, err := client.Do(req)
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
		response, err := client.Do(req)
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
	location, err := time.LoadLocation(draft.TimeZone)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	events, err := parseCalDAVEvents(endpoint, etag, body, location)
	if err != nil || len(events) != 1 {
		return calendar.RemoteEvent{}, fmt.Errorf("could not normalize created CalDAV event")
	}
	if events[0].ICalUID != draft.RequestID+"@gofer" {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV returned a different event identity")
	}
	events[0].StartTimeZone, events[0].EndTimeZone = draft.TimeZone, draft.TimeZone
	return events[0], nil
}
