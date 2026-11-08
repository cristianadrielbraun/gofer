package handler

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// A weekly review on four Wednesdays, with the second one skipped and the
// third moved to Thursday, and a lunch on its own.
const calDAVReviewStored = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//Test//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:review\r\nDTSTAMP:20260101T000000Z\r\nDTSTART;TZID=Europe/Zurich:20261202T090000\r\nDTEND;TZID=Europe/Zurich:20261202T100000\r\nRRULE:FREQ=WEEKLY;COUNT=4\r\nEXDATE;TZID=Europe/Zurich:20261209T090000\r\nSUMMARY:Review\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:review\r\nDTSTAMP:20260101T000000Z\r\nRECURRENCE-ID;TZID=Europe/Zurich:20261216T090000\r\nDTSTART;TZID=Europe/Zurich:20261217T100000\r\nDTEND;TZID=Europe/Zurich:20261217T110000\r\nSUMMARY:Review moved\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

const calDAVLunchStored = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//Test//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:lunch\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20261205T100000Z\r\nDTEND:20261205T110000Z\r\nSUMMARY:Lunch\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// cannotExpandServer answers like Baïkal with a calendar whose timezone its
// admin pages stored: every request that asks to expand fails.
type cannotExpandServer struct {
	mu       sync.Mutex
	expanded int // expanding requests, all refused
	plain    int // requests for the events as stored
}

func (s *cannotExpandServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.Contains(string(body), "expand") {
		s.expanded++
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	s.plain++
	resources := map[string]string{"/cal/review.ics": calDAVReviewStored, "/cal/lunch.ics": calDAVLunchStored}
	if strings.Contains(string(body), "calendar-multiget") {
		resources = map[string]string{"/cal/review.ics": calDAVReviewStored}
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusMultiStatus)
	fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`)
	hrefs := make([]string, 0, len(resources))
	for href := range resources {
		hrefs = append(hrefs, href)
	}
	sort.Strings(hrefs)
	for _, href := range hrefs {
		var escaped strings.Builder
		_ = xml.EscapeText(&escaped, []byte(resources[href]))
		fmt.Fprintf(w, `<d:response><d:href>%s</d:href><d:propstat><d:prop><d:getetag>"1"</d:getetag><c:calendar-data>%s</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, href, escaped.String())
	}
	fmt.Fprint(w, `</d:multistatus>`)
}

func (s *cannotExpandServer) counts() (expanded, plain int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expanded, s.plain
}

func TestCalDAVSyncExpandsItselfWhenTheServerCannot(t *testing.T) {
	fake := &cannotExpandServer{}
	server := httptest.NewTLSServer(fake)
	defer server.Close()
	prior := davHTTPTransport
	davHTTPTransport = server.Client().Transport
	defer func() { davHTTPTransport = prior }()
	source := storage.CalendarSource{ID: "source", Provider: "caldav", RemoteID: server.URL + "/cal/", TimeZone: "Europe/Zurich"}
	query := calendar.EventQuery{WindowStart: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), WindowEnd: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}

	page, err := listCalDAVCalendarEvents(davTestContext(t), source, "user", "pass", query)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, event := range page.Events {
		kind := "single"
		if event.SeriesRemoteID != "" {
			kind = "series"
		}
		got = append(got, event.StartAt.UTC().Format("Jan 2 15:04")+" "+event.Summary+" ("+kind+")")
	}
	sort.Strings(got)
	want := "Dec 17 09:00 Review moved (series); Dec 2 08:00 Review (series); Dec 23 08:00 Review (series); Dec 5 10:00 Lunch (single)"
	if strings.Join(got, "; ") != want {
		t.Fatalf("events:\n%s\nwant\n%s", strings.Join(got, "; "), want)
	}
	if expanded, plain := fake.counts(); expanded != 1 || plain != 1 {
		t.Fatalf("first sync sent %d expanding and %d plain requests, want 1 and 1", expanded, plain)
	}

	// The calendar is remembered, so the next sync doesn't fail on the server first.
	if _, err := listCalDAVCalendarEvents(davTestContext(t), source, "user", "pass", query); err != nil {
		t.Fatal(err)
	}
	if expanded, plain := fake.counts(); expanded != 1 || plain != 2 {
		t.Fatalf("after the second sync: %d expanding and %d plain requests, want 1 and 2", expanded, plain)
	}

	// Editing one date of the series checks it against the server's copy,
	// expanded by Gofer too.
	var last calendar.RemoteEvent
	for _, event := range page.Events {
		if event.Summary == "Review" && event.StartAt.Day() == 23 {
			last = event
		}
	}
	existing := storage.CalendarEvent{RemoteID: last.RemoteID, SeriesRemoteID: last.SeriesRemoteID, ETag: last.ETag, ICalUID: last.ICalUID, StartAt: last.StartAt, EndAt: last.EndAt}
	component, err := calendarCalDAVReadOccurrence(davTestContext(t), source, "user", "pass", existing)
	if err != nil {
		t.Fatalf("read occurrence %s: %v", existing.RemoteID, err)
	}
	if summary, _ := component.Props.Text("SUMMARY"); summary != "Review" {
		t.Fatalf("occurrence summary = %q", summary)
	}
}

// A server that fails for other reasons isn't asked again without expansion.
func TestCalDAVSyncLeavesOtherServerFailuresToTheRetry(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	prior := davHTTPTransport
	davHTTPTransport = server.Client().Transport
	defer func() { davHTTPTransport = prior }()
	source := storage.CalendarSource{ID: "source", Provider: "caldav", RemoteID: server.URL + "/cal/"}
	query := calendar.EventQuery{WindowStart: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), WindowEnd: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	if _, err := listCalDAVCalendarEvents(davTestContext(t), source, "user", "pass", query); err == nil || requests != 1 {
		t.Fatalf("err = %v after %d requests, want a failure after 1", err, requests)
	}
}

func TestCalDAVCalendarTimeZoneAcceptsABareName(t *testing.T) {
	for value, want := range map[string]string{
		"Europe/Zurich":   "Europe/Zurich",
		" Europe/Paris\n": "Europe/Paris",
		"Not/AZone":       "",
		"Local":           "",
		"BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:Europe/Prague\r\nEND:VTIMEZONE\r\nEND:VCALENDAR\r\n": "Europe/Prague",
	} {
		if got := calDAVCalendarTimeZone(value); got != want {
			t.Errorf("calDAVCalendarTimeZone(%q) = %q, want %q", value, got, want)
		}
	}
}
