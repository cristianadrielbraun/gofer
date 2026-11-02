package handler

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

// calDAVResourceEvents is one event resource of a CalDAV calendar as the
// server expanded it in the sync window.
type calDAVResourceEvents struct {
	href, etag string
	events     []calendar.RemoteEvent
}

// expandCalDAVChangedSeries replaces the server's expansion of each series
// with dates changed on their own with Gofer's, read from the stored event,
// as JMAP calendars are read. Servers get these wrong: Stalwart labels a
// moved date with the day it moved to and keeps the date it replaced, so the
// series shows dates twice or on days it was moved from. Series Gofer can't
// read keep the server's expansion.
func expandCalDAVChangedSeries(ctx context.Context, source storage.CalendarSource, username, password string, resources []calDAVResourceEvents, start, end time.Time, location *time.Location) error {
	index := map[string]int{}
	var series []string
	for i, resource := range resources {
		for _, event := range resource.events {
			if event.SeriesRemoteID != "" {
				index[resource.href] = i
				series = append(series, resource.href)
				break
			}
		}
	}
	for len(series) > 0 {
		n := min(len(series), 100)
		var body bytes.Buffer
		body.WriteString(`<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/><c:calendar-data/></d:prop>`)
		for _, href := range series[:n] {
			resource, err := url.Parse(href)
			if err != nil {
				return err
			}
			body.WriteString("<d:href>")
			_ = xml.EscapeText(&body, []byte(resource.EscapedPath()))
			body.WriteString("</d:href>")
		}
		body.WriteString("</c:calendar-multiget>")
		series = series[n:]
		multi, err := calDAVRequest(ctx, "REPORT", source.RemoteID, username, password, "1", body.String(), 30*time.Second)
		if err != nil {
			return err
		}
		for _, response := range multi.Responses {
			href, err := calendarSyncDAVResource(ctx, source.RemoteID, response.Href)
			if err != nil {
				return err
			}
			i, ok := index[href]
			if !ok || (response.Status != "" && !strings.Contains(response.Status, " 200 ")) {
				continue
			}
			var data, etag string
			for _, stat := range response.PropStats {
				if strings.Contains(stat.Status, " 200 ") {
					if stat.Prop.CalendarData != "" {
						data = stat.Prop.CalendarData
					}
					if stat.Prop.GetETag != "" {
						etag = strings.TrimSpace(stat.Prop.GetETag)
					}
				}
			}
			if etag != resources[i].etag {
				return fmt.Errorf("CalDAV event changed while reading its series")
			}
			events, ok, err := expandCalDAVStoredSeries(data, href, etag, start, end, location)
			if err != nil {
				log.Printf("calendar sync %s: expand %s: %v", source.ID, href, err)
				continue
			}
			if ok {
				resources[i].events = events
			}
		}
	}
	return nil
}

// expandCalDAVStoredSeries expands a stored series that has dates changed
// on their own; ok is false for any other event.
func expandCalDAVStoredSeries(data, href, etag string, start, end time.Time, location *time.Location) ([]calendar.RemoteEvent, bool, error) {
	cal, err := calendarUpdateDecodeICS([]byte(data))
	if err != nil {
		return nil, false, err
	}
	var master *ical.Event
	var changed []ical.Event
	for _, component := range cal.Events() {
		if component.Props.Get(ical.PropRecurrenceID) != nil {
			changed = append(changed, component)
			continue
		}
		if master != nil {
			return nil, false, fmt.Errorf("the event has two series")
		}
		master = &component
	}
	if master == nil || len(changed) == 0 || (master.Props.Get(ical.PropRecurrenceRule) == nil && master.Props.Get(ical.PropRecurrenceDates) == nil) {
		return nil, false, nil
	}
	expanded, err := expandCalendarSeries(append([]ical.Event{*master}, changed...), href, etag, start, end, location)
	if err != nil {
		return nil, false, err
	}
	events := make([]calendar.RemoteEvent, 0, len(expanded))
	for _, occurrence := range expanded {
		events = append(events, occurrence.remote)
	}
	return events, true, nil
}
