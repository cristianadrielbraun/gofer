package handler

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

func cloneCalendarComponent(source *ical.Component) *ical.Component {
	copy := ical.NewComponent(source.Name)
	for name, values := range source.Props {
		for _, prop := range values {
			params := make(ical.Params)
			for key, items := range prop.Params {
				params[key] = append([]string(nil), items...)
			}
			prop.Params = params
			copy.Props[name] = append(copy.Props[name], prop)
		}
	}
	for _, child := range source.Children {
		copy.Children = append(copy.Children, cloneCalendarComponent(child))
	}
	return copy
}

func calendarRecurrenceKey(prop *ical.Prop, location *time.Location) (string, error) {
	if prop == nil || prop.Params.Get("RANGE") != "" {
		return "", calendarUpdateUnsupported("Range exceptions cannot be changed in Gofer yet.")
	}
	at, err := prop.DateTime(location)
	if err != nil {
		return "", err
	}
	if prop.ValueType() == ical.ValueDate {
		return at.Format("2006-01-02"), nil
	}
	return at.UTC().Format(time.RFC3339), nil
}

// Inspect the complete resource, not just the expanded cached occurrence.
// Preserve other exceptions and alarms, but never act on scheduling resources,
// unrelated UIDs, floating/custom zones, or THISANDFUTURE range exceptions.
func calendarCalDAVOccurrenceResource(cal *ical.Calendar, headers http.Header, existing storage.CalendarEvent) (*ical.Component, *ical.Component, *ical.Prop, error) {
	var master, selected *ical.Component
	if cal.Props.Get("METHOD") != nil || headers.Get("Schedule-Tag") != "" || existing.ICalUID == "" {
		return nil, nil, nil, calendarUpdateUnsupported("Invitations cannot be changed in Gofer yet.")
	}
	for _, component := range cal.Children {
		if component.Name == ical.CompTimezone {
			continue
		}
		uid, err := component.Props.Text("UID")
		if component.Name != ical.CompEvent || err != nil || uid != existing.ICalUID || len(component.Props["UID"]) != 1 {
			return nil, nil, nil, calendarUpdateUnsupported("The resource does not contain just the selected series.")
		}
		if component.Props.Get("RECURRENCE-ID") == nil {
			if master != nil {
				return nil, nil, nil, calendarUpdateUnsupported("The resource contains multiple series.")
			}
			master = component
		}
		check := cloneCalendarComponent(component)
		for _, name := range []string{"RRULE", "RDATE", "EXDATE", "EXRULE", "RECURRENCE-ID"} {
			delete(check.Props, name)
		}
		if component.Props.Get("RECURRENCE-ID") != nil {
			for _, name := range []string{"RRULE", "RDATE", "EXDATE", "EXRULE"} {
				if component.Props.Get(name) != nil {
					return nil, nil, nil, calendarUpdateUnsupported("An exception contains its own repeat rule.")
				}
			}
			delete(check.Props, "STATUS") // A different, cancelled exception may stay intact.
		}
		one := ical.NewCalendar()
		one.Children = []*ical.Component{check}
		if reason := calendarUpdateCalDAVRestriction(one, headers); reason != "" {
			return nil, nil, nil, calendarUpdateUnsupported(reason)
		}
	}
	if master == nil || len(master.Props["RRULE"]) != 1 || master.Props.Get("EXRULE") != nil || len(master.Props["DTSTART"]) != 1 {
		return nil, nil, nil, calendarUpdateUnsupported("The provider did not return a supported recurring resource.")
	}
	start := master.Props.Get("DTSTART")
	location := time.UTC
	var err error
	if start.ValueType() != ical.ValueDate && !strings.HasSuffix(start.Value, "Z") {
		location, err = calendarSeriesLocation(start.Params.Get("TZID"))
		if err != nil {
			return nil, nil, nil, err
		}
	}
	key, err := url.QueryUnescape(strings.TrimPrefix(existing.RemoteID, existing.SeriesRemoteID+"#recurrence="))
	if err != nil || existing.RemoteID != existing.SeriesRemoteID+"#recurrence="+url.QueryEscape(key) {
		return nil, nil, nil, calendarUpdateUnsupported("The occurrence identity is unavailable. Refresh the calendar.")
	}
	rid := ical.NewProp("RECURRENCE-ID")
	if start.ValueType() == ical.ValueDate {
		at, err := time.Parse("2006-01-02", key)
		if err != nil {
			return nil, nil, nil, err
		}
		rid.SetDate(at)
	} else {
		at, err := time.Parse(time.RFC3339, key)
		if err != nil {
			return nil, nil, nil, err
		}
		rid.SetDateTime(at.In(location))
	}
	seen := make(map[string]bool)
	for _, event := range cal.Events() {
		prop := event.Props.Get("RECURRENCE-ID")
		if prop == nil {
			continue
		}
		candidate, err := calendarRecurrenceKey(prop, location)
		if err != nil || len(event.Props["RECURRENCE-ID"]) != 1 || prop.ValueType() != start.ValueType() || seen[candidate] {
			return nil, nil, nil, calendarUpdateUnsupported("The series has unsupported or ambiguous exceptions.")
		}
		seen[candidate] = true
		if candidate == key {
			selected = event.Component
		}
	}
	return master, selected, rid, nil
}

