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
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

const calDAVResponseLimit = 16 << 20

var calDAVHTTPTransport http.RoundTripper

func calDAVRequest(ctx context.Context, method, endpoint, username, password, depth, body string, timeout time.Duration) (davMultiStatus, error) {
	req, err := newCardDAVRequest(ctx, method, endpoint, username, password, strings.NewReader(body))
	if err != nil {
		return davMultiStatus{}, err
	}
	if req.URL.Scheme != "https" || req.URL.User != nil {
		return davMultiStatus{}, fmt.Errorf("CalDAV requires an HTTPS URL without embedded credentials")
	}
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	client := &http.Client{
		Transport: calDAVHTTPTransport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many CalDAV redirects")
			}
			if len(via) > 0 && (!strings.EqualFold(via[0].URL.Scheme, req.URL.Scheme) || !strings.EqualFold(via[0].URL.Host, req.URL.Host) || req.URL.User != nil) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return davMultiStatus{}, fmt.Errorf("CalDAV request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return davMultiStatus{}, fmt.Errorf("CalDAV returned HTTP %d", resp.StatusCode)
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

func listCalDAVCalendarEvents(ctx context.Context, source storage.CalendarSource, username, password string, query calendar.EventQuery) (calendar.EventPage, error) {
	if query.WindowStart.IsZero() || query.WindowEnd.IsZero() || !query.WindowEnd.After(query.WindowStart) {
		return calendar.EventPage{}, fmt.Errorf("CalDAV event window is invalid")
	}
	start := query.WindowStart.UTC().Format("20060102T150405Z")
	end := query.WindowEnd.UTC().Format("20060102T150405Z")
	// RFC 4791 section 9.6.5 expands recurrence sets, including exceptions,
	// into individual instances in this bounded window, with zoned times in UTC.
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
 <d:prop><d:getetag/><c:calendar-data><c:expand start="%s" end="%s"/></c:calendar-data></d:prop>
 <c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"><c:time-range start="%s" end="%s"/></c:comp-filter></c:comp-filter></c:filter>
</c:calendar-query>`, start, end, start, end)
	multi, err := calDAVRequest(ctx, "REPORT", source.RemoteID, username, password, "1", body, 30*time.Second)
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
	for _, response := range multi.Responses {
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
		href, err := resolveCalDAVHref(source.RemoteID, response.Href)
		if err != nil {
			return calendar.EventPage{}, err
		}
		events, err := parseCalDAVEvents(href, etag, data, location)
		if err != nil {
			return calendar.EventPage{}, fmt.Errorf("read CalDAV event: %w", err)
		}
		for _, event := range events {
			if seen[event.RemoteID] {
				return calendar.EventPage{}, fmt.Errorf("CalDAV returned a duplicate event instance")
			}
			seen[event.RemoteID] = true
			page.Events = append(page.Events, event)
		}
	}
	// RFC 4791 allows the initial expanded occurrence to omit RECURRENCE-ID.
	// Read minimal original metadata in batches so it is not mistaken for a
	// standalone event, even when the next recurrence is outside this window.
	if err := identifyCalDAVInitialOccurrences(ctx, source, username, password, page.Events); err != nil {
		return calendar.EventPage{}, err
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
		event.StartDate, event.EndDate = start.Format("2006-01-02"), end.Format("2006-01-02")
		if event.EndDate <= event.StartDate {
			return calendar.RemoteEvent{}, fmt.Errorf("CalDAV all-day event has an invalid date range")
		}
	} else {
		event.StartTimeZone, event.EndTimeZone = start.Location().String(), end.Location().String()
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
	return event, nil
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
	if strings.TrimSpace(data) == "" {
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
