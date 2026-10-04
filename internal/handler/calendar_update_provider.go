package handler

import (
	"bytes"
	"context"
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

var (
	errCalendarUpdateConflict    = errors.New("This event has changed. Refresh the calendar before editing it again.")
	errCalendarUpdateUnsupported = errors.New("This event cannot be edited in Gofer.")
)

// calendarUpdateRestriction checks event shape only, including cached events.
// Version availability and provider-only safety flags are checked by the adapters.
// Its nonempty return value is suitable for display in the editor.
func calendarUpdateRestriction(event calendar.RemoteEvent) string {
	if event.Deleted || strings.EqualFold(strings.TrimSpace(event.Status), "cancelled") || strings.EqualFold(strings.TrimSpace(event.Status), "canceled") {
		return "Cancelled events cannot be edited in Gofer."
	}
	if strings.TrimSpace(event.SeriesRemoteID) != "" || calendarUpdateHasDetails(event.Recurrence) {
		return "Recurring events and individual occurrences cannot be edited in Gofer yet."
	}
	if calendarUpdateHasDetails(event.Attendees) {
		if event.ResponseStatus != "organizer" {
			return "Events with guests can only be edited by their organizer."
		}
		if _, err := calendarMeetingGuests(event.Attendees); err != nil {
			return err.Error()
		}
	}
	if calendarUpdateHasDetails(event.OnlineMeeting) {
		return "Online meetings cannot be edited in Gofer yet."
	}
	return ""
}

func calendarUpdateHasDetails(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return true // Unreadable cached details must not enable editing.
	}
	switch value := value.(type) {
	case nil:
		return false
	case []any:
		return len(value) != 0
	case map[string]any:
		return len(value) != 0
	default:
		return true
	}
}

func calendarUpdateUnsupported(reason string) error {
	return fmt.Errorf("%w %s", errCalendarUpdateUnsupported, reason)
}

func calendarUpdateExistingRestriction(existing storage.CalendarEvent) error {
	if strings.TrimSpace(existing.RemoteID) == "" || strings.TrimSpace(existing.ETag) == "" {
		return calendarUpdateUnsupported("Refresh the calendar to retrieve the event's current version.")
	}
	if reason := calendarUpdateRestriction(calendar.RemoteEvent{
		Status: existing.Status, Deleted: existing.IsDeleted, SeriesRemoteID: existing.SeriesRemoteID,
		Recurrence: json.RawMessage(existing.RecurrenceJSON), Attendees: json.RawMessage(existing.AttendeesJSON),
		OnlineMeeting:  json.RawMessage(existing.OnlineMeetingJSON),
		ResponseStatus: existing.ResponseStatus,
	}); reason != "" {
		return calendarUpdateUnsupported(reason)
	}
	return nil
}

// Only a single nonempty entity tag is safe here: never accept wildcard/list
// validators or turn an unquoted value into a conditional HTTP header.
func calendarUpdateValidETag(value string, allowWeak bool) bool {
	if allowWeak {
		value = strings.TrimPrefix(value, "W/")
	}
	if len(value) < 3 || value[0] != '"' || value[len(value)-1] != '"' {
		return false
	}
	for i := 1; i < len(value)-1; i++ {
		if value[i] < 0x21 || value[i] == '"' || value[i] == 0x7f {
			return false
		}
	}
	return true
}

func calendarUpdateHTTPError(err error) error {
	var provider calendarCreateProviderError
	if errors.As(err, &provider) && (provider.Status == http.StatusConflict || provider.Status == http.StatusPreconditionFailed) {
		return errCalendarUpdateConflict
	}
	return err
}

