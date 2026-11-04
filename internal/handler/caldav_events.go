package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

const calDAVResponseLimit = 16 << 20

func calDAVRequest(ctx context.Context, method, endpoint, username, password, depth, body string, timeout time.Duration) (davMultiStatus, error) {
	if err := calendarDiscoveryDAVGuard(ctx); err != nil {
		return davMultiStatus{}, err
	}
	req, err := newCardDAVRequest(ctx, method, endpoint, username, password, strings.NewReader(body))
	if err != nil {
		return davMultiStatus{}, err
	}
	if !calDAVURLScheme(req.URL.Scheme) || req.URL.User != nil {
		return davMultiStatus{}, fmt.Errorf("CalDAV requires an HTTP(S) URL without embedded credentials")
	}
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	client := &http.Client{
		Transport: calDAVHTTPTransport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if err := calendarDiscoveryDAVGuard(ctx); err != nil {
				return err
			}
			if len(via) >= 5 {
				return fmt.Errorf("too many CalDAV redirects")
			}
			if len(via) > 0 && (!sameCalDAVOrigin(via[0].URL, req.URL) || req.URL.User != nil) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	resp, err := calendarProviderDo(client, req)
	if err != nil {
		return davMultiStatus{}, fmt.Errorf("CalDAV request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return davMultiStatus{}, calendarDiscoveryDAVFailure(ctx, calDAVDiscoveryError{status: resp.StatusCode, retryAt: providerRetryAfter(resp)})
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, calDAVResponseLimit+1))
	if err != nil {
		return davMultiStatus{}, err
	}
	if len(data) > calDAVResponseLimit {
		return davMultiStatus{}, fmt.Errorf("CalDAV response exceeds the calendar sync size limit")
	}
	// A successful HTML error page must never be treated as an empty calendar.
	var root struct{ XMLName xml.Name }
	if err := xml.Unmarshal(data, &root); err != nil || root.XMLName.Local != "multistatus" || root.XMLName.Space != "DAV:" {
		return davMultiStatus{}, fmt.Errorf("CalDAV did not return a valid multistatus response")
	}
	multi, err := decodeDAVMultiStatus(bytes.NewReader(data))
	if err != nil {
		return davMultiStatus{}, fmt.Errorf("decode CalDAV response: %w", err)
	}
	return multi, nil
}

func calendarSyncDAVResource(ctx context.Context, collection, href string) (string, error) {
	resolved, err := resolveCalDAVHref(collection, href)
	if err != nil {
		return "", err
	}
	if _, owned := ctx.Value(calendarDiscoveryDAVGuardKey{}).(calendarDiscoveryDAVCallbacks); !owned {
		return resolved, nil
	}
	base, baseErr := url.Parse(collection)
	remote, remoteErr := url.Parse(resolved)
	if baseErr != nil || remoteErr != nil || remote.User != nil || remote.RawQuery != "" || remote.Fragment != "" || !strings.HasPrefix(path.Clean(remote.Path), strings.TrimRight(path.Clean(base.Path), "/")+"/") {
		return "", fmt.Errorf("CalDAV event resource is outside the selected calendar")
	}
	return resolved, nil
}

func listCalDAVCalendarEvents(ctx context.Context, source storage.CalendarSource, username, password string, query calendar.EventQuery) (calendar.EventPage, error) {
	if query.WindowStart.IsZero() || query.WindowEnd.IsZero() || !query.WindowEnd.After(query.WindowStart) {
		return calendar.EventPage{}, fmt.Errorf("CalDAV event window is invalid")
	}
	start := query.WindowStart.UTC().Format("20060102T150405Z")
	end := query.WindowEnd.UTC().Format("20060102T150405Z")
	// RFC 4791 section 9.6.5 expands recurrence sets, including exceptions,
	// into individual instances in this bounded window, with zoned times in UTC.
	// A server that can't do that is asked for the events as stored, and
	// Gofer expands them itself.
	calendarQuery := func(data string) string {
		return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
 <d:prop><d:getetag/>%s</d:prop>
 <c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"><c:time-range start="%s" end="%s"/></c:comp-filter></c:comp-filter></c:filter>
</c:calendar-query>`, data, start, end)
	}
	expandedByServer := !calDAVExpandRefusedFor(source.RemoteID)
	var multi davMultiStatus
	var err error
	if expandedByServer {
		multi, err = calDAVRequest(ctx, "REPORT", source.RemoteID, username, password, "1",
			calendarQuery(fmt.Sprintf(`<c:calendar-data><c:expand start="%s" end="%s"/></c:calendar-data>`, start, end)), 30*time.Second)
		if calDAVServerCannotExpand(err) {
			expandedByServer = false
		}
	}
	if !expandedByServer {
		multi, err = calDAVRequest(ctx, "REPORT", source.RemoteID, username, password, "1", calendarQuery(`<c:calendar-data/>`), 30*time.Second)
		if err == nil {
			rememberCalDAVExpandRefused(source.RemoteID)
		}
	}
	if err != nil {
		return calendar.EventPage{}, err
	}
	location := query.WindowStart.Location()
	if source.TimeZone != "" {
		location, err = time.LoadLocation(source.TimeZone)
		if err != nil {
			return calendar.EventPage{}, fmt.Errorf("load calendar timezone: %w", err)
		}
	}
	page := calendar.EventPage{}
	seen := make(map[string]bool)
	var resources []calDAVResourceEvents
	for _, response := range multi.Responses {
		if err := ctx.Err(); err != nil {
			return calendar.EventPage{}, err
		}
		if response.Status != "" && !strings.Contains(response.Status, " 200 ") {
			return calendar.EventPage{}, fmt.Errorf("CalDAV returned an incomplete calendar result")
		}
		var data, etag string
		for _, propStat := range response.PropStats {
			if !strings.Contains(propStat.Status, " 200 ") {
				continue
			}
			if propStat.Prop.CalendarData != "" {
				data = propStat.Prop.CalendarData
			}
			if propStat.Prop.GetETag != "" {
				etag = strings.TrimSpace(propStat.Prop.GetETag)
			}
		}
		if strings.TrimSpace(data) == "" || strings.TrimSpace(response.Href) == "" {
			return calendar.EventPage{}, fmt.Errorf("CalDAV returned an event without its calendar data or resource URL")
		}
		href, err := calendarSyncDAVResource(ctx, source.RemoteID, response.Href)
		if err != nil {
			return calendar.EventPage{}, err
		}
		var events []calendar.RemoteEvent
		if expandedByServer {
			events, err = parseCalDAVEvents(href, etag, data, location)
		} else {
			var occurrences []jmapOccurrence
			occurrences, err = expandCalDAVResource(data, href, etag, query.WindowStart, query.WindowEnd, location)
			for _, occurrence := range occurrences {
				events = append(events, occurrence.remote)
			}
		}
		if err != nil {
			return calendar.EventPage{}, fmt.Errorf("read CalDAV event: %w", err)
		}
		resources = append(resources, calDAVResourceEvents{href: href, etag: etag, events: events})
	}
	// The server's expansion of a series with dates changed on their own
	// isn't trusted; Gofer expands those from the stored event.
	if expandedByServer {
		if err := expandCalDAVChangedSeries(ctx, source, username, password, resources, query.WindowStart, query.WindowEnd, location); err != nil {
			return calendar.EventPage{}, err
		}
	}
	for _, resource := range resources {
		for _, event := range resource.events {
			if seen[event.RemoteID] {
				return calendar.EventPage{}, fmt.Errorf("CalDAV returned a duplicate event instance")
			}
			seen[event.RemoteID] = true
			page.Events = append(page.Events, event)
			if len(page.Events) > calendarSyncMaxEvents {
				return calendar.EventPage{}, fmt.Errorf("CalDAV event traversal exceeded its event limit")
			}
		}
	}
	// RFC 4791 allows the initial expanded occurrence to omit RECURRENCE-ID.
	// Read minimal original metadata in batches so it is not mistaken for a
	// standalone event, even when the next recurrence is outside this window.
	// Gofer's own expansion marks every occurrence already.
	if expandedByServer {
		if err := identifyCalDAVInitialOccurrences(ctx, source, username, password, page.Events); err != nil {
			return calendar.EventPage{}, err
		}
	}
	clear(seen)
	for _, event := range page.Events {
		if seen[event.RemoteID] {
			return calendar.EventPage{}, fmt.Errorf("CalDAV returned a duplicate initial occurrence")
		}
		seen[event.RemoteID] = true
	}
	return page, nil
}

func parseCalDAVEvents(href, etag, data string, location *time.Location) ([]calendar.RemoteEvent, error) {
	decoder := ical.NewDecoder(strings.NewReader(data))
	cal, err := decoder.Decode()
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Decode(); err != io.EOF {
		return nil, fmt.Errorf("CalDAV event data contains an additional or invalid calendar")
	}
	components := cal.Events()
	if len(components) == 0 {
		return nil, fmt.Errorf("CalDAV calendar data contains no events")
	}
	events := make([]calendar.RemoteEvent, 0, len(components))
	for _, component := range components {
		event, err := normalizeCalDAVEvent(component, href, etag, location)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

func normalizeCalDAVEvent(component ical.Event, href, etag string, location *time.Location) (calendar.RemoteEvent, error) {
	props := component.Props
	for _, name := range []string{"RRULE", "EXRULE", "RDATE", "EXDATE"} {
		if props.Get(name) != nil {
			return calendar.RemoteEvent{}, fmt.Errorf("the CalDAV server did not expand recurring events")
		}
	}
	event := calendar.RemoteEvent{
		RemoteID: href, ETag: etag, Status: "confirmed",
		Recurrence: json.RawMessage("[]"), Attendees: json.RawMessage("[]"), OnlineMeeting: json.RawMessage("{}"),
	}
	for _, field := range []struct {
		name  string
		value *string
	}{{"UID", &event.ICalUID}, {"SUMMARY", &event.Summary}, {"DESCRIPTION", &event.Description}, {"LOCATION", &event.Location}} {
		text, err := props.Text(field.name)
		if err != nil {
			return calendar.RemoteEvent{}, err
		}
		*field.value = strings.TrimSpace(text)
	}
	if alternate := props.Get("X-ALT-DESC"); alternate != nil && strings.EqualFold(alternate.Params.Get("FMTTYPE"), "text/html") {
		if rich, err := props.Text("X-ALT-DESC"); err == nil {
			event.Description = calendar.SanitizeDescriptionHTML(rich)
		}
	}
	if event.ICalUID == "" {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV event has no UID")
	}
	if status := props.Get("STATUS"); status != nil {
		event.Status = strings.ToLower(strings.TrimSpace(status.Value))
		event.Deleted = event.Status == "cancelled"
	}
	if recurrence := props.Get("RECURRENCE-ID"); recurrence != nil {
		at, err := recurrence.DateTime(location)
		if err != nil {
			return calendar.RemoteEvent{}, fmt.Errorf("invalid recurrence identifier: %w", err)
		}
		key := at.UTC().Format(time.RFC3339)
		if recurrence.ValueType() == ical.ValueDate {
			key = at.Format("2006-01-02")
		}
		event.RemoteID = href + "#recurrence=" + url.QueryEscape(key)
		event.SeriesRemoteID = href
		event.Recurrence, _ = json.Marshal([]string{"RECURRENCE-ID:" + key})
	}
	startProp := props.Get("DTSTART")
	if startProp == nil {
		if event.Deleted {
			return event, nil
		}
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV event has no start date")
	}
	event.AllDay = startProp.ValueType() == ical.ValueDate
	if event.AllDay {
		// DATE values have no timezone. Parsing them in UTC also keeps the
		// implicit one-day duration correct across daylight-saving changes.
		location = time.UTC
	}
	if props.Get("DTEND") != nil && props.Get("DURATION") != nil {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV event has both an end date and a duration")
	}
	start, err := component.DateTimeStart(location)
	if err != nil {
		return calendar.RemoteEvent{}, fmt.Errorf("invalid event start: %w", err)
	}
	end, err := component.DateTimeEnd(location)
	if durationProp := props.Get("DURATION"); durationProp != nil && err == nil {
		duration, durationErr := durationProp.Duration()
		if durationErr != nil {
			return calendar.RemoteEvent{}, durationErr
		}
		// Days and weeks are calendar units; hours are elapsed time. They
		// differ when a TZID event spans a daylight-saving change.
		prefix := strings.SplitN(strings.TrimPrefix(durationProp.Value, "+"), "T", 2)[0]
		if strings.HasPrefix(prefix, "P") && (strings.HasSuffix(prefix, "D") || strings.HasSuffix(prefix, "W")) {
			days, countErr := strconv.Atoi(prefix[1 : len(prefix)-1])
			if countErr != nil {
				return calendar.RemoteEvent{}, countErr
			}
			if strings.HasSuffix(prefix, "W") {
				days *= 7
			}
			end = start.AddDate(0, 0, days).Add(duration - time.Duration(days)*24*time.Hour)
		}
		if event.AllDay && duration%(24*time.Hour) != 0 {
			return calendar.RemoteEvent{}, fmt.Errorf("CalDAV all-day duration must use whole days")
		}
	}
	if err != nil || end.Before(start) {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV event has an invalid end date")
	}
	if endProp := props.Get("DTEND"); endProp != nil && (endProp.ValueType() == ical.ValueDate) != event.AllDay {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV event start and end use different date types")
	}
	if event.AllDay {
		// Some clients write an all-day event's end as the same day it starts,
		// treating DTEND as inclusive. Other clients show those as one day.
		if !end.After(start) {
			end = start.AddDate(0, 0, 1)
		}
		event.StartDate, event.EndDate = start.Format("2006-01-02"), end.Format("2006-01-02")
		if event.EndDate <= event.StartDate {
			return calendar.RemoteEvent{}, fmt.Errorf("CalDAV all-day event has an invalid date range")
		}
	} else {
		event.StartTimeZone, event.EndTimeZone = start.Location().String(), end.Location().String()
		// Gofer writes its own events' times in UTC, with the zone they were
		// made in beside them.
		if zone, _ := props.Text("X-GOFER-TIMEZONE"); start.Location() == time.UTC && zone != "" {
			if location, err := time.LoadLocation(zone); err == nil && location.String() != "Local" {
				event.StartTimeZone, event.EndTimeZone = location.String(), location.String()
			}
		}
		start, end = start.UTC(), end.UTC()
		event.StartAt, event.EndAt = &start, &end
	}
	if organizer := props.Get("ORGANIZER"); organizer != nil {
		event.OrganizerName = organizer.Params.Get("CN")
		event.OrganizerEmail = calDAVPersonEmail(organizer.Value)
	}
	type attendee struct {
		Name   string `json:"name,omitempty"`
		Email  string `json:"email"`
		Status string `json:"status,omitempty"`
		Role   string `json:"role,omitempty"`
	}
	var attendees []attendee
	for _, person := range props["ATTENDEE"] {
		attendees = append(attendees, attendee{person.Params.Get("CN"), calDAVPersonEmail(person.Value), person.Params.Get("PARTSTAT"), person.Params.Get("ROLE")})
	}
	if len(attendees) > 0 {
		event.Attendees, _ = json.Marshal(attendees)
	}
	if link := props.Get("URL"); link != nil {
		event.HTMLLink = strings.TrimSpace(link.Value)
	}
	for _, field := range []struct {
		name  string
		value **time.Time
	}{{"CREATED", &event.ProviderCreatedAt}, {"LAST-MODIFIED", &event.ProviderUpdatedAt}} {
		if props.Get(field.name) == nil {
			continue
		}
		at, err := props.DateTime(field.name, time.UTC)
		if err != nil {
			return calendar.RemoteEvent{}, err
		}
		at = at.UTC()
		*field.value = &at
	}
	if event.StartAt != nil {
		event.ReminderMinutes = calDAVEarliestReminder(component.Component, *event.StartAt)
	}
	event.ShowAs = "busy"
	if transparency, _ := component.Props.Text("TRANSP"); strings.EqualFold(strings.TrimSpace(transparency), "TRANSPARENT") {
		event.ShowAs = "free"
	}
	class, _ := component.Props.Text("CLASS")
	class = strings.ToUpper(strings.TrimSpace(class))
	event.Private = class == "PRIVATE" || class == "CONFIDENTIAL"
	return event, nil
}

// calDAVEarliestReminder returns the earliest display or sound alarm set
// before the event starts. Email alarms are left to the server.
func calDAVEarliestReminder(component *ical.Component, start time.Time) *int {
	var earliest *int
	for _, child := range component.Children {
		if child.Name != ical.CompAlarm {
			continue
		}
		action, _ := child.Props.Text("ACTION")
		if action = strings.ToUpper(strings.TrimSpace(action)); action != "DISPLAY" && action != "AUDIO" {
			continue
		}
		trigger := child.Props.Get("TRIGGER")
		if trigger == nil {
			continue
		}
		minutes := -1
		if trigger.ValueType() == ical.ValueDateTime {
			at, err := trigger.DateTime(time.UTC)
			if err != nil {
				continue
			}
			minutes = int(start.Sub(at).Minutes())
		} else if !strings.EqualFold(trigger.Params.Get("RELATED"), "END") {
			before, err := trigger.Duration()
			if err != nil || before > 0 {
				continue
			}
			minutes = int((-before).Minutes())
		}
		if minutes >= 0 && (earliest == nil || minutes > *earliest) {
			earliest = &minutes
		}
	}
	return earliest
}

func calDAVPersonEmail(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err == nil && strings.EqualFold(parsed.Scheme, "mailto") {
		if address, err := url.PathUnescape(parsed.Opaque); err == nil {
			return address
		}
	}
	return strings.TrimSpace(value)
}

func calDAVCalendarTimeZone(data string) string {
	data = strings.TrimSpace(data)
	if data == "" {
		return ""
	}
	// Baïkal stores the bare name, such as "Europe/Zurich", where the
	// property should hold a VCALENDAR with the zone in it.
	if !strings.ContainsAny(data, "\r\n:") {
		if location, err := time.LoadLocation(data); err == nil && location.String() != "Local" {
			return data
		}
		return ""
	}
	cal, err := ical.NewDecoder(strings.NewReader(data)).Decode()
	if err != nil {
		return ""
	}
	for _, child := range cal.Children {
		if child.Name != "VTIMEZONE" {
			continue
		}
		zone, err := child.Props.Text("TZID")
		if err == nil {
			if _, err := time.LoadLocation(zone); err == nil {
				return zone
			}
		}
	}
	return ""
}

// calDAVSetReminder replaces the event's display and sound alarms, the ones
// read as its reminder, with one display alarm, or removes them (minutes nil).
// Email alarms and alarms relative to the event's end are kept.
func calDAVSetReminder(component *ical.Component, minutes *int) {
	children := component.Children[:0]
	for _, child := range component.Children {
		if child.Name == ical.CompAlarm {
			action, _ := child.Props.Text("ACTION")
			action = strings.ToUpper(strings.TrimSpace(action))
			trigger := child.Props.Get("TRIGGER")
			if (action == "DISPLAY" || action == "AUDIO") && trigger != nil && !strings.EqualFold(trigger.Params.Get("RELATED"), "END") {
				continue
			}
		}
		children = append(children, child)
	}
	component.Children = children
	if minutes == nil {
		return
	}
	alarm := ical.NewComponent(ical.CompAlarm)
	alarm.Props.SetText("ACTION", "DISPLAY")
	alarm.Props.SetText("DESCRIPTION", "Reminder")
	trigger := ical.NewProp("TRIGGER")
	trigger.Value = calDAVReminderTrigger(*minutes)
	alarm.Props.Set(trigger)
	component.Children = append(component.Children, alarm)
}

func calDAVReminderTrigger(minutes int) string {
	if minutes == 0 {
		return "PT0S"
	}
	if minutes%1440 == 0 {
		return fmt.Sprintf("-P%dD", minutes/1440)
	}
	return fmt.Sprintf("-PT%dM", minutes)
}

// calendarSameReminder reports whether a chosen reminder is the one saved.
func calendarSameReminder(a, b *int) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// calDAVSetShowAs marks the event's time busy or free; CalDAV has only these
// two, so the other Outlook states count as busy.
func calDAVSetShowAs(component *ical.Component, showAs string) {
	value := "OPAQUE"
	if showAs == "free" {
		value = "TRANSPARENT"
	}
	component.Props.SetText("TRANSP", value)
}

// calendarSameShowAs reports whether a chosen show-as is the one saved; an
// event that doesn't say is busy.
func calendarSameShowAs(chosen, saved string) bool {
	if saved == "" {
		saved = "busy"
	}
	return chosen == saved
}

// calDAVSetPrivate marks the event private, or removes the mark, which makes
// it public as CalDAV's default.
func calDAVSetPrivate(component *ical.Component, private bool) {
	if private {
		component.Props.SetText("CLASS", "PRIVATE")
		return
	}
	delete(component.Props, "CLASS")
}
