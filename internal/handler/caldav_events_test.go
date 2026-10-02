package handler

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestParseCalDAVEvents(t *testing.T) {
	prague, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, properties, start, end, startDate, endDate string
	}{
		{"timezone and folded text", "DTSTART;TZID=Europe/Prague:20260917T090000\nDTEND;TZID=Europe/Prague:20260917T100000\nSUMMARY:Planning\\, te\n am\\nReview", "2026-09-17T07:00:00Z", "2026-09-17T08:00:00Z", "", ""},
		{"duration", "DTSTART:20260917T090000Z\nDURATION:PT90M", "2026-09-17T09:00:00Z", "2026-09-17T10:30:00Z", "", ""},
		{"duration across DST", "DTSTART;TZID=Europe/Prague:20261024T120000\nDURATION:P2D", "2026-10-24T10:00:00Z", "2026-10-26T11:00:00Z", "", ""},
		{"floating time", "DTSTART:20260917T090000\nDTEND:20260917T100000", "2026-09-17T07:00:00Z", "2026-09-17T08:00:00Z", "", ""},
		{"instant", "DTSTART:20260917T090000Z", "2026-09-17T09:00:00Z", "2026-09-17T09:00:00Z", "", ""},
		{"exclusive all day", "DTSTART;VALUE=DATE:20260917\nDTEND;VALUE=DATE:20260920", "", "", "2026-09-17", "2026-09-20"},
		{"all day across DST", "DTSTART;VALUE=DATE:20261025", "", "", "2026-10-25", "2026-10-26"},
		{"all day duration", "DTSTART;VALUE=DATE:20261025\nDURATION:P2D", "", "", "2026-10-25", "2026-10-27"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := "BEGIN:VCALENDAR\nVERSION:2.0\nBEGIN:VEVENT\nUID:meeting@example.com\n" + test.properties + "\nEND:VEVENT\nEND:VCALENDAR\n"
			events, err := parseCalDAVEvents("https://dav.example/main/event.ics", `"etag-1"`, data, prague)
			if err != nil || len(events) != 1 {
				t.Fatalf("parse = %#v, %v", events, err)
			}
			event := events[0]
			if event.StartDate != test.startDate || event.EndDate != test.endDate || event.AllDay != (test.startDate != "") {
				t.Fatalf("unexpected date range: %#v", event)
			}
			if test.start != "" && (event.StartAt.Format(time.RFC3339) != test.start || event.EndAt.Format(time.RFC3339) != test.end) {
				t.Fatalf("unexpected time range: %#v", event)
			}
			if test.name == "timezone and folded text" && event.Summary != "Planning, team\nReview" {
				t.Fatalf("unfolded summary = %q", event.Summary)
			}
		})
	}
	for _, properties := range []string{
		"DTSTART:20260917T090000Z\nRRULE:FREQ=WEEKLY",
		"DTSTART:20260917T090000Z\nDTEND:20260917T080000Z",
		"DTSTART;VALUE=DATE:20260917\nDTEND:20260918T000000Z",
		"DTSTART;TZID=Unknown/Zone:20260917T090000",
	} {
		data := "BEGIN:VCALENDAR\nVERSION:2.0\nBEGIN:VEVENT\nUID:invalid@example.com\n" + properties + "\nEND:VEVENT\nEND:VCALENDAR\n"
		if _, err := parseCalDAVEvents("https://dav.example/main/invalid.ics", "", data, time.UTC); err == nil {
			t.Fatalf("accepted unsafe event data: %s", properties)
		}
	}
}

const calDAVExpandedFixture = `BEGIN:VCALENDAR
VERSION:2.0
BEGIN:VEVENT
UID:weekly@example.com
DTSTART:20260917T090000Z
DTEND:20260917T100000Z
SUMMARY:Weekly planning
ORGANIZER;CN=Organizer:mailto:organizer@example.com
ATTENDEE;CN=Person;PARTSTAT=ACCEPTED:mailto:person@example.com
END:VEVENT
BEGIN:VEVENT
UID:weekly@example.com
RECURRENCE-ID:20260924T090000Z
DTSTART:20260924T110000Z
DTEND:20260924T120000Z
SUMMARY:Moved weekly planning
END:VEVENT
BEGIN:VEVENT
UID:weekly@example.com
RECURRENCE-ID:20260910T090000Z
DTSTART:20260910T090000Z
SUMMARY:Reminder
END:VEVENT
END:VCALENDAR
`

