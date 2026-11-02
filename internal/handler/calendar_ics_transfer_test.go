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

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const calendarImportTestFile = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//Example//Test//EN\r\n" +
	"METHOD:PUBLISH\r\n" +
	"X-WR-CALNAME:Garden club\r\n" +
	"BEGIN:VTIMEZONE\r\n" +
	"TZID:Europe/Paris\r\n" +
	"BEGIN:STANDARD\r\n" +
	"DTSTART:19701025T030000\r\n" +
	"TZOFFSETFROM:+0200\r\n" +
	"TZOFFSETTO:+0100\r\n" +
	"END:STANDARD\r\n" +
	"END:VTIMEZONE\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:weekly-pruning\r\n" +
	"DTSTAMP:20260101T000000Z\r\n" +
	"DTSTART;TZID=Europe/Paris:20260105T090000\r\n" +
	"DTEND;TZID=Europe/Paris:20260105T100000\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=4\r\n" +
	"SUMMARY:Pruning\r\n" +
	"ORGANIZER;SCHEDULE-STATUS=1.2:mailto:robin@example.com\r\n" +
	"ATTENDEE;PARTSTAT=ACCEPTED:mailto:sam@example.com\r\n" +
	"BEGIN:VALARM\r\nACTION:DISPLAY\r\nDESCRIPTION:Reminder\r\nTRIGGER:-PT1H\r\nEND:VALARM\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:seed-swap\r\n" +
	"DTSTAMP:20260101T000000Z\r\n" +
	"DTSTART;VALUE=DATE:20260301\r\n" +
	"SUMMARY:Seed swap\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VTODO\r\n" +
	"UID:water-plants\r\n" +
	"SUMMARY:Water the plants\r\n" +
	"END:VTODO\r\n" +
	"END:VCALENDAR\r\n" +
	"BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//Example//Test//EN\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:weekly-pruning\r\n" +
	"DTSTAMP:20260101T000000Z\r\n" +
	"RECURRENCE-ID;TZID=Europe/Paris:20260112T090000\r\n" +
	"DTSTART;TZID=Europe/Paris:20260112T110000\r\n" +
	"DTEND;TZID=Europe/Paris:20260112T120000\r\n" +
	"SUMMARY:Pruning (late)\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"DTSTAMP:20260101T000000Z\r\n" +
	"DTSTART:20260401T080000Z\r\n" +
	"SUMMARY:No UID\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func TestCalendarImportResources(t *testing.T) {
	resources, err := calendarImportResources([]byte(calendarImportTestFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 3 {
		t.Fatalf("resources = %d, want 3 (one per UID)", len(resources))
	}
	pruning, err := encodeCalendarReply(resources[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"BEGIN:VTIMEZONE", "TZID:Europe/Paris", "RECURRENCE-ID", "Pruning (late)", "SCHEDULE-AGENT=CLIENT"} {
		if !strings.Contains(pruning, want) {
			t.Errorf("pruning resource lacks %q:\n%s", want, pruning)
		}
	}
	for _, unwanted := range []string{"METHOD", "SCHEDULE-STATUS", "X-WR-CALNAME", "VTODO"} {
		if strings.Contains(pruning, unwanted) {
			t.Errorf("pruning resource has %q:\n%s", unwanted, pruning)
		}
	}
	swap, _ := encodeCalendarReply(resources[1])
	if strings.Contains(swap, "VTIMEZONE") {
		t.Errorf("seed swap resource carries an unused time zone:\n%s", swap)
	}
	uid, _ := resources[2].Events()[0].Props.Text("UID")
	if !strings.HasPrefix(uid, "gofer-import-") {
		t.Errorf("an event without a UID was given %q", uid)
	}
	if again, _ := calendarImportResources([]byte(calendarImportTestFile)); len(again) != 3 {
		t.Fatal("the file reads differently a second time")
	} else if second, _ := again[2].Events()[0].Props.Text("UID"); second != uid {
		t.Errorf("an event without a UID was given %q, then %q: importing again would add it twice", uid, second)
	}

	for name, data := range map[string]string{
		"not ics":   "hello",
		"no events": "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VTODO\r\nUID:a\r\nEND:VTODO\r\nEND:VCALENDAR\r\n",
		"empty":     "",
	} {
		if _, err := calendarImportResources([]byte(data)); err == nil {
			t.Errorf("%s: imported", name)
		}
	}
}

func TestCalDAVImportThenExport(t *testing.T) {
	var mu sync.Mutex
	resources := map[string]string{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "PUT":
			data, _ := io.ReadAll(r.Body)
			if r.Header.Get("If-None-Match") != "*" || !strings.HasPrefix(r.URL.Path, "/cal/") {
				w.WriteHeader(400)
				return
			}
			// Like most servers, refuse a UID the calendar already has.
			for _, existing := range resources {
				if strings.Contains(string(data), "UID:seed-swap") && strings.Contains(existing, "UID:seed-swap") {
					w.WriteHeader(403)
					return
				}
			}
			resources[r.URL.Path] = string(data)
			w.WriteHeader(201)
		case "REPORT":
			paths := make([]string, 0, len(resources))
			for path := range resources {
				paths = append(paths, path)
			}
			sort.Strings(paths)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(207)
			io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`)
			for _, path := range paths {
				var escaped strings.Builder
				_ = xml.EscapeText(&escaped, []byte(resources[path]))
				fmt.Fprintf(w, `<d:response><d:href>%s</d:href><d:propstat><d:prop><d:getetag>"1"</d:getetag><c:calendar-data>%s</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, path, escaped.String())
			}
			io.WriteString(w, `</d:multistatus>`)
		default:
			w.WriteHeader(405)
		}
	}))
	defer server.Close()
	prior := davHTTPTransport
	davHTTPTransport = server.Client().Transport
	defer func() { davHTTPTransport = prior }()
	source := storage.CalendarSource{ID: "source", AccountID: "account", Provider: "caldav", Name: "Garden club", RemoteID: server.URL + "/cal/", AccessRole: "owner"}
	credentials := calendarCredentials{username: "user", password: "pass", baseURL: source.RemoteID}

	parsed, err := calendarImportResources([]byte(calendarImportTestFile))
	if err != nil {
		t.Fatal(err)
	}
	var steps []int
	result, err := importCalDAVEvents(t.Context(), source, credentials, parsed, nil, func(counts calendarImportResult) {
		steps = append(steps, counts.done())
	})
	if err != nil || result != (calendarImportResult{Added: 3}) {
		t.Fatalf("first import = %+v, %v", result, err)
	}
	if fmt.Sprint(steps) != "[1 2 3]" {
		t.Errorf("progress = %v, want one report per event", steps)
	}
	result, err = importCalDAVEvents(t.Context(), source, credentials, parsed[1:2], nil, nil)
	if err != nil || result != (calendarImportResult{Existing: 1}) {
		t.Fatalf("second import of seed swap = %+v, %v", result, err)
	}

	data, err := exportCalDAVEvents(t.Context(), source, credentials)
	if err != nil {
		t.Fatal(err)
	}
	exported := string(data)
	if strings.Count(exported, "BEGIN:VEVENT") != 4 || strings.Count(exported, "BEGIN:VTIMEZONE") != 1 || strings.Count(exported, "BEGIN:VCALENDAR") != 1 {
		t.Fatalf("export has the wrong shape:\n%s", exported)
	}
	if !strings.Contains(exported, "X-WR-CALNAME:Garden club") {
		t.Errorf("export lacks the calendar name:\n%s", exported)
	}
	// The export imports again as the same three events.
	again, err := calendarImportResources(data)
	if err != nil || len(again) != 3 {
		t.Fatalf("export reimports as %d resources, %v", len(again), err)
	}
}