// Ask the provider to prove membership at the resource version we just read.
// Never guess recurrence dates locally, or send an instance fragment as a URL.
func calendarCalDAVVerifyOccurrence(ctx context.Context, source storage.CalendarSource, username, password string, existing storage.CalendarEvent) error {
	component, err := calendarCalDAVReadOccurrence(ctx, source, username, password, existing)
	if err != nil {
		return err
	}
	remote, err := normalizeCalDAVEvent(ical.Event{Component: component}, existing.SeriesRemoteID, existing.ETag, time.UTC)
	if remote.SeriesRemoteID == "" {
		remote.RemoteID, remote.SeriesRemoteID = existing.RemoteID, existing.SeriesRemoteID
	}
	if err != nil || calendarOccurrenceRestriction(remote, existing.SeriesRemoteID) != "" {
		return errCalendarUpdateConflict
	}
	return nil
}

// Read the expanded occurrence without ordinary-edit restrictions. RSVP has its
// own invitation/identity checks; ordinary edits still pass through the wrapper.
func calendarCalDAVReadOccurrence(ctx context.Context, source storage.CalendarSource, username, password string, existing storage.CalendarEvent) (*ical.Component, error) {
	start, end := time.Time{}, time.Time{}
	if existing.AllDay {
		start, _ = time.Parse("2006-01-02", existing.StartDate)
		end, _ = time.Parse("2006-01-02", existing.EndDate)
	} else if existing.StartAt != nil && existing.EndAt != nil {
		start, end = *existing.StartAt, *existing.EndAt
	}
	if start.IsZero() || !end.After(start) {
		return nil, errCalendarUpdateConflict
	}
	resource, _ := url.Parse(existing.SeriesRemoteID)
	var href bytes.Buffer
	_ = xml.EscapeText(&href, []byte(resource.EscapedPath()))
	body := fmt.Sprintf(`<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/><c:calendar-data><c:expand start="%s" end="%s"/></c:calendar-data></d:prop><d:href>%s</d:href></c:calendar-multiget>`, start.Add(-24*time.Hour).UTC().Format("20060102T150405Z"), end.Add(24*time.Hour).UTC().Format("20060102T150405Z"), href.String())
	multi, err := calDAVRequest(ctx, "REPORT", source.RemoteID, username, password, "1", body, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if len(multi.Responses) != 1 {
		return nil, errCalendarUpdateConflict
	}
	response := multi.Responses[0]
	resolved, err := resolveCalDAVHref(source.RemoteID, response.Href)
	if err != nil || resolved != existing.SeriesRemoteID || (response.Status != "" && !strings.Contains(response.Status, " 200 ")) {
		return nil, errCalendarUpdateConflict
	}
	var data, etag string
	for _, stat := range response.PropStats {
		if !strings.Contains(stat.Status, " 200 ") {
			return nil, errCalendarUpdateConflict
		}
		if stat.Prop.CalendarData != "" {
			data = stat.Prop.CalendarData
		}
		if stat.Prop.GetETag != "" {
			etag = strings.TrimSpace(stat.Prop.GetETag)
		}
	}
	if etag != existing.ETag {
		return nil, errCalendarUpdateConflict
	}
	cal, err := calendarUpdateDecodeICS([]byte(data))
	if err != nil {
		return nil, err
	}
	matches := 0
	var selected *ical.Component
	for _, event := range cal.Events() {
		remote, err := normalizeCalDAVEvent(event, resolved, etag, time.UTC)
		if err != nil {
			return nil, err
		}
		if remote.SeriesRemoteID == "" {
			key := remote.StartDate
			if !remote.AllDay && remote.StartAt != nil {
				key = remote.StartAt.UTC().Format(time.RFC3339)
			}
			remote.SeriesRemoteID = resolved
			remote.RemoteID = resolved + "#recurrence=" + url.QueryEscape(key)
		}
		if remote.RemoteID == existing.RemoteID {
			if remote.ICalUID != existing.ICalUID || remote.Deleted || remote.Status == "cancelled" {
				return nil, errCalendarUpdateConflict
			}
			matches++
			selected = event.Component
		}
	}
	if matches != 1 {
		return nil, errCalendarUpdateConflict
	}
	return selected, nil
}

// A nil draft means delete just this occurrence: add EXDATE and remove only its
// override. Never DELETE the .ics resource, which would remove the whole series.
func updateCalDAVCalendarOccurrence(ctx context.Context, source storage.CalendarSource, username, password string, existing storage.CalendarEvent, draft *calendar.EventDraft) (calendar.RemoteEvent, error) {
	preflight := func(err error) (calendar.RemoteEvent, error) {
		return calendar.RemoteEvent{}, calendarDeletePreflightError{err}
	}
	if err := calendarOccurrenceExistingRestriction(existing); err != nil {
		return preflight(err)
	}
	if !calendarUpdateValidETag(existing.ETag, false) {
		return preflight(errCalendarUpdateUnsupported)
	}
	endpoint, err := calendarUpdateCalDAVEndpoint(source, storage.CalendarEvent{RemoteID: existing.SeriesRemoteID})
	if err != nil {
		return preflight(err)
	}
	collection, _ := url.Parse(source.RemoteID)
	resource, _ := url.Parse(endpoint)
	if !strings.HasPrefix(resource.Path, strings.TrimSuffix(collection.Path, "/")+"/") || strings.HasSuffix(resource.Path, "/") || path.Clean(resource.Path) != resource.Path {
		return preflight(calendarUpdateUnsupported("The series must be a resource inside this calendar."))
	}
	if draft != nil {
		if draft.Recurrence != nil {
			return preflight(errCalendarUpdateUnsupported)
		}
		if _, err := calendarUpdateDraftLocation(*draft); err != nil {
			return preflight(err)
		}
	}
	client := calendarDeleteClient(calDAVHTTPTransport)
	current, headers, err := calendarUpdateCalDAVGet(ctx, client, endpoint, username, password)
	if err != nil {
		return preflight(err)
	}
	if headers.Get("ETag") != existing.ETag {
		return preflight(errCalendarUpdateConflict)
	}
	master, override, rid, err := calendarCalDAVOccurrenceResource(current, headers, existing)
	if err != nil {
		return preflight(err)
	}
	if err := calendarCalDAVVerifyOccurrence(ctx, source, username, password, existing); err != nil {
		return preflight(err)
	}
	if draft == nil {
		exdate := *rid
		exdate.Name = "EXDATE"
		master.Props.Add(&exdate)
		if override != nil {
			children := current.Children[:0]
			for _, child := range current.Children {
				if child != override {
					children = append(children, child)
				}
			}
			current.Children = children
		}
	} else {
		if override == nil {
			override = cloneCalendarComponent(master)
			for _, name := range []string{"RRULE", "RDATE", "EXDATE", "EXRULE"} {
				delete(override.Props, name)
			}
			override.Props.Set(rid)
			current.Children = append(current.Children, override)
		}
		raw, err := calendarCreateICS(*draft)
		if err != nil {
			return preflight(err)
		}
		edited, err := calendarUpdateDecodeICS([]byte(raw))
		if err != nil {
			return preflight(err)
		}
		for _, name := range []string{"SUMMARY", "DESCRIPTION", "LOCATION", "DTSTART", "DTEND", "X-GOFER-TIMEZONE"} {
			if name == "DESCRIPTION" && draft.Description == calendarDescriptionText(existing.Description) {
				continue
			}
			override.Props.Set(edited.Events()[0].Props.Get(name))
		}
		delete(override.Props, "DURATION")
		override.Props.SetDateTime("DTSTAMP", time.Now().UTC())
		override.Props.SetDateTime("LAST-MODIFIED", time.Now().UTC())
	}
	var body bytes.Buffer
	if err := ical.NewEncoder(&body).Encode(current); err != nil {
		return preflight(err)
	}
	req, err := newCardDAVRequest(ctx, http.MethodPut, endpoint, username, password, &body)
	if err != nil {
		return preflight(err)
	}
	req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
	req.Header.Set("If-Match", existing.ETag)
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
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm the occurrence change (HTTP %d)", status)
	}
	confirmed, headers, err := calendarUpdateCalDAVGet(ctx, client, endpoint, username, password)
	if err != nil {
		return calendar.RemoteEvent{}, fmt.Errorf("could not confirm the occurrence change: %v", err)
	}
	etag := headers.Get("ETag")
	if !calendarUpdateValidETag(etag, false) || (calendarUpdateValidETag(putETag, false) && putETag != etag) {
		return calendar.RemoteEvent{}, fmt.Errorf("the resource changed before confirmation")
	}
	_, saved, _, err := calendarCalDAVOccurrenceResource(confirmed, headers, existing)
	if err != nil {
		return calendar.RemoteEvent{}, fmt.Errorf("could not confirm the occurrence resource: %v", err)
	}
	// Servers may rewrite timestamps or reorder components. Compare the remaining
	// semantic content by UID/RECURRENCE-ID, including every untouched exception.
	if !calendarOccurrenceResourceMatches(current, confirmed) {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm the preserved series and exceptions")
	}
	if draft == nil {
		return calendar.RemoteEvent{RemoteID: existing.RemoteID, SeriesRemoteID: existing.SeriesRemoteID, ETag: etag, Deleted: true}, nil
	}
	if saved == nil {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not return the saved exception")
	}
	remote, err := normalizeCalDAVEvent(ical.Event{Component: saved}, endpoint, etag, time.UTC)
	if err != nil || remote.RemoteID != existing.RemoteID || !calendarUpdateMatchesDraft(remote, *draft) {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm the selected occurrence changes")
	}
	remote.StartTimeZone, remote.EndTimeZone = draft.TimeZone, draft.TimeZone
	return remote, nil
}