func TestCalDAVReportRejectsCrossOriginRedirect(t *testing.T) {
	var foreignRequests atomic.Int32
	foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests.Add(1)
	}))
	defer foreign.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", foreign.URL+"/calendar/")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	previousTransport := calDAVHTTPTransport
	calDAVHTTPTransport = server.Client().Transport
	t.Cleanup(func() { calDAVHTTPTransport = previousTransport })
	if _, err := calDAVRequest(t.Context(), "REPORT", server.URL+"/calendar/", "username", "password", "1", "<query/>", time.Second); err == nil {
		t.Fatal("accepted a redirect to a different origin")
	}
	if foreignRequests.Load() != 0 {
		t.Fatal("forwarded a CalDAV request to a different origin")
	}
}

func TestCalDAVCalendarRefreshPreservesCacheAndAccountScope(t *testing.T) {
	for _, reuseMailbox := range []bool{false, true} {
		t.Run(fmt.Sprintf("reuseMailbox=%t", reuseMailbox), func(t *testing.T) {
			var requests, responseMode atomic.Int32
			wantUsername := "calendar-login"
			if reuseMailbox {
				wantUsername = "mailbox-login"
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				username, password, ok := r.BasicAuth()
				if r.Method != "REPORT" || r.URL.Path != "/calendars/main/" || !ok || username != wantUsername || password != "test-password" {
					t.Errorf("wrong CalDAV request or credentials: method=%s path=%s username=%s", r.Method, r.URL.Path, username)
					http.Error(w, "wrong request", http.StatusBadRequest)
					return
				}
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), `<c:expand start=`) || !strings.Contains(string(body), `<c:time-range start=`) || r.Header.Get("Depth") != "1" {
					t.Errorf("missing bounded, expanded CalDAV query: %s", body)
				}
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusMultiStatus)
				switch responseMode.Load() {
				case 1:
					_, _ = fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>/calendars/main/weekly.ics</d:href><d:propstat><d:prop><d:getetag>missing-data</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
				case 2:
					_, _ = fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:"/>`)
				default:
					_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>/calendars/main/weekly.ics</d:href><d:propstat><d:prop><d:getetag>"version-1"</d:getetag><c:calendar-data><![CDATA[%s]]></c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, calDAVExpandedFixture)
				}
			}))
			defer server.Close()
			previousTransport := calDAVHTTPTransport
			calDAVHTTPTransport = server.Client().Transport
			t.Cleanup(func() { calDAVHTTPTransport = previousTransport })
			db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if _, err := db.Write().Exec(`
				INSERT INTO users (id, username, username_normalized) VALUES ('calendar-user', 'calendar-user', 'calendar-user'), ('other-user', 'other-user', 'other-user');
				INSERT INTO accounts (id, user_id, provider, email_address, username) VALUES ('calendar-account', 'calendar-user', 'imap', 'calendar@example.com', 'mailbox-login'), ('other-account', 'other-user', 'imap', 'other@example.com', 'other-login');`); err != nil {
				t.Fatal(err)
			}
			store, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SaveCalDAVConfig(t.Context(), "calendar-user", "calendar-account", server.URL, "calendar-login", "test-password", false); err != nil {
				t.Fatal(err)
			}
			if reuseMailbox {
				if _, err := db.Write().Exec(`UPDATE accounts SET encrypted_password = (SELECT encrypted_password FROM account_caldav_configs WHERE account_id = 'calendar-account') WHERE id = 'calendar-account'`); err != nil {
					t.Fatal(err)
				}
				if err := store.SaveCalDAVConfig(t.Context(), "calendar-user", "calendar-account", server.URL, "", "", true); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.ReplaceCalendarSources(t.Context(), "calendar-user", "calendar-account", "caldav", []storage.CalendarSource{
				{ID: "main-source", RemoteID: server.URL + "/calendars/main/", Name: "Main", IsSelected: true},
				{ID: "unselected-source", RemoteID: server.URL + "/calendars/unselected/", Name: "Unselected"},
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.ReplaceCalendarSources(t.Context(), "other-user", "other-account", "caldav", []storage.CalendarSource{
				{ID: "private-source", RemoteID: server.URL + "/calendars/private/", Name: "Private", IsSelected: true},
			}); err != nil {
				t.Fatal(err)
			}
			h := &Handler{db: db, accountStore: store}
			request := func(method, path string) *http.Request {
				r := httptest.NewRequest(method, path, nil)
				return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "calendar-user", Username: "calendar-user"}))
			}
			page := httptest.NewRecorder()
			h.handleCalendar(page, request(http.MethodGet, "/calendar?month=2026-09"))
			if requests.Load() != 0 || !strings.Contains(page.Body.String(), "data-calendar-auto-sync") || !strings.Contains(page.Body.String(), "Calendar sync pending") || strings.Contains(page.Body.String(), ">Refreshing calendars</span>") {
				t.Fatalf("Calendar did not render its cache and pending status before fetching")
			}
			refresh := func() *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				h.handleCalendarSync(rec, request(http.MethodPost, "/api/calendar/sync?month=2026-09"))
				return rec
			}
			rec := refresh()
			if rec.Code != http.StatusOK || rec.Header().Get("X-Gofer-Status") == "error" || strings.Contains(rec.Body.String(), "data-calendar-auto-sync") || !strings.Contains(rec.Body.String(), "Synced 3 event(s)") {
				t.Fatalf("refresh status=%d header=%s body=%s", rec.Code, rec.Header().Get("X-Gofer-Status"), rec.Body.String())
			}
			start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
			cached, err := db.ListCalendarEvents(t.Context(), "calendar-user", start, start.AddDate(0, 1, 0))
			if err != nil || len(cached) != 3 || requests.Load() != 1 {
				t.Fatalf("cached events=%d requests=%d error=%v", len(cached), requests.Load(), err)
			}
			ids := map[string]bool{}
			for _, event := range cached {
				if ids[event.RemoteID] || event.SourceID != "main-source" || event.ETag != `"version-1"` {
					t.Fatalf("invalid event identity or scope: %#v", event)
				}
				ids[event.RemoteID] = true
			}
			if err := db.FailCalendarSync(t.Context(), "other-user", "main-source", "foreign error"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("foreign source status mutation returned %v", err)
			}
			responseMode.Store(1)
			rec = refresh()
			if rec.Header().Get("X-Gofer-Status") != "error" || !strings.Contains(rec.Body.String(), "Weekly planning") || !strings.Contains(rec.Body.String(), "Previously cached events remain available") {
				t.Fatalf("incomplete refresh did not preserve and display cached events")
			}
			var state, lastError string
			var lastSuccess sql.NullString
			if err := db.Read().QueryRow(`SELECT state, last_error, last_success_at FROM calendar_sync_state WHERE source_id = 'main-source'`).Scan(&state, &lastError, &lastSuccess); err != nil || state != "failed" || lastError == "" || !lastSuccess.Valid {
				t.Fatalf("failed sync state=%s error=%q success=%v query=%v", state, lastError, lastSuccess, err)
			}
			responseMode.Store(2)
			rec = refresh()
			cached, err = db.ListCalendarEvents(t.Context(), "calendar-user", start, start.AddDate(0, 1, 0))
			if rec.Header().Get("X-Gofer-Status") == "error" || err != nil || len(cached) != 0 {
				t.Fatalf("empty successful snapshot did not remove absent events: events=%d error=%v", len(cached), err)
			}
			if _, err := resolveCalDAVHref(server.URL, "https://foreign.example/calendar/"); err == nil {
				t.Fatal("accepted a calendar URL on a different origin")
			}
			invalidMonth := httptest.NewRecorder()
			r := request(http.MethodPost, "/api/calendar/sync")
			r.Body = io.NopCloser(strings.NewReader(url.Values{"month": {"invalid"}}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			h.handleCalendarSync(invalidMonth, r)
			if invalidMonth.Code != http.StatusBadRequest {
				t.Fatalf("invalid month status=%d", invalidMonth.Code)
			}
		})
	}
}