// Reads reuse the creation adapter's bounded JSON helper/default transport.
// Writes add If-Match and never retry, redirect, or fall back to an unconditional
// request. An invalid successful response remains an ordinary (uncertain) error.
func calendarUpdateJSON(ctx context.Context, endpoint, token, etag string, payload, out any) error {
	if !calendarUpdateValidETag(etag, true) {
		return calendarUpdateUnsupported("The calendar provider did not supply a safe event version.")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Prefer", `IdType="ImmutableId"`)
	req.Header.Set("If-Match", etag)
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusBadRequest {
		var failure struct{ Error struct{ Code string } }
		if json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&failure) == nil && failure.Error.Code == "ErrorOccurrenceCrossingBoundary" {
			return errCalendarOccurrenceBoundary
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return calendarUpdateHTTPError(calendarCreateProviderError{response.StatusCode})
	}
	if response.StatusCode == http.StatusNoContent {
		return nil // The adapter will confirm the representation with a GET.
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("calendar provider did not confirm the update (HTTP %d)", response.StatusCode)
	}
	raw, err = io.ReadAll(io.LimitReader(response.Body, calDAVResponseLimit+1))
	if err != nil {
		return err
	}
	if len(raw) > calDAVResponseLimit {
		return fmt.Errorf("calendar event response exceeds size limit")
	}
	return json.Unmarshal(raw, out)
}

func calendarUpdateDraftLocation(draft calendar.EventDraft) (*time.Location, error) {
	location, err := time.LoadLocation(draft.TimeZone)
	if err != nil || draft.TimeZone == "" || draft.TimeZone == "Local" {
		return nil, fmt.Errorf("calendar update requires a valid timezone")
	}
	if draft.AllDay {
		start, startErr := time.Parse("2006-01-02", draft.StartDate)
		end, endErr := time.Parse("2006-01-02", draft.EndDate)
		if startErr != nil || endErr != nil || !end.After(start) {
			return nil, fmt.Errorf("calendar update requires a valid date range")
		}
	} else if draft.StartAt == nil || draft.EndAt == nil || draft.StartAt.IsZero() || !draft.EndAt.After(*draft.StartAt) {
		return nil, fmt.Errorf("calendar update requires a valid time range")
	}
	return location, nil
}

// A confirmation GET must show the submitted editable fields, not just an old
// or concurrently replaced appointment at the same URL. Providers may return
// a plain-text body as HTML, so compare its visible text for this purpose.
func calendarUpdateMatchesDraft(event calendar.RemoteEvent, draft calendar.EventDraft) bool {
	text := func(value string) string { return strings.Join(strings.Fields(calendarDescriptionText(value)), " ") }
	if event.Summary != strings.TrimSpace(draft.Summary) || event.Location != strings.TrimSpace(draft.Location) ||
		text(event.Description) != text(draft.Description) || event.AllDay != draft.AllDay {
		return false
	}
	if draft.AllDay {
		return event.StartDate == draft.StartDate && event.EndDate == draft.EndDate
	}
	return event.StartAt != nil && event.EndAt != nil && event.StartAt.Equal(*draft.StartAt) && event.EndAt.Equal(*draft.EndAt)
}

// Extra fields remain local to updates; the read/create wire types stay intact.
type googleCalendarUpdateEvent struct {
	googleCalendarEvent
	EventType         string                       `json:"eventType"`
	AttendeesOmitted  bool                         `json:"attendeesOmitted"`
	Locked            bool                         `json:"locked"`
	OriginalStartTime *googleCalendarEventDateTime `json:"originalStartTime"`
}

func (remote googleCalendarUpdateEvent) restriction() string {
	recurrence, _ := json.Marshal(remote.Recurrence)
	if reason := calendarUpdateRestriction(calendar.RemoteEvent{
		Status: remote.Status, SeriesRemoteID: remote.RecurringEventID,
		Recurrence: recurrence, Attendees: remote.Attendees, OnlineMeeting: remote.ConferenceData,
		ResponseStatus: calendarGoogleOrganizerStatus(remote.googleCalendarEvent),
	}); reason != "" {
		return reason
	}
	if remote.OriginalStartTime != nil {
		return "Recurring events and individual occurrences cannot be edited in Gofer yet."
	}
	if remote.AttendeesOmitted {
		return "Events with hidden guest details cannot be edited in Gofer."
	}
	if strings.TrimSpace(remote.HangoutLink) != "" {
		return "Online meetings cannot be edited in Gofer yet."
	}
	if remote.Locked || (remote.EventType != "" && remote.EventType != "default") {
		return "Only regular appointments can be edited in Gofer."
	}
	return ""
}

func updateGoogleCalendarEvent(ctx context.Context, token, remoteCalendarID string, existing storage.CalendarEvent, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
	return updateGoogleCalendarEventScope(ctx, token, remoteCalendarID, existing, draft, false)
}

func updateGoogleCalendarEventScope(ctx context.Context, token, remoteCalendarID string, existing storage.CalendarEvent, draft calendar.EventDraft, series bool, occurrenceScope ...bool) (calendar.RemoteEvent, error) {
	occurrence := calendarOccurrenceScope(occurrenceScope)
	if err := calendarUpdateExistingScopeRestriction(existing, series, occurrence); err != nil {
		return calendar.RemoteEvent{}, err
	}
	if occurrence && draft.Recurrence != nil {
		return calendar.RemoteEvent{}, errCalendarUpdateUnsupported
	}
	version := strings.TrimSpace(existing.ETag)
	if !calendarUpdateValidETag(version, false) {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported("Refresh the calendar to retrieve a safe event version.")
	}
	if _, err := calendarUpdateDraftLocation(draft); err != nil {
		return calendar.RemoteEvent{}, err
	}
	endpoint := googleCalendarAPIBaseURL + "/calendars/" + url.PathEscape(remoteCalendarID) + "/events/" + url.PathEscape(existing.RemoteID)
	var current googleCalendarUpdateEvent
	if err := calendarCreateJSON(ctx, http.MethodGet, endpoint, token, nil, &current); err != nil {
		return calendar.RemoteEvent{}, calendarUpdateHTTPError(err)
	}
	if current.ID != existing.RemoteID {
		return calendar.RemoteEvent{}, fmt.Errorf("Google did not return the requested event identity")
	}
	if !calendarUpdateValidETag(current.ETag, false) {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported("Google did not supply a safe event version.")
	}
	if current.ETag != version {
		return calendar.RemoteEvent{}, errCalendarUpdateConflict
	}
	if err := calendarMeetingScopeRestriction(calendarGoogleOrganizerStatus(current.googleCalendarEvent), current.Attendees, draft, series || occurrence || draft.Recurrence != nil); err != nil {
		return calendar.RemoteEvent{}, err
	}
	draft.OrganizerEmail = current.Organizer.Email
	if err := calendarMeetingCheckSelfGuest(draft, draft.OrganizerEmail); err != nil {
		return calendar.RemoteEvent{}, err
	}
	if series {
		if draft.Recurrence == nil {
			return calendar.RemoteEvent{}, errCalendarUpdateUnsupported
		}
		if _, err := calendarGoogleSeriesEvent(current); err != nil {
			return calendar.RemoteEvent{}, err
		}
	} else if occurrence {
		if reason := current.occurrenceRestriction(existing.SeriesRemoteID); reason != "" {
			return calendar.RemoteEvent{}, calendarUpdateUnsupported(reason)
		}
	} else if reason := current.restriction(); reason != "" {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported(reason)
	}
	if _, err := normalizeGoogleCalendarEvent(current.googleCalendarEvent); err != nil {
		return calendar.RemoteEvent{}, err
	}
	// Google merges nested PATCH objects. Explicitly clear the opposite date
	// representation when switching between all-day and timed appointments.
	start := map[string]any{"date": nil, "dateTime": nil, "timeZone": nil}
	end := map[string]any{"date": nil, "dateTime": nil, "timeZone": nil}
	if draft.AllDay {
		start["date"], end["date"] = draft.StartDate, draft.EndDate
	} else {
		start["dateTime"], end["dateTime"] = draft.StartAt.Format(time.RFC3339Nano), draft.EndAt.Format(time.RFC3339Nano)
		start["timeZone"], end["timeZone"] = draft.TimeZone, draft.TimeZone
	}
	payload := map[string]any{"summary": draft.Summary, "location": draft.Location, "start": start, "end": end}
	if draft.GuestsSet {
		payload["attendees"] = calendarMeetingGoogleGuests(draft, current.Attendees)
	}
	if draft.Recurrence != nil {
		payload["recurrence"] = []string{"RRULE:" + calendarRecurrenceRule(draft)}
	}
	if draft.Description != calendarDescriptionText(existing.Description) {
		payload["description"] = draft.Description
	}
	var updated googleCalendarUpdateEvent
	writeEndpoint := endpoint
	if len(draft.Guests) > 0 || calendarUpdateHasDetails(current.Attendees) {
		writeEndpoint += "?sendUpdates=all"
	}
	if err := calendarUpdateJSON(ctx, writeEndpoint, token, current.ETag, payload, &updated); err != nil {
		return calendar.RemoteEvent{}, err
	}
	needsConfirmation := strings.TrimSpace(updated.ETag) == ""
	if needsConfirmation {
		if updated.ID != "" && updated.ID != existing.RemoteID {
			return calendar.RemoteEvent{}, fmt.Errorf("Google returned a different event identity after the update")
		}
		updated = googleCalendarUpdateEvent{}
		if err := calendarCreateJSON(ctx, http.MethodGet, endpoint, token, nil, &updated); err != nil {
			return calendar.RemoteEvent{}, fmt.Errorf("could not confirm the Google update: %v", err)
		}
	}
	restriction := updated.savedRestriction(draft)
	if occurrence {
		restriction = updated.occurrenceRestriction(existing.SeriesRemoteID)
		if restriction == "" && *updated.OriginalStartTime != *current.OriginalStartTime {
			restriction = "The occurrence identity changed."
		}
	}
	if updated.ID != existing.RemoteID || !calendarUpdateValidETag(updated.ETag, false) || restriction != "" {
		return calendar.RemoteEvent{}, fmt.Errorf("Google did not confirm a supported updated event and version")
	}
	event, err := normalizeGoogleCalendarEvent(updated.googleCalendarEvent)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	if draft.GuestsSet && !calendarMeetingGuestsMatch(event.Attendees, draft.Guests, event.OrganizerEmail) {
		return calendar.RemoteEvent{}, fmt.Errorf("Google did not confirm the submitted guests")
	}
	if (needsConfirmation || draft.Recurrence != nil || occurrence) && !calendarUpdateMatchesDraft(event, draft) {
		return calendar.RemoteEvent{}, fmt.Errorf("Google did not confirm the submitted event changes")
	}
	event.StartTimeZone, event.EndTimeZone = draft.TimeZone, draft.TimeZone
	return event, nil
}

type outlookCalendarUpdateEvent struct {
	outlookCalendarEvent
	ODataETag             string `json:"@odata.etag"`
	Type                  string `json:"type"`
	IsOnlineMeeting       bool   `json:"isOnlineMeeting"`
	OriginalStartTimeZone string `json:"originalStartTimeZone"`
}

func (remote outlookCalendarUpdateEvent) restriction() string {
	if reason := calendarUpdateRestriction(calendar.RemoteEvent{
		Deleted: remote.IsCancelled, SeriesRemoteID: remote.SeriesMasterID,
		Recurrence: remote.Recurrence, Attendees: remote.Attendees, OnlineMeeting: remote.OnlineMeeting,
		ResponseStatus: calendarOutlookOrganizerStatus(remote.outlookCalendarEvent),
	}); reason != "" {
		return reason
	}
	if remote.Type != "" && remote.Type != "singleInstance" {
		return "Recurring events and individual occurrences cannot be edited in Gofer yet."
	}
	if remote.IsOnlineMeeting || strings.TrimSpace(remote.OnlineMeetingURL) != "" || (remote.OnlineMeetingProvider != "" && remote.OnlineMeetingProvider != "unknown") {
		return "Online meetings cannot be edited in Gofer yet."
	}
	return ""
}

func updateOutlookCalendarEvent(ctx context.Context, token, remoteCalendarID string, existing storage.CalendarEvent, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
	return updateOutlookCalendarEventScope(ctx, token, remoteCalendarID, existing, draft, false)
}

func updateOutlookCalendarEventScope(ctx context.Context, token, remoteCalendarID string, existing storage.CalendarEvent, draft calendar.EventDraft, series bool, occurrenceScope ...bool) (calendar.RemoteEvent, error) {
	occurrence := calendarOccurrenceScope(occurrenceScope)
	if err := calendarUpdateExistingScopeRestriction(existing, series, occurrence); err != nil {
		return calendar.RemoteEvent{}, err
	}
	if occurrence && draft.Recurrence != nil {
		return calendar.RemoteEvent{}, errCalendarUpdateUnsupported
	}
	location, err := calendarUpdateDraftLocation(draft)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	endpoint := outlookGraphBaseURL + "/me/calendars/" + url.PathEscape(remoteCalendarID) + "/events/" + url.PathEscape(existing.RemoteID)
	var current outlookCalendarUpdateEvent
	if err := calendarCreateJSON(ctx, http.MethodGet, endpoint, token, nil, &current); err != nil {
		return calendar.RemoteEvent{}, calendarUpdateHTTPError(err)
	}
	if current.ID != existing.RemoteID {
		return calendar.RemoteEvent{}, fmt.Errorf("Microsoft did not return the requested event identity")
	}
	// The cache stores changeKey, but it is NOT an HTTP entity tag. Use the
	// fresh @odata.etag verbatim, including W/ and quotes; never guess it from
	// changeKey or strip the weak prefix. Microsoft documents this syntax at:
	// https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-odata/c3569037-0557-4769-8f75-a91ffcd7b05b
	if strings.TrimSpace(current.ChangeKey) == "" || !calendarUpdateValidETag(current.ODataETag, true) {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported("Microsoft did not supply a safe event version.")
	}
	version := strings.TrimSpace(existing.ETag)
	if version != current.ChangeKey && version != current.ODataETag {
		return calendar.RemoteEvent{}, errCalendarUpdateConflict
	}
	if err := calendarMeetingScopeRestriction(calendarOutlookOrganizerStatus(current.outlookCalendarEvent), current.Attendees, draft, series || occurrence || draft.Recurrence != nil); err != nil {
		return calendar.RemoteEvent{}, err
	}
	draft.OrganizerEmail = current.Organizer.EmailAddress.Address
	if err := calendarMeetingCheckSelfGuest(draft, draft.OrganizerEmail); err != nil {
		return calendar.RemoteEvent{}, err
	}
	if series {
		if draft.Recurrence == nil {
			return calendar.RemoteEvent{}, errCalendarUpdateUnsupported
		}
		if _, err := calendarOutlookSeriesEvent(current); err != nil {
			return calendar.RemoteEvent{}, err
		}
	} else if occurrence {
		if reason := current.occurrenceRestriction(existing.SeriesRemoteID); reason != "" {
			return calendar.RemoteEvent{}, calendarUpdateUnsupported(reason)
		}
	} else if reason := current.restriction(); reason != "" {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported(reason)
	}
	if _, err := normalizeOutlookCalendarEvent(current.outlookCalendarEvent); err != nil {
		return calendar.RemoteEvent{}, err
	}
	start, end := map[string]string{"timeZone": draft.TimeZone}, map[string]string{"timeZone": draft.TimeZone}
	if draft.AllDay {
		start["dateTime"], end["dateTime"] = draft.StartDate+"T00:00:00", draft.EndDate+"T00:00:00"
	} else {
		start["dateTime"], end["dateTime"] = draft.StartAt.In(location).Format("2006-01-02T15:04:05.999999999"), draft.EndAt.In(location).Format("2006-01-02T15:04:05.999999999")
	}
	payload := map[string]any{"subject": draft.Summary, "start": start, "end": end, "isAllDay": draft.AllDay}
	if draft.GuestsSet {
		payload["attendees"] = calendarMeetingOutlookGuests(draft, current.Attendees)
	}
	if draft.Recurrence != nil {
		payload["recurrence"] = calendarOutlookRecurrence(draft)
	}
	if draft.Description != calendarDescriptionText(existing.Description) {
		payload["body"] = outlookCalendarItemBody{ContentType: "text", Content: draft.Description}
	}
	if draft.Location != existing.Location {
		payload["location"] = map[string]string{"displayName": draft.Location}
	}
	var updated outlookCalendarUpdateEvent
	if err := calendarUpdateJSON(ctx, endpoint, token, current.ODataETag, payload, &updated); err != nil {
		return calendar.RemoteEvent{}, err
	}
	needsConfirmation := strings.TrimSpace(updated.ChangeKey) == ""
	if needsConfirmation {
		if updated.ID != "" && updated.ID != existing.RemoteID {
			return calendar.RemoteEvent{}, fmt.Errorf("Microsoft returned a different event identity after the update")
		}
		updated = outlookCalendarUpdateEvent{}
		if err := calendarCreateJSON(ctx, http.MethodGet, endpoint, token, nil, &updated); err != nil {
			return calendar.RemoteEvent{}, fmt.Errorf("could not confirm the Microsoft update: %v", err)
		}
	}
	restriction := updated.savedRestriction(draft)
	if occurrence {
		restriction = updated.occurrenceRestriction(existing.SeriesRemoteID)
	}
	if updated.ID != existing.RemoteID || strings.TrimSpace(updated.ChangeKey) == "" || restriction != "" {
		return calendar.RemoteEvent{}, fmt.Errorf("Microsoft did not confirm a supported updated event and version")
	}
	event, err := normalizeOutlookCalendarEvent(updated.outlookCalendarEvent)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	if draft.GuestsSet && !calendarMeetingGuestsMatch(event.Attendees, draft.Guests, event.OrganizerEmail) {
		return calendar.RemoteEvent{}, fmt.Errorf("Microsoft did not confirm the submitted guests")
	}
	if (needsConfirmation || draft.Recurrence != nil || occurrence) && !calendarUpdateMatchesDraft(event, draft) {
		return calendar.RemoteEvent{}, fmt.Errorf("Microsoft did not confirm the submitted event changes")
	}
	event.StartTimeZone, event.EndTimeZone = draft.TimeZone, draft.TimeZone
	return event, nil
}

func calendarUpdateCalDAVEndpoint(source storage.CalendarSource, existing storage.CalendarEvent) (string, error) {
	for _, raw := range []string{source.RemoteID, existing.RemoteID} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(raw, "#") {
			return "", calendarUpdateUnsupported("The calendar event does not have a safe HTTPS resource URL.")
		}
	}
	if _, err := resolveCalDAVHref(source.RemoteID, existing.RemoteID); err != nil {
		return "", calendarUpdateUnsupported("The calendar event belongs to a different server.")
	}
	return existing.RemoteID, nil // Keep the original escaped resource URL, including its UID-independent filename.
}