func TestCalDAVExportEmptyCalendar(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(207)
		io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"/>`)
	}))
	defer server.Close()
	prior := davHTTPTransport
	davHTTPTransport = server.Client().Transport
	defer func() { davHTTPTransport = prior }()
	source := storage.CalendarSource{Provider: "caldav", RemoteID: server.URL + "/cal/"}
	data, err := exportCalDAVEvents(t.Context(), source, calendarCredentials{username: "u", password: "p", baseURL: source.RemoteID})
	if err != nil || !strings.HasPrefix(string(data), "BEGIN:VCALENDAR") {
		t.Fatalf("empty export = %q, %v", data, err)
	}
}

func TestCalendarExportFilename(t *testing.T) {
	for name, want := range map[string]string{"Garden club": "Garden club.ics", "a/b\\c": "abc.ics", "": "calendar.ics", "..": "calendar.ics", "Café": "Café.ics"} {
		if got := calendarExportFilename(name); got != want {
			t.Errorf("calendarExportFilename(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestCalendarImportRequestChecks(t *testing.T) {
	h := &Handler{}
	for _, test := range []struct{ query, body, want string }{
		{"", calendarImportTestFile, "Choose a calendar"},
		{"?source=s1&account=a1&name=Garden", calendarImportTestFile, "Choose a calendar"},
		{"?account=a1", calendarImportTestFile, "Enter a name"},
		{"?account=a1&name=Line%0Abreak", calendarImportTestFile, "line breaks"},
		{"?source=s1", "not a calendar", "iCalendar"},
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/calendar/import"+test.query, strings.NewReader(test.body))
		response := httptest.NewRecorder()
		h.handleCalendarImport(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), test.want) {
			t.Errorf("%s: %d %q, want 400 with %q", test.query, response.Code, response.Body.String(), test.want)
		}
	}
}

// Events a CalDAV server refuses and doesn't have count as failed, with the
// first one's reason, not as already in the calendar.
func TestCalDAVImportTellsRefusedFromExisting(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "REPORT" {
			w.WriteHeader(http.StatusMultiStatus)
			io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"/>`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	prior := davHTTPTransport
	davHTTPTransport = server.Client().Transport
	defer func() { davHTTPTransport = prior }()
	source := storage.CalendarSource{ID: "source", Provider: "caldav", RemoteID: server.URL + "/cal/", AccessRole: "owner"}
	resources, err := calendarImportResources([]byte(calendarImportTestFile))
	if err != nil {
		t.Fatal(err)
	}
	result, err := importCalDAVEvents(t.Context(), source, calendarCredentials{username: "u", password: "p", baseURL: source.RemoteID}, resources, nil, nil)
	if err != nil || result.Added != 0 || result.Existing != 0 || result.Failed != 3 || result.Reason != "“Pruning”: the calendar refused it (HTTP 400)." {
		t.Fatalf("import = %+v, %v", result, err)
	}
	// The same, with the calendar's UIDs read first.
	result, err = importCalendarEvents(t.Context(), source, calendarCredentials{username: "u", password: "p", baseURL: source.RemoteID}, resources, nil)
	if err != nil || result.Existing != 0 || result.Failed != 3 {
		t.Fatalf("import with the UIDs read first = %+v, %v", result, err)
	}
}

// The calendar's UIDs are read once, and events it has aren't sent at all,
// so a server that would accept a second event with a UID doesn't get one.
func TestCalDAVImportReadsTheCalendarsUIDsFirst(t *testing.T) {
	var puts, reports int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			reports++
			w.WriteHeader(http.StatusMultiStatus)
			io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>/cal/a.ics</d:href><d:propstat><d:prop><c:calendar-data>BEGIN:VCALENDAR&#13;
VERSION:2.0&#13;
PRODID:x&#13;
BEGIN:VEVENT&#13;
UID:seed-swap&#13;
END:VEVENT&#13;
END:VCALENDAR&#13;
</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
		case http.MethodPut:
			puts++
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer server.Close()
	prior := davHTTPTransport
	davHTTPTransport = server.Client().Transport
	defer func() { davHTTPTransport = prior }()
	source := storage.CalendarSource{ID: "source", Provider: "caldav", RemoteID: server.URL + "/cal/", AccessRole: "owner"}
	resources, err := calendarImportResources([]byte(calendarImportTestFile))
	if err != nil {
		t.Fatal(err)
	}
	result, err := importCalendarEvents(t.Context(), source, calendarCredentials{username: "u", password: "p", baseURL: source.RemoteID}, resources, nil)
	if err != nil || result != (calendarImportResult{Added: 2, Existing: 1}) || puts != 2 || reports != 1 {
		t.Fatalf("import = %+v, %v; %d uploads, %d reports", result, err, puts, reports)
	}
}