func calendarOccurrenceResourceMatches(expected, actual *ical.Calendar) bool {
	normalize := func(cal *ical.Calendar) map[string]*ical.Component {
		result := make(map[string]*ical.Component)
		for _, child := range cal.Children {
			copy := cloneCalendarComponent(child)
			delete(copy.Props, "DTSTAMP")
			delete(copy.Props, "LAST-MODIFIED")
			key := child.Name
			if child.Name == ical.CompEvent {
				if rid := child.Props.Get("RECURRENCE-ID"); rid != nil {
					value, err := calendarRecurrenceKey(rid, time.UTC)
					if err != nil {
						return nil
					}
					key += value
				}
			} else {
				id := child.Props.Get("TZID")
				if id == nil {
					return nil
				}
				key += id.Value
			}
			if result[key] != nil {
				return nil
			}
			result[key] = copy
		}
		return result
	}
	a, b := normalize(expected), normalize(actual)
	return a != nil && b != nil && reflect.DeepEqual(a, b)
}

// Deleting a series need not rewrite its rule or exceptions. Keep whole-series
// deletion available after a one-off edit, while retaining the invitation and
// ownership guards. Series editing still refuses to rewrite exception sets.
func calendarCalDAVDeleteSeriesEvent(cal *ical.Calendar, headers http.Header, endpoint string, location *time.Location) (calendar.RemoteEvent, error) {
	var root *ical.Component
	for _, event := range cal.Events() {
		if event.Props.Get("RECURRENCE-ID") == nil {
			root = event.Component
			break
		}
	}
	if root == nil {
		return calendar.RemoteEvent{}, errCalendarUpdateUnsupported
	}
	key, err := calendarRecurrenceKey(root.Props.Get("DTSTART"), location)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	uid, err := root.Props.Text("UID")
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	master, _, _, err := calendarCalDAVOccurrenceResource(cal, headers, storage.CalendarEvent{RemoteID: endpoint + "#recurrence=" + url.QueryEscape(key), SeriesRemoteID: endpoint, ICalUID: uid})
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	copy := &ical.Calendar{Component: cloneCalendarComponent(cal.Component)}
	copy.Children = nil
	for _, child := range cal.Children {
		if child.Name == ical.CompTimezone {
			copy.Children = append(copy.Children, cloneCalendarComponent(child))
		}
	}
	plain := cloneCalendarComponent(master)
	delete(plain.Props, "EXDATE")
	copy.Children = append(copy.Children, plain)
	return calendarCalDAVSeriesEvent(copy, headers, endpoint, location)
}
