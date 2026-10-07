package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func identifyCalDAVInitialOccurrences(ctx context.Context, source storage.CalendarSource, username, password string, events []calendar.RemoteEvent) error {
	var pending []*calendar.RemoteEvent
	for i := range events {
		if events[i].SeriesRemoteID == "" && !events[i].Deleted {
			pending = append(pending, &events[i])
		}
	}
	for len(pending) > 0 {
		n := min(len(pending), 100)
		batch := make(map[string]*calendar.RemoteEvent, n)
		var body bytes.Buffer
		body.WriteString(`<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/><c:calendar-data><c:comp name="VCALENDAR"><c:comp name="VEVENT"><c:prop name="UID"/><c:prop name="DTSTART"/><c:prop name="RRULE"/><c:prop name="RDATE"/><c:prop name="RECURRENCE-ID"/></c:comp></c:comp></c:calendar-data></d:prop>`)
		for _, event := range pending[:n] {
			resource, err := url.Parse(event.RemoteID)
			if err != nil {
				return err
			}
			batch[event.RemoteID] = event
			body.WriteString("<d:href>")
			_ = xml.EscapeText(&body, []byte(resource.EscapedPath()))
			body.WriteString("</d:href>")
		}
		body.WriteString("</c:calendar-multiget>")
		multi, err := calDAVRequest(ctx, "REPORT", source.RemoteID, username, password, "1", body.String(), 30*time.Second)
		if err != nil {
			return err
		}
		for _, response := range multi.Responses {
			href, err := calendarSyncDAVResource(ctx, source.RemoteID, response.Href)
			if err != nil {
				return err
			}
			event := batch[href]
			if event == nil || (response.Status != "" && !strings.Contains(response.Status, " 200 ")) {
				return fmt.Errorf("CalDAV returned inconsistent original event metadata")
			}
			var data, etag string
			for _, stat := range response.PropStats {
				if !strings.Contains(stat.Status, " 200 ") {
					return fmt.Errorf("CalDAV original event metadata is incomplete")
				}
				if stat.Prop.CalendarData != "" {
					data = stat.Prop.CalendarData
				}
				if stat.Prop.GetETag != "" {
					etag = strings.TrimSpace(stat.Prop.GetETag)
				}
			}
			if etag != event.ETag {
				return fmt.Errorf("CalDAV event changed while identifying its recurrence")
			}
			cal, err := calendarUpdateDecodeICS([]byte(data))
			if err != nil {
				return err
			}
			masters := 0
			for _, component := range cal.Events() {
				if component.Props.Get("RECURRENCE-ID") != nil {
					continue
				}
				masters++
				uid, err := component.Props.Text("UID")
				if err != nil || uid != event.ICalUID {
					return fmt.Errorf("CalDAV original event identity changed")
				}
				if component.Props.Get("RRULE") != nil || component.Props.Get("RDATE") != nil {
					key := ""
					if event.AllDay {
						key = event.StartDate
					} else if event.StartAt != nil {
						key = event.StartAt.UTC().Format(time.RFC3339)
					}
					if key == "" {
						return fmt.Errorf("CalDAV initial occurrence has no start")
					}
					location := time.UTC
					zone := source.TimeZone
					if zone == "" {
						zone = event.StartTimeZone
					}
					if zone != "" {
						location, err = time.LoadLocation(zone)
						if err != nil {
							return err
						}
					}
					original, err := calendarRecurrenceKey(component.Props.Get("DTSTART"), location)
					if err != nil || original != key {
						return fmt.Errorf("CalDAV initial recurrence identity changed")
					}
					event.SeriesRemoteID = href
					event.RemoteID = href + "#recurrence=" + url.QueryEscape(key)
					event.Recurrence, _ = json.Marshal([]string{"RECURRENCE-ID:" + key})
				}
			}
			if masters != 1 {
				return fmt.Errorf("CalDAV initial event metadata is ambiguous")
			}
			delete(batch, href)
		}
		if len(batch) != 0 {
			return fmt.Errorf("CalDAV omitted original event metadata")
		}
		pending = pending[n:]
	}
	return nil
}
