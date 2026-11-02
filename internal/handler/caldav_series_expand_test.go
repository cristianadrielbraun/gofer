package handler

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// A daily series on the 1st, 2nd and 3rd, with the 1st moved to the 2nd
// and the 2nd moved to the 4th, as stored.
const calDAVMovedDatesStored = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//Test//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:standup\r\nDTSTAMP:20260101T000000Z\r\nRECURRENCE-ID;TZID=Europe/Prague:20261201T090000\r\nDTSTART;TZID=Europe/Prague:20261202T090000\r\nDTEND;TZID=Europe/Prague:20261202T093000\r\nSUMMARY:Standup moved\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:standup\r\nDTSTAMP:20260101T000000Z\r\nRECURRENCE-ID;TZID=Europe/Prague:20261202T090000\r\nDTSTART;TZID=Europe/Prague:20261204T090000\r\nDTEND;TZID=Europe/Prague:20261204T093000\r\nSUMMARY:Standup moved again\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:standup\r\nDTSTAMP:20260101T000000Z\r\nDTSTART;TZID=Europe/Prague:20261201T090000\r\nDTEND;TZID=Europe/Prague:20261201T093000\r\nRRULE:FREQ=DAILY;COUNT=3\r\nSUMMARY:Standup\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// The same series as Stalwart expands it: each moved date labelled with the
// day it moved to, and every date of the series kept.
const calDAVMovedDatesExpandedByStalwart = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//Test//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:standup\r\nDTSTAMP:20260101T000000Z\r\nRECURRENCE-ID:20261202T080000Z\r\nDTSTART:20261202T080000Z\r\nDTEND:20261202T083000Z\r\nSUMMARY:Standup moved\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:standup\r\nDTSTAMP:20260101T000000Z\r\nRECURRENCE-ID:20261204T080000Z\r\nDTSTART:20261204T080000Z\r\nDTEND:20261204T083000Z\r\nSUMMARY:Standup moved again\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:standup\r\nDTSTAMP:20260101T000000Z\r\nRECURRENCE-ID:20261201T080000Z\r\nDTSTART:20261201T080000Z\r\nDTEND:20261201T083000Z\r\nSUMMARY:Standup\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:standup\r\nDTSTAMP:20260101T000000Z\r\nRECURRENCE-ID:20261202T080000Z\r\nDTSTART:20261202T080000Z\r\nDTEND:20261202T083000Z\r\nSUMMARY:Standup\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:standup\r\nDTSTAMP:20260101T000000Z\r\nRECURRENCE-ID:20261203T080000Z\r\nDTSTART:20261203T080000Z\r\nDTEND:20261203T083000Z\r\nSUMMARY:Standup\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func TestCalDAVSyncExpandsSeriesWithMovedDatesItself(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		data := calDAVMovedDatesExpandedByStalwart
		if strings.Contains(string(body), "calendar-multiget") {
			data = calDAVMovedDatesStored
		}
		var escaped strings.Builder
		_ = xml.EscapeText(&escaped, []byte(data))
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>/cal/standup.ics</d:href><d:propstat><d:prop><d:getetag>"1"</d:getetag><c:calendar-data>%s</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, escaped.String())
	}))
	defer server.Close()
	prior := davHTTPTransport
	davHTTPTransport = server.Client().Transport
	defer func() { davHTTPTransport = prior }()
	source := storage.CalendarSource{ID: "source", Provider: "caldav", RemoteID: server.URL + "/cal/"}
	query := calendar.EventQuery{WindowStart: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), WindowEnd: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	page, err := listCalDAVCalendarEvents(t.Context(), source, "user", "pass", query)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, event := range page.Events {
		date, _, _ := strings.Cut(event.RemoteID[strings.Index(event.RemoteID, "#recurrence=")+len("#recurrence="):], "T")
		got = append(got, date+" at "+event.StartAt.UTC().Format("Jan 2")+": "+event.Summary)
	}
	sort.Strings(got)
	want := "2026-12-01 at Dec 2: Standup moved; 2026-12-02 at Dec 4: Standup moved again; 2026-12-03 at Dec 3: Standup"
	if strings.Join(got, "; ") != want {
		t.Fatalf("occurrences:\n%s\nwant\n%s", strings.Join(got, "; "), want)
	}
}
