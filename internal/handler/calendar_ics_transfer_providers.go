package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

// The providers that import and export .ics files. CalDAV and JMAP
// calendars take an event's iCalendar as it is. Google's import takes each
// event as JSON under its iCalendar UID, which Google keeps. Microsoft's
// calendars have no import: each event is created through Graph, with the
// file's UID in a property of its own, so a second import finds it, and no
// guests, since Microsoft would send them invitations.

// outlookImportUIDProperty holds the UID an event had in the file it was
// imported from.
const outlookImportUIDProperty = "String {00020329-0000-0000-C000-000000000046} Name X-Gofer-ICalUID"

// calendarTransferProvider says whether a calendar's events can be imported
// and exported.
func calendarTransferProvider(provider string) bool {
	switch provider {
	case storage.CalendarSourceProviderCalDAV, storage.CalendarSourceProviderJMAP, providers.ProviderGmail, providers.ProviderOutlook:
		return true
	}
	return false
}

// importCalendarEvents adds each resource to the calendar as a new event,
// skipping those it already has. It stops at a failure that isn't about the
// one event, returning what it did. progress, when set, is told the counts
// after each event.
func importCalendarEvents(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials, resources []*ical.Calendar, progress func(calendarImportResult)) (calendarImportResult, error) {
	switch source.Provider {
	case storage.CalendarSourceProviderJMAP:
		// jmapCalendarDAV finds an event the calendar has by its UID.
		return importCalDAVEvents(withJMAPCalendarDAV(ctx, source, credentials), source, credentials, resources, nil, progress)
	case storage.CalendarSourceProviderCalDAV:
		existing, err := calDAVCalendarUIDs(ctx, source, credentials)
		var provider calendarCreateProviderError
		if errors.As(err, &provider) && calendarImportAborts(err) {
			return calendarImportResult{}, err
		}
		if err != nil {
			// The server refusing a UID it has, and asking it, still
			// tell them.
			log.Printf("calendar import %s: list UIDs: %v", source.ID, err)
			existing = nil
		}
		return importCalDAVEvents(ctx, source, credentials, resources, existing, progress)
	case providers.ProviderGmail:
		existing, err := googleCalendarUIDs(ctx, credentials.token, source.RemoteID)
		if err != nil {
			return calendarImportResult{}, err
		}
		return importEachCalendarEvent(ctx, resources, existing, progress, func(resource *ical.Calendar) error {
			return importGoogleCalendarEvent(ctx, credentials.token, source.RemoteID, resource)
		})
	case providers.ProviderOutlook:
		existing, err := outlookCalendarUIDs(ctx, credentials.token, source.RemoteID)
		if err != nil {
			return calendarImportResult{}, err
		}
		return importEachCalendarEvent(ctx, resources, existing, progress, func(resource *ical.Calendar) error {
			return importOutlookCalendarEvent(ctx, credentials.token, source.RemoteID, resource)
		})
	}
	return calendarImportResult{}, calendarTransferProblem("This calendar can't be imported into.")
}

// exportCalendarEvents reads every event in the calendar into one .ics file.
func exportCalendarEvents(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials) ([]byte, error) {
	switch source.Provider {
	case storage.CalendarSourceProviderCalDAV:
		return exportCalDAVEvents(ctx, source, credentials)
	case storage.CalendarSourceProviderJMAP:
		return exportJMAPCalendarEvents(ctx, source, credentials)
	case providers.ProviderGmail:
		return exportGoogleCalendarEvents(ctx, source, credentials.token)
	case providers.ProviderOutlook:
		return exportOutlookCalendarEvents(ctx, source, credentials.token)
	}
	return nil, calendarTransferProblem("This calendar can't be exported.")
}

// calendarImportUID is the UID of a resource's events.
func calendarImportUID(resource *ical.Calendar) string {
	for _, event := range resource.Events() {
		if uid, _ := event.Props.Text(ical.PropUID); uid != "" {
			return uid
		}
	}
	return ""
}

// importEachCalendarEvent runs add on each resource whose UID isn't in
// existing, counting the others already in the calendar, and failed those
// add fails with a reason about the one event.
func importEachCalendarEvent(ctx context.Context, resources []*ical.Calendar, existing map[string]bool, progress func(calendarImportResult), add func(*ical.Calendar) error) (calendarImportResult, error) {
	var result calendarImportResult
	for _, resource := range resources {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if existing[calendarImportUID(resource)] {
			result.Existing++
			if progress != nil {
				progress(result)
			}
			continue
		}
		err := add(resource)
		switch {
		case err != nil && calendarImportAborts(err):
			return result, err
		case err != nil:
			log.Printf("calendar import: event not imported: %v", err)
			result.fail(resource, err)
		default:
			result.Added++
		}
		if progress != nil {
			progress(result)
		}
	}
	return result, nil
}

// calendarImportAborts says whether a failure is about the import as a
// whole, such as an expired sign-in, a rate limit or an outage, rather than
// about the one event.
func calendarImportAborts(err error) bool {
	var problem calendarTransferProblem
	if errors.As(err, &problem) {
		return false
	}
	var provider calendarCreateProviderError
	if errors.As(err, &provider) {
		status := provider.Status
		// Google answers a rate limit with 403.
		return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests || status >= 500
	}
	return true
}