func calendarUpdateDecodeICS(raw []byte) (cal *ical.Calendar, err error) {
	// go-ical's parameter parser can panic on a truncated parameter. Remote
	// malformed data must fail safely before a write or during confirmation.
	defer func() {
		if recover() != nil {
			cal, err = nil, fmt.Errorf("CalDAV returned malformed calendar data")
		}
	}()
	decoder := ical.NewDecoder(bytes.NewReader(raw))
	cal, err = decoder.Decode()
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Decode(); err != io.EOF {
		return nil, fmt.Errorf("CalDAV returned additional or invalid calendar data")
	}
	return cal, nil
}

func calendarUpdateCalDAVRestriction(cal *ical.Calendar, headers http.Header) string {
	if len(cal.Events()) != 1 {
		return "Only calendar resources containing one appointment can be edited in Gofer."
	}
	if cal.Props.Get("METHOD") != nil || headers.Get("Schedule-Tag") != "" {
		return "Scheduling messages and invitations cannot be edited in Gofer."
	}
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent && child.Name != ical.CompTimezone {
			return "Only calendar resources containing one appointment can be edited in Gofer."
		}
	}
	props := cal.Events()[0].Props
	if status := props.Get("STATUS"); status != nil && strings.EqualFold(strings.TrimSpace(status.Value), "CANCELLED") {
		return "Cancelled events cannot be edited in Gofer."
	}
	for _, name := range []string{"RRULE", "EXRULE", "RDATE", "EXDATE", "RECURRENCE-ID"} {
		if props.Get(name) != nil {
			return "Recurring events and individual occurrences cannot be edited in Gofer yet."
		}
	}
	if props.Get("ATTENDEE") != nil || props.Get("ORGANIZER") != nil {
		return "Invitations and events with guests cannot be edited in Gofer yet."
	}
	for name, values := range props {
		if strings.Contains(name, "CONFERENCE") || strings.Contains(name, "ONLINEMEETING") || strings.Contains(name, "ONLINE-MEETING") || strings.Contains(name, "SKYPETEAMS") {
			return "Online meetings cannot be edited in Gofer yet."
		}
		for _, value := range values {
			for param := range value.Params {
				if strings.HasPrefix(param, "SCHEDULE-") {
					return "Scheduling messages and invitations cannot be edited in Gofer."
				}
			}
		}
	}
	return ""
}

