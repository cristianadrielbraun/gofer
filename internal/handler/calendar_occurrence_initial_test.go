package handler

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestCalendarCalDAVInitialOccurrenceMetadata(t *testing.T) {
	for _, mode := range []string{"timed", "all-day", "single", "missing", "changed-version", "wrong-uid", "wrong-date", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			date := "DTSTART;TZID=Europe/Prague:20261030T090000"
			if mode == "all-day" {
				date = "DTSTART;VALUE=DATE:20261030"
			}
			rule := "RRULE:FREQ=YEARLY;INTERVAL=5\n" // No sibling exists in the visible window.
			if mode == "single" {
				rule = ""
			}
			metadata := "BEGIN:VCALENDAR\nVERSION:2.0\nBEGIN:VEVENT\nUID:uid\n" + date + "\n" + rule + "END:VEVENT\nEND:VCALENDAR\n"
			if mode == "wrong-uid" {
				metadata = strings.Replace(metadata, "UID:uid", "UID:other", 1)
			}
			if mode == "wrong-date" {
				metadata = strings.Replace(metadata, "20261030", "20261031", 1)
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.Method != "REPORT" || !strings.Contains(string(body), `calendar-multiget`) || !strings.Contains(string(body), `<c:prop name="RRULE"/>`) || strings.Contains(string(body), `<c:expand`) {
					t.Error("metadata report must read original recurrence metadata")
				}
				etag := `"v1"`
				if mode == "changed-version" {
					etag = `"v2"`
				}
				entry := fmt.Sprintf(`<d:response><d:href>/cal/first.ics</d:href><d:propstat><d:prop><d:getetag>%s</d:getetag><c:calendar-data><![CDATA[%s]]></c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, etag, metadata)
				if mode == "missing" {
					entry = ""
				}
				if mode == "duplicate" {
					entry += entry
				}
				w.WriteHeader(207)
				_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">%s</d:multistatus>`, entry)
			}))
			defer server.Close()
			previous := calDAVHTTPTransport
			calDAVHTTPTransport = server.Client().Transport
			defer func() { calDAVHTTPTransport = previous }()
			at := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
			events := []calendar.RemoteEvent{{RemoteID: server.URL + "/cal/first.ics", ICalUID: "uid", ETag: `"v1"`, StartAt: &at}}
			if mode == "all-day" {
				events[0].AllDay, events[0].StartDate = true, "2026-10-30"
				events[0].StartAt = nil
			}
			err := identifyCalDAVInitialOccurrences(t.Context(), storage.CalendarSource{RemoteID: server.URL + "/cal/", TimeZone: "Europe/Prague"}, "user", "pass", events)
			switch mode {
			case "timed", "all-day":
				if err != nil || events[0].SeriesRemoteID != server.URL+"/cal/first.ics" || !strings.Contains(events[0].RemoteID, "#recurrence=2026-10-30") {
					t.Fatalf("initial occurrence: %+v %v", events[0], err)
				}
			case "single":
				if err != nil || events[0].SeriesRemoteID != "" {
					t.Fatalf("standalone event reclassified: %+v %v", events[0], err)
				}
			default:
				if err == nil {
					t.Fatal("accepted incomplete or inconsistent metadata")
				}
			}
		})
	}
}