// calendarProviderJSON is calendarCreateJSON with more Prefer headers, such
// as for the time zone Microsoft gives times in.
func calendarProviderJSON(ctx context.Context, method, endpoint, token string, payload, out any, prefer ...string) error {
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
	for _, value := range prefer {
		req.Header.Add("Prefer", value)
	}
	client := &http.Client{Transport: netguard.APITransport, Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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
		return fmt.Errorf("calendar provider response exceeds size limit")
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// exportJMAPCalendarEvents reads every event of a JMAP calendar.
func exportJMAPCalendarEvents(ctx context.Context, source storage.CalendarSource, credentials calendarCredentials) ([]byte, error) {
	calendarID, name, ok := jmapCalendarDAVParse(source.RemoteID)
	if !ok || name != "" || credentials.jmap == nil {
		return nil, errors.New("JMAP calendar has no valid id or sign-in")
	}
	pulled, err := mail.PullJMAPCalendarEvents(ctx, credentials.jmap, credentials.password, calendarID, time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}
	var files []string
	for _, event := range pulled.Events {
		if !jmapCalendarEventIn(event, calendarID) {
			continue
		}
		data, err := mail.JMAPEventICS(event)
		if err != nil {
			log.Printf("calendar export: JMAP event %s: %v", event.ID(), err)
			continue
		}
		files = append(files, string(data))
	}
	return calendarExportFile(source.Name, files)
}

// calendarImportEvent is one event of an imported resource: its series, or
// a date of the series changed on its own (RecurrenceID set).
type calendarImportEvent struct {
	component    *ical.Component
	uid          string
	start, end   calendarImportTime
	recurrenceID *calendarImportTime
}

// calendarImportTime is a start, end or date of a series as the providers
// take it.
type calendarImportTime struct {
	allDay bool
	date   string // 2006-01-02, for an all-day time
	at     time.Time
	zone   *time.Location // UTC when the file gave none
}

// readCalendarImportTime reads a DATE or DATE-TIME property, in its TZID
// zone when Go knows it and UTC otherwise.
func readCalendarImportTime(prop *ical.Prop) (calendarImportTime, error) {
	if prop == nil {
		return calendarImportTime{}, errors.New("the event has no start")
	}
	value := strings.TrimSpace(prop.Value)
	if prop.ValueType() == ical.ValueDate || len(value) == len("20060102") {
		day, err := time.Parse("20060102", value)
		if err != nil {
			return calendarImportTime{}, err
		}
		return calendarImportTime{allDay: true, date: day.Format("2006-01-02"), at: day, zone: time.UTC}, nil
	}
	zone := time.UTC
	if tzid := prop.Params.Get(ical.PropTimezoneID); tzid != "" && !strings.HasSuffix(value, "Z") {
		if location, err := time.LoadLocation(tzid); err == nil {
			zone = location
		} else if location := outlookCalendarTimeLocation(tzid); location != time.UTC {
			zone = location
		}
	}
	layout := "20060102T150405"
	if strings.HasSuffix(value, "Z") {
		layout = "20060102T150405Z"
	}
	at, err := time.ParseInLocation(layout, value, zone)
	if err != nil {
		return calendarImportTime{}, err
	}
	if calendarZoneIsUTC(zone) {
		zone = time.UTC
	}
	return calendarImportTime{at: at, zone: zone}, nil
}

// readCalendarImportResource splits a resource into its series (nil when
// the file has only changed dates of it) and its changed dates.
func readCalendarImportResource(resource *ical.Calendar) (*calendarImportEvent, []calendarImportEvent, error) {
	var series *calendarImportEvent
	var changed []calendarImportEvent
	for _, component := range resource.Children {
		if component.Name != ical.CompEvent {
			continue
		}
		event := calendarImportEvent{component: component}
		event.uid, _ = component.Props.Text(ical.PropUID)
		var err error
		if event.start, err = readCalendarImportTime(component.Props.Get(ical.PropDateTimeStart)); err != nil {
			return nil, nil, err
		}
		event.end = event.start
		if end := component.Props.Get(ical.PropDateTimeEnd); end != nil {
			if event.end, err = readCalendarImportTime(end); err != nil {
				return nil, nil, err
			}
		} else if duration := component.Props.Get(ical.PropDuration); duration != nil {
			length, err := duration.Duration()
			if err != nil {
				return nil, nil, err
			}
			event.end.at = event.start.at.Add(length)
			event.end.date = event.end.at.Format("2006-01-02")
		} else if event.start.allDay {
			event.end.at = event.start.at.AddDate(0, 0, 1)
			event.end.date = event.end.at.Format("2006-01-02")
		}
		if event.start.allDay && !event.end.at.After(event.start.at) {
			event.end.at = event.start.at.AddDate(0, 0, 1)
			event.end.date = event.end.at.Format("2006-01-02")
		}
		if event.end.at.Before(event.start.at) {
			event.end = event.start
		}
		if id := component.Props.Get(ical.PropRecurrenceID); id != nil {
			date, err := readCalendarImportTime(id)
			if err != nil {
				return nil, nil, err
			}
			event.recurrenceID = &date
			changed = append(changed, event)
			continue
		}
		if series == nil {
			series = &event
		}
	}
	return series, changed, nil
}

// calendarImportText is a text property, "" when it is missing.
func calendarImportText(component *ical.Component, name string) string {
	value, _ := component.Props.Text(name)
	return strings.TrimSpace(value)
}

// calendarImportLines are the event's repeat rules and dates as iCalendar
// lines, such as "RRULE:FREQ=WEEKLY;BYDAY=MO".
func calendarImportLines(component *ical.Component) ([]string, error) {
	event := ical.NewEvent()
	event.Props.SetText(ical.PropUID, "rule")
	event.Props.SetDateTime(ical.PropDateTimeStamp, time.Unix(0, 0).UTC())
	found := false
	for _, name := range []string{ical.PropRecurrenceRule, ical.PropRecurrenceDates, ical.PropExceptionDates} {
		for _, prop := range component.Props[name] {
			event.Props.Add(&prop)
			found = true
		}
	}
	if !found {
		return nil, nil
	}
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//Gofer//Calendar//EN")
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Children = append(cal.Children, event.Component)
	var buffer bytes.Buffer
	if err := ical.NewEncoder(&buffer).Encode(cal); err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(buffer.String(), "\r\n ", ""), "\r\n") {
		name := strings.ToUpper(strings.SplitN(strings.SplitN(line, ":", 2)[0], ";", 2)[0])
		if name == ical.PropRecurrenceRule || name == ical.PropRecurrenceDates || name == ical.PropExceptionDates {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// calendarImportReminder is one of an event's reminders: minutes before
// it starts, and whether it is sent by email rather than shown.
type calendarImportReminder struct {
	minutes int
	email   bool
}

// calendarImportReminders are the event's reminders that come before or at
// its start, earliest last, and whether it has any VALARM at all; an event
// with none gets the calendar's default.
func calendarImportReminders(event calendarImportEvent) ([]calendarImportReminder, bool) {
	var out []calendarImportReminder
	seen := map[calendarImportReminder]bool{}
	alarms := false
	for _, child := range event.component.Children {
		if child.Name != ical.CompAlarm {
			continue
		}
		alarms = true
		trigger := child.Props.Get(ical.PropTrigger)
		if trigger == nil {
			continue
		}
		var before time.Duration
		if trigger.ValueType() == ical.ValueDateTime {
			at, err := trigger.DateTime(time.UTC)
			if err != nil {
				continue
			}
			before = event.start.at.Sub(at)
		} else {
			offset, err := trigger.Duration()
			if err != nil {
				continue
			}
			before = -offset
			if strings.EqualFold(trigger.Params.Get(ical.ParamRelated), "END") {
				before -= event.end.at.Sub(event.start.at)
			}
		}
		if before < 0 || before > 28*24*time.Hour {
			continue // After the start, or further ahead than Google takes.
		}
		action, _ := child.Props.Text(ical.PropAction)
		reminder := calendarImportReminder{minutes: int(before / time.Minute), email: strings.EqualFold(strings.TrimSpace(action), "EMAIL")}
		if !seen[reminder] {
			seen[reminder] = true
			out = append(out, reminder)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].minutes > out[j].minutes })
	return out, alarms
}

// calendarExportAlarm is a reminder shown minutes before the event starts.
func calendarExportAlarm(minutes int) *ical.Component {
	alarm := ical.NewComponent(ical.CompAlarm)
	alarm.Props.SetText(ical.PropAction, "DISPLAY")
	alarm.Props.SetText(ical.PropDescription, "Reminder")
	trigger := ical.NewProp(ical.PropTrigger)
	trigger.Value = fmt.Sprintf("-PT%dM", minutes)
	if minutes == 0 {
		trigger.Value = "PT0M"
	}
	alarm.Props.Set(trigger)
	return alarm
}

// calendarExportSeries is the VEVENT of cal's series, not a changed date.
func calendarExportSeries(cal *ical.Calendar) *ical.Component {
	for _, child := range cal.Children {
		if child.Name == ical.CompEvent && child.Props.Get(ical.PropRecurrenceID) == nil {
			return child
		}
	}
	return nil
}

// Google

// googleImportTime is a time as Google's events take it. A series needs a
// zone, which is UTC when the file gave none.
func googleImportTime(at calendarImportTime, series bool) map[string]string {
	if at.allDay {
		return map[string]string{"date": at.date}
	}
	out := map[string]string{"dateTime": at.at.Format(time.RFC3339)}
	if at.zone != time.UTC || series {
		out["timeZone"] = at.zone.String()
	}
	return out
}

// googleImportPayload is an event as Google's import takes it.
func googleImportPayload(event calendarImportEvent, lines []string) map[string]any {
	component := event.component
	payload := map[string]any{
		"iCalUID": event.uid,
		"summary": calendarImportText(component, ical.PropSummary),
		"start":   googleImportTime(event.start, len(lines) > 0 || event.recurrenceID != nil),
		"end":     googleImportTime(event.end, len(lines) > 0 || event.recurrenceID != nil),
	}
	if description := calendarImportText(component, ical.PropDescription); description != "" {
		payload["description"] = description
	}
	if location := calendarImportText(component, ical.PropLocation); location != "" {
		payload["location"] = location
	}
	if len(lines) > 0 {
		payload["recurrence"] = lines
	}
	switch strings.ToUpper(calendarImportText(component, ical.PropStatus)) {
	case "TENTATIVE":
		payload["status"] = "tentative"
	case "CANCELLED":
		payload["status"] = "cancelled"
	}
	if strings.EqualFold(calendarImportText(component, ical.PropTransparency), "TRANSPARENT") {
		payload["transparency"] = "transparent"
	}
	switch strings.ToUpper(calendarImportText(component, ical.PropClass)) {
	case "PRIVATE", "CONFIDENTIAL":
		payload["visibility"] = "private"
	}
	if organizer := component.Props.Get(ical.PropOrganizer); organizer != nil {
		if email := calendarReplyAddress(organizer.Value); email != "" {
			person := map[string]string{"email": email}
			if name := organizer.Params.Get(ical.ParamCommonName); name != "" {
				person["displayName"] = name
			}
			payload["organizer"] = person
		}
	}
	var attendees []map[string]any
	for _, attendee := range component.Props[ical.PropAttendee] {
		email := calendarReplyAddress(attendee.Value)
		if email == "" {
			continue
		}
		person := map[string]any{"email": email}
		if name := attendee.Params.Get(ical.ParamCommonName); name != "" {
			person["displayName"] = name
		}
		status := map[string]string{"ACCEPTED": "accepted", "DECLINED": "declined", "TENTATIVE": "tentative"}[strings.ToUpper(attendee.Params.Get(ical.ParamParticipationStatus))]
		if status == "" {
			status = "needsAction"
		}
		person["responseStatus"] = status
		if strings.EqualFold(attendee.Params.Get(ical.ParamRole), "OPT-PARTICIPANT") {
			person["optional"] = true
		}
		attendees = append(attendees, person)
	}
	if len(attendees) > 0 {
		payload["attendees"] = attendees
	}
	if event.recurrenceID != nil {
		payload["originalStartTime"] = googleImportTime(*event.recurrenceID, true)
	}
	if reminders, alarms := calendarImportReminders(event); alarms {
		// Google keeps five, and an event with alarms none of which it can
		// keep has none rather than the calendar's.
		overrides := []map[string]any{}
		for _, reminder := range reminders {
			if len(overrides) == 5 {
				break
			}
			method := "popup"
			if reminder.email {
				method = "email"
			}
			overrides = append(overrides, map[string]any{"method": method, "minutes": reminder.minutes})
		}
		payload["reminders"] = map[string]any{"useDefault": false, "overrides": overrides}
	}
	return payload
}

// googleCalendarUIDs are the iCalendar UIDs of a Google calendar's events.
func googleCalendarUIDs(ctx context.Context, token, remoteCalendarID string) (map[string]bool, error) {
	uids := map[string]bool{}
	values := url.Values{"maxResults": {"2500"}, "singleEvents": {"false"}, "fields": {"items(iCalUID),nextPageToken"}}
	endpoint := googleCalendarAPIBaseURL + "/calendars/" + url.PathEscape(remoteCalendarID) + "/events?"
	for page := 0; ; page++ {
		if page >= calendarSyncMaxPages {
			return nil, errors.New("Google Calendar UID listing exceeded its page limit")
		}
		var listed struct {
			Items []struct {
				ICalUID string `json:"iCalUID"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := calendarProviderJSON(ctx, http.MethodGet, endpoint+values.Encode(), token, nil, &listed); err != nil {
			return nil, err
		}
		for _, item := range listed.Items {
			uids[item.ICalUID] = true
		}
		if listed.NextPageToken == "" {
			return uids, nil
		}
		values.Set("pageToken", listed.NextPageToken)
	}
}

// importGoogleCalendarEvent imports one resource into a Google calendar.
// Import sends no invitations; Google keeps the guests as they were.
func importGoogleCalendarEvent(ctx context.Context, token, remoteCalendarID string, resource *ical.Calendar) error {
	series, changed, err := readCalendarImportResource(resource)
	if err != nil {
		return calendarTransferProblem(err.Error())
	}
	if series == nil {
		return calendarTransferProblem("the file has changed dates of a repeating event without the event itself")
	}
	events := googleCalendarAPIBaseURL + "/calendars/" + url.PathEscape(remoteCalendarID) + "/events"
	lines, err := calendarImportLines(series.component)
	if err != nil {
		return calendarTransferProblem(err.Error())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := calendarProviderJSON(ctx, http.MethodPost, events+"/import", token, googleImportPayload(*series, lines), &created); err != nil {
		return err
	}
	for _, date := range changed {
		if err := calendarProviderJSON(ctx, http.MethodPost, events+"/import", token, googleImportPayload(date, nil), nil); err != nil {
			if calendarImportAborts(err) {
				return err
			}
			log.Printf("calendar import: Google changed date of %s: %v", series.uid, err)
		}
	}
	return nil
}

// exportGoogleCalendarEvents reads every event of a Google calendar, each
// series with its changed and deleted dates.
func exportGoogleCalendarEvents(ctx context.Context, source storage.CalendarSource, token string) ([]byte, error) {
	var series []googleCalendarEvent
	var defaults []googleCalendarReminder
	changed := map[string][]googleCalendarUpdateEvent{}
	values := url.Values{"maxResults": {"250"}, "showDeleted": {"true"}, "singleEvents": {"false"}}
	endpoint := googleCalendarAPIBaseURL + "/calendars/" + url.PathEscape(source.RemoteID) + "/events?"
	for page := 0; ; page++ {
		if page >= calendarSyncMaxPages {
			return nil, errors.New("Google Calendar export exceeded its page limit")
		}
		var listed struct {
			Items            []googleCalendarUpdateEvent `json:"items"`
			NextPageToken    string                      `json:"nextPageToken"`
			DefaultReminders []googleCalendarReminder    `json:"defaultReminders"`
		}
		if err := calendarProviderJSON(ctx, http.MethodGet, endpoint+values.Encode(), token, nil, &listed); err != nil {
			return nil, err
		}
		if listed.DefaultReminders != nil {
			defaults = listed.DefaultReminders
		}
		for _, item := range listed.Items {
			switch {
			case item.ExtendedProperties.Private["goferMeetDraft"] == "true":
			case item.RecurringEventID != "":
				changed[item.RecurringEventID] = append(changed[item.RecurringEventID], item)
			case item.Status != "cancelled":
				series = append(series, item.googleCalendarEvent)
			}
		}
		if listed.NextPageToken == "" {
			break
		}
		values.Set("pageToken", listed.NextPageToken)
	}
	var files []string
	for _, remote := range series {
		cal, err := googleCalendarForwardCalendar(remote, changed[remote.ID])
		if err != nil {
			log.Printf("calendar export: Google event %s: %v", remote.ID, err)
			continue
		}
		reminders := remote.Reminders.Overrides
		if remote.Reminders.UseDefault {
			reminders = defaults
		}
		if event := calendarExportSeries(cal); event != nil {
			for _, reminder := range reminders {
				event.Children = append(event.Children, calendarExportAlarm(reminder.Minutes))
			}
		}
		data, err := calendarExportPersonalEvents(cal)
		if err != nil {
			log.Printf("calendar export: Google event %s: %v", remote.ID, err)
			continue
		}
		files = append(files, data)
	}
	return calendarExportFile(source.Name, files)
}

// calendarExportPersonalEvents encodes cal, leaving out the organizer of an
// event with no guests, which is no meeting.
func calendarExportPersonalEvents(cal *ical.Calendar) (string, error) {
	for _, event := range cal.Children {
		if event.Name == ical.CompEvent && len(event.Props[ical.PropAttendee]) == 0 {
			event.Props.Del(ical.PropOrganizer)
		}
	}
	var buffer bytes.Buffer
	if err := ical.NewEncoder(&buffer).Encode(cal); err != nil {
		return "", err
	}
	return buffer.String(), nil
}

// Microsoft

var outlookImportIndexes = map[int]string{1: "first", 2: "second", 3: "third", 4: "fourth", -1: "last"}

// outlookImportRecurrence is an RRULE as Microsoft's pattern and range, or
// an error when Microsoft has no pattern like it.
func outlookImportRecurrence(rule string, start calendarImportTime) (map[string]any, error) {
	option, err := rrule.StrToROption(rule)
	if err != nil {
		return nil, err
	}
	unsupported := calendarTransferProblem("Microsoft calendars have no repeat pattern like its " + rule)
	if len(option.Byhour) > 0 || len(option.Byminute) > 0 || len(option.Bysecond) > 0 || len(option.Byyearday) > 0 || len(option.Byweekno) > 0 || len(option.Byeaster) > 0 || len(option.Bysetpos) > 1 || len(option.Bymonth) > 1 || len(option.Bymonthday) > 1 {
		return nil, unsupported
	}
	interval := option.Interval
	if interval < 1 {
		interval = 1
	}
	local := start.at.In(start.zone)
	pattern := map[string]any{"interval": interval}
	var days []string
	index := 0
	for _, day := range option.Byweekday {
		days = append(days, strings.ToLower(time.Weekday((day.Day()+1)%7).String()))
		if day.N() != 0 {
			if index != 0 && index != day.N() {
				return nil, unsupported
			}
			index = day.N()
		}
	}
	if len(option.Bysetpos) == 1 {
		if index != 0 {
			return nil, unsupported
		}
		index = option.Bysetpos[0]
	}
	if index != 0 && outlookImportIndexes[index] == "" {
		return nil, unsupported
	}
	monthDay := local.Day()
	if len(option.Bymonthday) == 1 {
		if monthDay = option.Bymonthday[0]; monthDay < 1 {
			return nil, unsupported
		}
	}
	month := int(local.Month())
	if len(option.Bymonth) == 1 {
		month = option.Bymonth[0]
	}
	switch option.Freq {
	case rrule.DAILY:
		if len(days) > 0 {
			if index != 0 || interval != 1 {
				return nil, unsupported
			}
			pattern["type"], pattern["daysOfWeek"] = "weekly", days
		} else {
			pattern["type"] = "daily"
		}
	case rrule.WEEKLY:
		if index != 0 || len(option.Bymonthday) > 0 || len(option.Bymonth) > 0 {
			return nil, unsupported
		}
		if len(days) == 0 {
			days = []string{strings.ToLower(local.Weekday().String())}
		}
		pattern["type"], pattern["daysOfWeek"] = "weekly", days
		pattern["firstDayOfWeek"] = strings.ToLower(time.Weekday((option.Wkst.Day() + 1) % 7).String())
	case rrule.MONTHLY, rrule.YEARLY:
		kind := "Monthly"
		if option.Freq == rrule.YEARLY {
			kind = "Yearly"
			pattern["month"] = month
		} else if len(option.Bymonth) > 0 {
			return nil, unsupported
		}
		if len(days) > 0 {
			if index == 0 || len(option.Bymonthday) > 0 {
				return nil, unsupported
			}
			pattern["type"], pattern["daysOfWeek"], pattern["index"] = "relative"+kind, days, outlookImportIndexes[index]
		} else {
			pattern["type"], pattern["dayOfMonth"] = "absolute"+kind, monthDay
		}
	default:
		return nil, unsupported
	}
	zone := start.zone.String()
	repeat := map[string]any{"type": "noEnd", "startDate": local.Format("2006-01-02"), "recurrenceTimeZone": zone}
	switch {
	case option.Count > 0:
		repeat["type"], repeat["numberOfOccurrences"] = "numbered", option.Count
	case !option.Until.IsZero():
		until := option.Until
		if !strings.HasSuffix(outlookImportRulePart(rule, "UNTIL"), "Z") {
			// A floating or all-day UNTIL is a date in the event's zone.
			until = time.Date(until.Year(), until.Month(), until.Day(), 0, 0, 0, 0, start.zone)
		}
		// Microsoft ends a series on a date: the last one whose occurrence
		// would still start by UNTIL.
		last := until.In(start.zone)
		if !start.allDay {
			clock := func(at time.Time) int { return at.Hour()*3600 + at.Minute()*60 + at.Second() }
			if clock(last) < clock(local) {
				last = last.AddDate(0, 0, -1)
			}
		}
		repeat["type"], repeat["endDate"] = "endDate", last.Format("2006-01-02")
	}
	return map[string]any{"pattern": pattern, "range": repeat}, nil
}

// outlookImportRulePart is the value of one part of an RRULE, such as UNTIL.
func outlookImportRulePart(rule, name string) string {
	for _, part := range strings.Split(rule, ";") {
		if key, value, ok := strings.Cut(part, "="); ok && strings.EqualFold(key, name) {
			return strings.ToUpper(strings.TrimSpace(value))
		}
	}
	return ""
}

// outlookAllDayDate is the date of an all-day event's original start,
// which Microsoft may give as midnight in another zone: the nearest
// midnight's date.
func outlookAllDayDate(at time.Time) time.Time {
	at = at.UTC().Add(12 * time.Hour)
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
}

// outlookImportTime is a time as Microsoft's events take it.
func outlookImportTime(at calendarImportTime) map[string]string {
	if at.allDay {
		return map[string]string{"dateTime": at.date + "T00:00:00", "timeZone": "UTC"}
	}
	return map[string]string{"dateTime": at.at.In(at.zone).Format("2006-01-02T15:04:05"), "timeZone": at.zone.String()}
}

// outlookImportPayload is an event's details as Microsoft takes them, for
// a new event or a changed date of a series.
func outlookImportPayload(event calendarImportEvent) map[string]any {
	component := event.component
	payload := map[string]any{
		"subject":  calendarImportText(component, ical.PropSummary),
		"body":     map[string]string{"contentType": "text", "content": outlookImportDescription(component)},
		"location": map[string]string{"displayName": calendarImportText(component, ical.PropLocation)},
		"start":    outlookImportTime(event.start),
		"end":      outlookImportTime(event.end),
		"isAllDay": event.start.allDay,
		"showAs":   "busy",
	}
	if strings.EqualFold(calendarImportText(component, ical.PropTransparency), "TRANSPARENT") {
		payload["showAs"] = "free"
	} else if strings.EqualFold(calendarImportText(component, ical.PropStatus), "TENTATIVE") {
		payload["showAs"] = "tentative"
	}
	switch strings.ToUpper(calendarImportText(component, ical.PropClass)) {
	case "PRIVATE", "CONFIDENTIAL":
		payload["sensitivity"] = "private"
	}
	if reminders, alarms := calendarImportReminders(event); alarms {
		// Microsoft keeps one reminder: the earliest.
		payload["isReminderOn"] = len(reminders) > 0
		if len(reminders) > 0 {
			payload["reminderMinutesBeforeStart"] = reminders[0].minutes
		}
	}
	return payload
}

// outlookImportDescription is the event's description, with its organizer
// and guests written after it: Microsoft would send guests invitations,
// so a meeting comes in with them as text. Their answers aren't written,
// since they may have changed since the file was made.
func outlookImportDescription(component *ical.Component) string {
	description := calendarImportText(component, ical.PropDescription)
	person := func(prop ical.Prop) string {
		email := calendarReplyAddress(prop.Value)
		name := strings.TrimSpace(prop.Params.Get(ical.ParamCommonName))
		switch {
		case name != "" && email != "" && !strings.EqualFold(name, email):
			return name + " (" + email + ")"
		case email != "":
			return email
		}
		return name
	}
	organizer := ""
	organizerEmail := ""
	if prop := component.Props.Get(ical.PropOrganizer); prop != nil {
		organizer, organizerEmail = person(*prop), calendarReplyAddress(prop.Value)
	}
	var guests []string
	for _, prop := range component.Props[ical.PropAttendee] {
		if organizerEmail != "" && strings.EqualFold(calendarReplyAddress(prop.Value), organizerEmail) {
			continue
		}
		if guest := person(prop); guest != "" {
			guests = append(guests, guest)
		}
	}
	if len(guests) == 0 {
		return description // No meeting, whoever it names as organizer.
	}
	var lines []string
	if organizer != "" {
		lines = append(lines, "Organizer: "+organizer)
	}
	lines = append(lines, "Guests: "+strings.Join(guests, ", "))
	if description != "" {
		description += "\n\n"
	}
	return description + strings.Join(lines, "\n")
}

// outlookODataString quotes a value for an OData filter.
func outlookODataString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// outlookCalendarUIDs are the UIDs of a Microsoft calendar's events: the
// UID of the file each was imported from, and Microsoft's own, which an
// export of the calendar gives an event made in it.
func outlookCalendarUIDs(ctx context.Context, token, remoteCalendarID string) (map[string]bool, error) {
	uids := map[string]bool{}
	events := outlookGraphBaseURL + "/me/calendars/" + url.PathEscape(remoteCalendarID) + "/events"
	expand := "singleValueExtendedProperties($filter=id eq " + outlookODataString(outlookImportUIDProperty) + ")"
	query := url.Values{"$top": {"1000"}, "$select": {"id,iCalUId"}, "$expand": {expand}}
	next := events + "?" + query.Encode()
	for page := 0; next != ""; page++ {
		if page >= calendarSyncMaxPages {
			return nil, errors.New("Microsoft calendar UID listing exceeded its page limit")
		}
		var listed struct {
			Value    []outlookCalendarEvent `json:"value"`
			NextLink string                 `json:"@odata.nextLink"`
		}
		if err := calendarProviderJSON(ctx, http.MethodGet, next, token, nil, &listed); err != nil {
			return nil, err
		}
		for _, event := range listed.Value {
			if uid := strings.TrimSpace(event.ICalUID); uid != "" {
				uids[uid] = true
			}
			for _, prop := range event.SingleValueExtendedProperties {
				if strings.EqualFold(prop.ID, outlookImportUIDProperty) && strings.TrimSpace(prop.Value) != "" {
					uids[strings.TrimSpace(prop.Value)] = true
				}
			}
		}
		next = listed.NextLink
		if next != "" && !strings.HasPrefix(next, events) {
			return nil, errors.New("Microsoft gave a next page outside the calendar")
		}
	}
	return uids, nil
}

// importOutlookCalendarEvent creates one resource as a Microsoft event, and
// then changes or deletes the dates of a series the file changed or left
// out.
func importOutlookCalendarEvent(ctx context.Context, token, remoteCalendarID string, resource *ical.Calendar) error {
	series, changed, err := readCalendarImportResource(resource)
	if err != nil {
		return calendarTransferProblem(err.Error())
	}
	if series == nil {
		return calendarTransferProblem("the file has changed dates of a repeating event without the event itself")
	}
	if strings.EqualFold(calendarImportText(series.component, ical.PropStatus), "CANCELLED") {
		return calendarTransferProblem("the event is cancelled")
	}
	events := outlookGraphBaseURL + "/me/calendars/" + url.PathEscape(remoteCalendarID) + "/events"
	payload := outlookImportPayload(*series)
	payload["singleValueExtendedProperties"] = []map[string]string{{"id": outlookImportUIDProperty, "value": series.uid}}
	readDates := func(name string) []calendarImportTime {
		var dates []calendarImportTime
		for _, prop := range series.component.Props[name] {
			if strings.EqualFold(prop.Params.Get(ical.ParamValue), "PERIOD") {
				continue
			}
			for _, value := range strings.Split(prop.Value, ",") {
				date := prop
				date.Value = strings.TrimSpace(value)
				if at, err := readCalendarImportTime(&date); err == nil {
					dates = append(dates, at)
				}
			}
		}
		return dates
	}
	excluded := readDates(ical.PropExceptionDates)
	// Microsoft's series have no extra dates, so each is an event of its
	// own, as the date's changed copy has it when the file has one, unless
	// the file deleted it.
	var extra []calendarImportEvent
	for _, at := range readDates(ical.PropRecurrenceDates) {
		deleted := false
		excluded = slices.DeleteFunc(excluded, func(other calendarImportTime) bool {
			// Not a date of the series to delete from it.
			same := calendarImportSameTime(at, other)
			deleted = deleted || same
			return same
		})
		if deleted || calendarImportSameTime(at, series.start) {
			continue // One the file deleted, or the series' own first date.
		}
		event := *series
		event.start, event.end = at, at
		event.end.at = at.at.Add(series.end.at.Sub(series.start.at))
		event.end.date = event.end.at.Format("2006-01-02")
		cancelled := false
		changed = slices.DeleteFunc(changed, func(date calendarImportEvent) bool {
			if !calendarImportSameTime(*date.recurrenceID, at) {
				return false
			}
			event = date
			cancelled = strings.EqualFold(calendarImportText(date.component, ical.PropStatus), "CANCELLED")
			return true
		})
		if !cancelled {
			event.recurrenceID = &at
			extra = append(extra, event)
		}
	}
	if rules := series.component.Props[ical.PropRecurrenceRule]; len(rules) > 0 {
		if len(rules) > 1 {
			return calendarTransferProblem("Microsoft calendars can't repeat an event by more than one rule")
		}
		recurrence, err := outlookImportRecurrence(rules[0].Value, series.start)
		if err != nil {
			return calendarTransferProblem(err.Error())
		}
		if series.start.allDay {
			recurrence["range"].(map[string]any)["recurrenceTimeZone"] = "UTC"
		}
		payload["recurrence"] = recurrence
	} else {
		changed, excluded = nil, nil
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := calendarProviderJSON(ctx, http.MethodPost, events, token, payload, &created); err != nil {
		return err
	}
	if created.ID == "" {
		return errors.New("Microsoft did not return the imported event's id")
	}
	// The series' dates the file deleted or changed on their own.
	for _, at := range excluded {
		if err := outlookImportChangeDate(ctx, token, events, created.ID, at, nil); err != nil {
			if calendarImportAborts(err) {
				return err
			}
			log.Printf("calendar import: Microsoft deleted date of %s: %v", series.uid, err)
		}
	}
	for _, date := range changed {
		var change map[string]any
		if !strings.EqualFold(calendarImportText(date.component, ical.PropStatus), "CANCELLED") {
			change = outlookImportPayload(date)
		}
		if err := outlookImportChangeDate(ctx, token, events, created.ID, *date.recurrenceID, change); err != nil {
			if calendarImportAborts(err) {
				return err
			}
			log.Printf("calendar import: Microsoft changed date of %s: %v", series.uid, err)
		}
	}
	for _, date := range extra {
		// Each under a UID of its own, so a second import finds it.
		uid := series.uid + "#rdate-" + date.recurrenceID.date
		if !date.recurrenceID.allDay {
			uid = series.uid + "#rdate-" + date.recurrenceID.at.UTC().Format("20060102T150405Z")
		}
		payload := outlookImportPayload(date)
		payload["singleValueExtendedProperties"] = []map[string]string{{"id": outlookImportUIDProperty, "value": uid}}
		if err := calendarProviderJSON(ctx, http.MethodPost, events, token, payload, nil); err != nil {
			if calendarImportAborts(err) {
				return err
			}
			log.Printf("calendar import: Microsoft extra date of %s: %v", series.uid, err)
		}
	}
	return nil
}

// calendarImportSameTime says whether two dates of a series are the same
// one: the same day for all-day ones, the same moment otherwise.
func calendarImportSameTime(a, b calendarImportTime) bool {
	if a.allDay || b.allDay {
		return a.at.Format("2006-01-02") == b.at.Format("2006-01-02")
	}
	return a.at.Equal(b.at)
}

// outlookImportChangeDate changes one date of a series, or deletes it when
// change is nil.
func outlookImportChangeDate(ctx context.Context, token, events, seriesID string, at calendarImportTime, change map[string]any) error {
	var instances struct {
		Value []struct {
			ID            string `json:"id"`
			OriginalStart string `json:"originalStart"`
		} `json:"value"`
	}
	query := url.Values{
		"startDateTime": {at.at.Add(-36 * time.Hour).UTC().Format(time.RFC3339)},
		"endDateTime":   {at.at.Add(36 * time.Hour).UTC().Format(time.RFC3339)},
		"$select":       {"id,originalStart"},
	}
	if err := calendarProviderJSON(ctx, http.MethodGet, events+"/"+url.PathEscape(seriesID)+"/instances?"+query.Encode(), token, nil, &instances); err != nil {
		return err
	}
	for _, instance := range instances.Value {
		original, err := time.Parse(time.RFC3339Nano, instance.OriginalStart)
		if err != nil {
			continue
		}
		same := original.Equal(at.at)
		if at.allDay {
			same = outlookAllDayDate(original).Format("2006-01-02") == at.date
		}
		if !same {
			continue
		}
		if change == nil {
			return calendarProviderJSON(ctx, http.MethodDelete, events+"/"+url.PathEscape(instance.ID), token, nil, nil)
		}
		return calendarProviderJSON(ctx, http.MethodPatch, events+"/"+url.PathEscape(instance.ID), token, change, nil)
	}
	return calendarTransferProblem("Microsoft has no such date in the series")
}

// outlookExportEvent is a Microsoft event as Gofer lists it for export.
type outlookExportEvent struct {
	outlookCalendarEvent
	Type          string `json:"type"`
	OriginalStart string `json:"originalStart"`
}

// exportOutlookCalendarEvents reads every event of a Microsoft calendar,
// each series with its changed and deleted dates.
func exportOutlookCalendarEvents(ctx context.Context, source storage.CalendarSource, token string) ([]byte, error) {
	events := outlookGraphBaseURL + "/me/calendars/" + url.PathEscape(source.RemoteID) + "/events"
	expand := "singleValueExtendedProperties($filter=id eq " + outlookODataString(outlookImportUIDProperty) + ")"
	query := url.Values{"$top": {"100"}, "$expand": {expand}}
	next := events + "?" + query.Encode()
	var files []string
	for page := 0; next != ""; page++ {
		if page >= calendarSyncMaxPages {
			return nil, errors.New("Microsoft calendar export exceeded its page limit")
		}
		var listed struct {
			Value    []outlookExportEvent `json:"value"`
			NextLink string               `json:"@odata.nextLink"`
		}
		if err := calendarProviderJSON(ctx, http.MethodGet, next, token, nil, &listed, `outlook.timezone="UTC"`, `outlook.body-content-type="text"`); err != nil {
			return nil, err
		}
		for _, remote := range listed.Value {
			if remote.IsCancelled || (remote.Type != "singleInstance" && remote.Type != "seriesMaster") {
				continue
			}
			data, err := exportOutlookCalendarEvent(ctx, token, events, remote)
			if err != nil {
				// A failed request ends the export; an event Gofer can't
				// write is left out.
				var provider calendarCreateProviderError
				if ctx.Err() != nil || (errors.As(err, &provider) && calendarImportAborts(err)) {
					return nil, err
				}
				log.Printf("calendar export: Microsoft event %s: %v", remote.ID, err)
				continue
			}
			files = append(files, data)
		}
		next = listed.NextLink
		if next != "" && !strings.HasPrefix(next, events) {
			return nil, errors.New("Microsoft gave a next page outside the calendar")
		}
	}
	return calendarExportFile(source.Name, files)
}

// exportOutlookCalendarEvent is one Microsoft event, a series with its
// changed and deleted dates, as an iCalendar file.
func exportOutlookCalendarEvent(ctx context.Context, token, events string, remote outlookExportEvent) (string, error) {
	uid := strings.TrimSpace(remote.ICalUID)
	for _, prop := range remote.SingleValueExtendedProperties {
		if strings.EqualFold(prop.ID, outlookImportUIDProperty) && strings.TrimSpace(prop.Value) != "" {
			uid = strings.TrimSpace(prop.Value)
		}
	}
	if uid == "" {
		uid = remote.ID
	}
	zone := outlookCalendarTimeLocation(remote.OriginalStartTimeZone)
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//Gofer//Calendar//EN")
	cal.Props.SetText(ical.PropVersion, "2.0")
	rule := ""
	if remote.Type == "seriesMaster" {
		var err error
		if rule, err = outlookExportRule(remote.Recurrence, remote.IsAllDay, zone); err != nil {
			return "", err
		}
	}
	series, err := outlookExportComponent(cal, remote.outlookCalendarEvent, uid, zone, rule)
	if err != nil {
		return "", err
	}
	if remote.IsReminderOn != nil && *remote.IsReminderOn && remote.ReminderMinutes != nil && *remote.ReminderMinutes >= 0 {
		series.Children = append(series.Children, calendarExportAlarm(*remote.ReminderMinutes))
	}
	cal.Children = append(cal.Children, series)
	if rule != "" {
		if err := outlookExportDates(ctx, token, events, remote, cal, series, uid, zone, rule); err != nil {
			return "", err
		}
	}
	return calendarExportPersonalEvents(cal)
}

// outlookExportComponent is a Microsoft event as a VEVENT, its time zone
// added to cal.
func outlookExportComponent(cal *ical.Calendar, remote outlookCalendarEvent, uid string, zone *time.Location, rule string) (*ical.Component, error) {
	// Google's builder writes times the way Microsoft's are meant: in the
	// zone the event was made in, with that zone's rules.
	shaped := googleCalendarEvent{ICalUID: uid, Summary: remote.Subject, Description: remote.Body.Content, Location: remote.Location.DisplayName}
	if rule != "" {
		shaped.Recurrence = []string{"RRULE:" + rule}
	}
	if remote.ShowAs == "free" {
		shaped.Transparency = "transparent"
	}
	if remote.IsAllDay {
		shaped.Start.Date, shaped.End.Date = outlookCalendarDate(remote.Start.DateTime), outlookCalendarDate(remote.End.DateTime)
	} else {
		start, err := parseOutlookCalendarDateTime(remote.Start)
		if err != nil || start == nil {
			return nil, fmt.Errorf("unreadable start: %v", err)
		}
		end, err := parseOutlookCalendarDateTime(remote.End)
		if err != nil || end == nil {
			return nil, fmt.Errorf("unreadable end: %v", err)
		}
		shaped.Start = googleCalendarEventDateTime{DateTime: start.Format(time.RFC3339), TimeZone: zone.String()}
		shaped.End = googleCalendarEventDateTime{DateTime: end.Format(time.RFC3339), TimeZone: zone.String()}
	}
	shaped.Organizer = googleCalendarPerson{Email: remote.Organizer.EmailAddress.Address, DisplayName: remote.Organizer.EmailAddress.Name}
	var attendees []struct {
		Type   string `json:"type"`
		Status struct {
			Response string `json:"response"`
		} `json:"status"`
		EmailAddress outlookEmailAddress `json:"emailAddress"`
	}
	if len(remote.Attendees) > 0 && json.Unmarshal(remote.Attendees, &attendees) != nil {
		return nil, errors.New("Microsoft returned unreadable guests")
	}
	var guests []map[string]any
	for _, person := range attendees {
		status := map[string]string{"accepted": "accepted", "declined": "declined", "tentativelyAccepted": "tentative"}[person.Status.Response]
		guests = append(guests, map[string]any{"email": person.EmailAddress.Address, "displayName": person.EmailAddress.Name, "responseStatus": status, "optional": person.Type == "optional", "resource": person.Type == "resource"})
	}
	if len(guests) > 0 {
		shaped.Attendees, _ = json.Marshal(guests)
	}
	if shaped.Organizer.Email == "" {
		shaped.Organizer.Email = "unknown@invalid"
	}
	event, err := googleCalendarForwardEvent(cal, shaped)
	if err != nil {
		return nil, err
	}
	if remote.Sensitivity == "private" || remote.Sensitivity == "confidential" {
		event.Props.SetText(ical.PropClass, "PRIVATE")
	}
	if remote.ShowAs == "tentative" {
		event.Props.SetText(ical.PropStatus, "TENTATIVE")
	}
	return event, nil
}

// outlookExportRule is Microsoft's pattern and range as an RRULE value.
func outlookExportRule(raw json.RawMessage, allDay bool, zone *time.Location) (string, error) {
	var recurrence struct {
		Pattern struct {
			Type           string   `json:"type"`
			Interval       int      `json:"interval"`
			Month          int      `json:"month"`
			DayOfMonth     int      `json:"dayOfMonth"`
			DaysOfWeek     []string `json:"daysOfWeek"`
			FirstDayOfWeek string   `json:"firstDayOfWeek"`
			Index          string   `json:"index"`
		} `json:"pattern"`
		Range struct {
			Type                string `json:"type"`
			EndDate             string `json:"endDate"`
			NumberOfOccurrences int    `json:"numberOfOccurrences"`
		} `json:"range"`
	}
	if err := json.Unmarshal(raw, &recurrence); err != nil {
		return "", fmt.Errorf("unreadable repeat rule: %w", err)
	}
	pattern := recurrence.Pattern
	days := func() string {
		var out []string
		for _, day := range pattern.DaysOfWeek {
			if len(day) >= 2 {
				out = append(out, strings.ToUpper(day[:2]))
			}
		}
		return strings.Join(out, ",")
	}
	setpos := map[string]string{"first": "1", "second": "2", "third": "3", "fourth": "4", "last": "-1"}[pattern.Index]
	var parts []string
	switch pattern.Type {
	case "daily":
		parts = []string{"FREQ=DAILY"}
	case "weekly":
		parts = []string{"FREQ=WEEKLY", "BYDAY=" + days()}
		if len(pattern.FirstDayOfWeek) >= 2 {
			parts = append(parts, "WKST="+strings.ToUpper(pattern.FirstDayOfWeek[:2]))
		}
	case "absoluteMonthly":
		parts = []string{"FREQ=MONTHLY", "BYMONTHDAY=" + strconv.Itoa(pattern.DayOfMonth)}
	case "relativeMonthly":
		parts = []string{"FREQ=MONTHLY", "BYDAY=" + days(), "BYSETPOS=" + setpos}
	case "absoluteYearly":
		parts = []string{"FREQ=YEARLY", "BYMONTH=" + strconv.Itoa(pattern.Month), "BYMONTHDAY=" + strconv.Itoa(pattern.DayOfMonth)}
	case "relativeYearly":
		parts = []string{"FREQ=YEARLY", "BYMONTH=" + strconv.Itoa(pattern.Month), "BYDAY=" + days(), "BYSETPOS=" + setpos}
	default:
		return "", fmt.Errorf("unknown repeat pattern %q", pattern.Type)
	}
	if pattern.Interval > 1 {
		parts = append(parts, "INTERVAL="+strconv.Itoa(pattern.Interval))
	}
	switch recurrence.Range.Type {
	case "numbered":
		parts = append(parts, "COUNT="+strconv.Itoa(recurrence.Range.NumberOfOccurrences))
	case "endDate":
		end, err := time.Parse("2006-01-02", recurrence.Range.EndDate)
		if err != nil {
			return "", fmt.Errorf("unreadable repeat end: %w", err)
		}
		if allDay {
			parts = append(parts, "UNTIL="+end.Format("20060102"))
		} else {
			last := time.Date(end.Year(), end.Month(), end.Day(), 23, 59, 59, 0, zone)
			parts = append(parts, "UNTIL="+last.UTC().Format("20060102T150405Z"))
		}
	}
	return strings.Join(parts, ";"), nil
}

// outlookExportDates adds a series' changed dates to cal and its deleted
// ones to the series as EXDATE: the dates the rule makes that Microsoft
// doesn't list. Dates are read up to two years ahead.
func outlookExportDates(ctx context.Context, token, events string, remote outlookExportEvent, cal *ical.Calendar, series *ical.Component, uid string, zone *time.Location, rule string) error {
	startProp := series.Props.Get(ical.PropDateTimeStart)
	start, err := readCalendarImportTime(startProp)
	if err != nil {
		return err
	}
	if remote.IsAllDay {
		zone = time.UTC
	}
	option, err := rrule.StrToROptionInLocation(rule, zone)
	if err != nil {
		return err
	}
	option.Dtstart = start.at.In(zone)
	if remote.IsAllDay {
		option.Dtstart = time.Date(start.at.Year(), start.at.Month(), start.at.Day(), 0, 0, 0, 0, time.UTC)
	}
	repeat, err := rrule.NewRRule(*option)
	if err != nil {
		return err
	}
	until := time.Now().AddDate(2, 0, 0)
	listed := map[int64]bool{}
	query := url.Values{
		"startDateTime": {option.Dtstart.Add(-24 * time.Hour).UTC().Format(time.RFC3339)},
		"endDateTime":   {until.UTC().Format(time.RFC3339)},
		"$top":          {"100"},
	}
	next := events + "/" + url.PathEscape(remote.ID) + "/instances?" + query.Encode()
	for page := 0; next != ""; page++ {
		if page >= calendarSyncMaxPages {
			return errors.New("Microsoft series export exceeded its page limit")
		}
		var instances struct {
			Value    []outlookExportEvent `json:"value"`
			NextLink string               `json:"@odata.nextLink"`
		}
		if err := calendarProviderJSON(ctx, http.MethodGet, next, token, nil, &instances, `outlook.timezone="UTC"`, `outlook.body-content-type="text"`); err != nil {
			return err
		}
		for _, instance := range instances.Value {
			original, err := time.Parse(time.RFC3339Nano, instance.OriginalStart)
			if err != nil {
				continue
			}
			if remote.IsAllDay {
				original = outlookAllDayDate(original)
			}
			listed[original.Unix()] = true
			if instance.Type != "exception" || instance.IsCancelled {
				continue
			}
			changed, err := outlookExportComponent(cal, instance.outlookCalendarEvent, uid, zone, "")
			if err != nil {
				return err
			}
			changed.Props.Set(outlookExportDate(ical.PropRecurrenceID, original, remote.IsAllDay, zone))
			cal.Children = append(cal.Children, changed)
		}
		next = instances.NextLink
		if next != "" && !strings.HasPrefix(next, events) {
			return errors.New("Microsoft gave a next page outside the calendar")
		}
	}
	for _, date := range repeat.Between(option.Dtstart.Add(-time.Second), until, true) {
		if !listed[date.Unix()] {
			series.Props.Add(outlookExportDate(ical.PropExceptionDates, date, remote.IsAllDay, zone))
		}
	}
	return nil
}

// outlookExportDate is a RECURRENCE-ID or EXDATE written as the series'
// start is: a date, or a time in its zone.
func outlookExportDate(name string, at time.Time, allDay bool, zone *time.Location) *ical.Prop {
	prop := ical.NewProp(name)
	switch {
	case allDay:
		prop.SetDate(at.UTC())
	case zone == time.UTC:
		prop.SetDateTime(at.UTC())
	default:
		prop.SetDateTime(at.In(zone))
	}
	return prop
}