func calendarUpdateCalDAVGet(ctx context.Context, client *http.Client, endpoint, username, password string) (*ical.Calendar, http.Header, error) {
	req, err := newCardDAVRequest(ctx, http.MethodGet, endpoint, username, password, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "text/calendar")
	response, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, nil, calendarUpdateHTTPError(calendarCreateProviderError{response.StatusCode})
	}
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("CalDAV did not return the requested event (HTTP %d)", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, calDAVResponseLimit+1))
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > calDAVResponseLimit {
		return nil, nil, fmt.Errorf("CalDAV event response exceeds size limit")
	}
	cal, err := calendarUpdateDecodeICS(raw)
	return cal, response.Header, err
}

func updateCalDAVCalendarEvent(ctx context.Context, source storage.CalendarSource, username, password string, existing storage.CalendarEvent, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
	return updateCalDAVCalendarEventScope(ctx, source, username, password, existing, draft, false)
}

func updateCalDAVCalendarEventScope(ctx context.Context, source storage.CalendarSource, username, password string, existing storage.CalendarEvent, draft calendar.EventDraft, series bool) (calendar.RemoteEvent, error) {
	if err := calendarUpdateExistingScopeRestriction(existing, series); err != nil {
		return calendar.RemoteEvent{}, err
	}
	version := strings.TrimSpace(existing.ETag)
	if !calendarUpdateValidETag(version, false) {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported("This calendar provider must supply a strong event version before editing.")
	}
	endpoint, err := calendarUpdateCalDAVEndpoint(source, existing)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	location, err := calendarUpdateDraftLocation(draft)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	// Use the creation helper for validated dates and escaped iCalendar TEXT,
	// but copy only editable properties into the full, freshly fetched resource.
	replacement, err := calendarCreateICS(draft)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	edited, err := calendarUpdateDecodeICS([]byte(replacement))
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	client := &http.Client{Transport: calDAVHTTPTransport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	current, headers, err := calendarUpdateCalDAVGet(ctx, client, endpoint, username, password)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	etag := headers.Get("ETag")
	if !calendarUpdateValidETag(etag, false) {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported("This calendar provider must supply a strong event version before editing.")
	}
	if etag != version {
		return calendar.RemoteEvent{}, errCalendarUpdateConflict
	}
	if !series {
		if reason := calendarUpdateCalDAVRestriction(current, headers); reason != "" {
			return calendar.RemoteEvent{}, calendarUpdateUnsupported(reason)
		}
	}
	currentLocation := location
	if source.TimeZone != "" {
		currentLocation, err = time.LoadLocation(source.TimeZone)
		if err != nil {
			return calendar.RemoteEvent{}, err
		}
	}
	var before calendar.RemoteEvent
	if series {
		if draft.Recurrence == nil {
			return calendar.RemoteEvent{}, errCalendarUpdateUnsupported
		}
		before, err = calendarCalDAVSeriesEvent(current, headers, endpoint, currentLocation)
	} else {
		before, err = normalizeCalDAVEvent(current.Events()[0], endpoint, etag, currentLocation)
	}
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	if existing.ICalUID != "" && before.ICalUID != existing.ICalUID {
		return calendar.RemoteEvent{}, errCalendarUpdateConflict
	}
	props := current.Events()[0].Props
	for _, name := range []string{"SUMMARY", "DESCRIPTION", "LOCATION", "DTSTART", "DTEND", "X-GOFER-TIMEZONE"} {
		if name == "DESCRIPTION" && draft.Description == calendarDescriptionText(existing.Description) {
			continue
		}
		props.Set(edited.Events()[0].Props.Get(name))
	}
	delete(props, "DURATION") // DTEND and DURATION are mutually exclusive.
	if draft.Recurrence != nil {
		props.Set(edited.Events()[0].Props.Get("RRULE"))
		// Retain reminders/metadata and any other timezone definitions. Replace
		// only the definition used by the newly zoned recurring appointment.
		for _, timezone := range edited.Children {
			if timezone.Name != ical.CompTimezone {
				continue
			}
			id, _ := timezone.Props.Text("TZID")
			children := current.Children[:0]
			for _, child := range current.Children {
				childID, _ := child.Props.Text("TZID")
				if child.Name != ical.CompTimezone || childID != id {
					children = append(children, child)
				}
			}
			current.Children = append(children, timezone)
		}
	}
	now := time.Now().UTC()
	props.SetDateTime("DTSTAMP", now)
	props.SetDateTime("LAST-MODIFIED", now)
	var body bytes.Buffer
	if err := ical.NewEncoder(&body).Encode(current); err != nil {
		return calendar.RemoteEvent{}, err
	}
	req, err := newCardDAVRequest(ctx, http.MethodPut, endpoint, username, password, &body)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
	req.Header.Set("If-Match", etag)
	response, err := client.Do(req)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	status, putETag := response.StatusCode, response.Header.Get("ETag")
	response.Body.Close()
	if status < 200 || status >= 300 {
		return calendar.RemoteEvent{}, calendarUpdateHTTPError(calendarCreateProviderError{status})
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm the update (HTTP %d)", status)
	}
	// RFC 4791 5.3.4 permits a transforming server to omit PUT's ETag. Read
	// back its actual representation and version. Every failure AFTER PUT is
	// uncertain, including a definite HTTP error from this confirmation GET.
	confirmed, headers, err := calendarUpdateCalDAVGet(ctx, client, endpoint, username, password)
	if err != nil {
		return calendar.RemoteEvent{}, fmt.Errorf("could not confirm the CalDAV update: %v", err)
	}
	etag = headers.Get("ETag")
	if !calendarUpdateValidETag(etag, false) {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm a supported updated event and version")
	}
	if calendarUpdateValidETag(putETag, false) && putETag != etag {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV event changed again before the update could be confirmed")
	}
	updated, err := calendarUpdateCalDAVConfirmedEvent(confirmed, headers, endpoint, etag, location, draft)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	if updated.RemoteID != existing.RemoteID || updated.ICalUID != before.ICalUID {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV returned a different event identity after the update")
	}
	if !calendarUpdateMatchesDraft(updated, draft) {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm the submitted event changes")
	}
	if zone, _ := confirmed.Events()[0].Props.Text("X-GOFER-TIMEZONE"); zone == draft.TimeZone {
		updated.StartTimeZone, updated.EndTimeZone = zone, zone
	}
	return updated, nil
}
