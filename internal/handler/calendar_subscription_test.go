package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// A feed as a holiday or team calendar publishes it: a yearly all-day
// event, a weekly meeting in a Windows time zone with one date moved, and
// one event no app could read.
const calendarFeedTestData = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//Example//Feed//EN\r\n" +
	"X-WR-CALNAME:Garden club\r\n" +
	"X-WR-TIMEZONE:Europe/Berlin\r\n" +
	"X-APPLE-CALENDAR-COLOR:#22C55EFF\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:new-year@example.test\r\n" +
	"DTSTAMP:20260101T000000Z\r\n" +
	"DTSTART;VALUE=DATE:20250101\r\n" +
	"DTEND;VALUE=DATE:20250102\r\n" +
	"RRULE:FREQ=YEARLY\r\n" +
	"SUMMARY:New Year's Day\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:seed-swap@example.test\r\n" +
	"DTSTAMP:20260101T000000Z\r\n" +
	"DTSTART;TZID=W. Europe Standard Time:20261104T190000\r\n" +
	"DTEND;TZID=W. Europe Standard Time:20261104T200000\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=4\r\n" +
	"SUMMARY:Seed swap\r\n" +
	"LOCATION:Community hall\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:seed-swap@example.test\r\n" +
	"DTSTAMP:20260101T000000Z\r\n" +
	"RECURRENCE-ID;TZID=W. Europe Standard Time:20261111T190000\r\n" +
	"DTSTART;TZID=W. Europe Standard Time:20261112T180000\r\n" +
	"DTEND;TZID=W. Europe Standard Time:20261112T190000\r\n" +
	"SUMMARY:Seed swap (moved)\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:broken@example.test\r\n" +
	"DTSTAMP:20260101T000000Z\r\n" +
	"DTSTART:20261120T100000Z\r\n" +
	"DTEND:20261120T090000Z\r\n" +
	"SUMMARY:Ends before it starts\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func TestNormalizeCalendarFeedURL(t *testing.T) {
	for raw, want := range map[string]string{
		"webcal://calendar.example.test/holidays.ics":      "https://calendar.example.test/holidays.ics",
		" WEBCALS://calendar.example.test/a.ics?key=x#top ": "https://calendar.example.test/a.ics?key=x",
		"https://calendar.example.test/feed":               "https://calendar.example.test/feed",
		"HTTP://calendar.example.test/feed":                "http://calendar.example.test/feed",
	} {
		if got, err := normalizeCalendarFeedURL(raw); err != nil || got != want {
			t.Errorf("normalize %q = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "calendar.example.test/feed.ics", "ftp://calendar.example.test/feed.ics", "https:///feed.ics", "file:///etc/passwd"} {
		if got, err := normalizeCalendarFeedURL(raw); err == nil {
			t.Errorf("normalize %q = %q, want an error", raw, got)
		}
	}
}

func TestCalendarFeedEvents(t *testing.T) {
	feed, err := decodeCalendarFeed([]byte("\xef\xbb\xbf" + calendarFeedTestData))
	if err != nil {
		t.Fatal(err)
	}
	if feed.name != "Garden club" || feed.color != "#22c55e" {
		t.Fatalf("feed name %q color %q", feed.name, feed.color)
	}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
	events, err := calendarFeedEvents(feed, start, end, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	byTitle := map[string][]string{}
	for _, event := range events {
		when := event.StartDate
		if event.StartAt != nil {
			when = event.StartAt.UTC().Format(time.RFC3339)
		}
		byTitle[event.Summary] = append(byTitle[event.Summary], when)
	}
	// 19:00 in Berlin is 18:00 UTC in November; the second date moved.
	if got := strings.Join(byTitle["Seed swap"], " "); got != "2026-11-04T18:00:00Z 2026-11-18T18:00:00Z 2026-11-25T18:00:00Z" {
		t.Errorf("weekly dates = %s", got)
	}
	if got := strings.Join(byTitle["Seed swap (moved)"], " "); got != "2026-11-12T17:00:00Z" {
		t.Errorf("moved date = %s", got)
	}
	if got := strings.Join(byTitle["New Year's Day"], " "); got != "2027-01-01" {
		t.Errorf("yearly dates = %s", got)
	}
	if _, ok := byTitle["Ends before it starts"]; ok || len(events) != 5 {
		t.Errorf("events = %v", byTitle)
	}

	if _, err := decodeCalendarFeed([]byte("<!doctype html><title>Sign in</title>")); err != errCalendarFeedNotICS {
		t.Errorf("a web page read as %v", err)
	}
	broken, err := decodeCalendarFeed([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:x\r\nDTSTART:20261120T100000Z\r\nDTEND:20261120T090000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := calendarFeedEvents(broken, start, end, time.UTC); err != errCalendarFeedEvents {
		t.Errorf("a feed of unreadable events read as %v", err)
	}
}

func TestCalendarFeedZoneName(t *testing.T) {
	for zone, want := range map[string]string{
		"Europe/Berlin":                         "Europe/Berlin",
		`"Europe/Berlin"`:                       "Europe/Berlin",
		"W. Europe Standard Time":               "Europe/Berlin",
		"/mozilla.org/20050126_1/Europe/Berlin": "Europe/Berlin",
		"/softwarestudio.org/Olson_20011030_5/America/Argentina/Buenos_Aires": "America/Argentina/Buenos_Aires",
		"Customized Time Zone": "",
	} {
		if got := calendarFeedZoneName(zone); got != want {
			t.Errorf("zone %q = %q, want %q", zone, got, want)
		}
	}
}

// calendarFeedServer serves the test feed at /feed.ics and a web page at
// /page, counting the feed's downloads.
func calendarFeedServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/feed.ics":
			reads.Add(1)
			w.Header().Set("Content-Type", "text/calendar")
			_, _ = w.Write([]byte(calendarFeedTestData))
		case "/page":
			_, _ = w.Write([]byte("<!doctype html><title>Sign in</title>"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &reads
}

func subscriptionTestRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "alice"}))
}

// waitCalendarEvents waits for the subscription's first read, which runs in
// the background, and returns its events' titles.
func waitCalendarEvents(t *testing.T, db *storage.DB, sourceID string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, err := db.Read().Query(`SELECT e.summary FROM calendar_events e JOIN calendar_sync_state s ON s.source_id = e.source_id
			WHERE e.source_id = ? AND e.is_deleted = 0 AND s.state = 'ok' ORDER BY e.summary`, sourceID)
		if err != nil {
			t.Fatal(err)
		}
		var titles []string
		for rows.Next() {
			var title string
			_ = rows.Scan(&title)
			titles = append(titles, title)
		}
		rows.Close()
		if len(titles) > 0 {
			return titles
		}
		if time.Now().After(deadline) {
			t.Fatal("the subscription was never read")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSharedCalendarSubscription(t *testing.T) {
	server, _ := calendarFeedServer(t)
	allowLocalDAVServers(t)
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('alice','alice','alice')`); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}

	// A web page instead of a calendar is refused before anything is saved.
	rec := httptest.NewRecorder()
	h.handleCalendarSubscriptionAdd(rec, subscriptionTestRequest(http.MethodPost, "/api/calendar/subscriptions", `{"url":"`+server.URL+`/page"}`))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "iCalendar") {
		t.Fatalf("web page: %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.handleCalendarSubscriptionAdd(rec, subscriptionTestRequest(http.MethodPost, "/api/calendar/subscriptions", `{"url":"`+server.URL+`/feed.ics","refresh_minutes":360}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("add: %d %q", rec.Code, rec.Body.String())
	}
	sources, err := db.ListSelectedCalendarSources(t.Context(), "alice")
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %+v, %v", sources, err)
	}
	source := sources[0]
	if source.Provider != storage.CalendarSourceProviderICS || source.Name != "Garden club" || source.Color != "#22c55e" || source.RefreshMinutes != 360 || calendarSourceWritable(source) {
		t.Fatalf("subscription = %+v", source)
	}
	if titles := waitCalendarEvents(t, db, source.ID); !strings.Contains(strings.Join(titles, "|"), "Seed swap") {
		t.Fatalf("events = %v", titles)
	}
	// It waits its own interval for the next read.
	sources, _ = db.ListSelectedCalendarSources(t.Context(), "alice")
	if next := sources[0].NextAttemptAt; next == nil || next.Before(time.Now().Add(5*time.Hour)) {
		t.Errorf("next refresh at %v", next)
	}

	rec = httptest.NewRecorder()
	h.handleCalendarSubscriptionAdd(rec, subscriptionTestRequest(http.MethodPost, "/api/calendar/subscriptions", `{"url":"`+server.URL+`/feed.ics"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second add: %d %q", rec.Code, rec.Body.String())
	}

	r := subscriptionTestRequest(http.MethodPatch, "/api/calendar/subscriptions/"+source.ID, `{"url":"`+server.URL+`/feed.ics","name":"Club","color":"#ef4444","refresh_minutes":15}`)
	r.SetPathValue("id", source.ID)
	rec = httptest.NewRecorder()
	h.handleCalendarSubscriptionChange(rec, r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("change: %d %q", rec.Code, rec.Body.String())
	}
	sources, _ = db.ListSelectedCalendarSources(t.Context(), "alice")
	if sources[0].Name != "Club" || sources[0].Color != "#ef4444" || sources[0].RefreshMinutes != 15 {
		t.Fatalf("changed = %+v", sources[0])
	}

	r = subscriptionTestRequest(http.MethodDelete, "/api/calendar/subscriptions/"+source.ID, "")
	r.SetPathValue("id", source.ID)
	rec = httptest.NewRecorder()
	h.handleCalendarSubscriptionChange(rec, r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %q", rec.Code, rec.Body.String())
	}
	var left int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events WHERE source_id = ?`, source.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("events left = %d, %v", left, err)
	}
}

func TestUserCalendarSubscription(t *testing.T) {
	feed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/feed.ics" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/calendar")
		_, _ = w.Write([]byte(calendarFeedTestData))
	})
	f := newUserContactPushFixture(t, "carddav", feed)
	rec := httptest.NewRecorder()
	f.h.handleCalendarSubscriptionAdd(rec, subscriptionTestRequest(http.MethodPost, "/api/calendar/subscriptions", `{"url":"`+f.server.URL+`/feed.ics","name":"Club"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("add: %d %q", rec.Code, rec.Body.String())
	}
	var source storage.CalendarSource
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		sources, err := db.ListSelectedCalendarSources(t.Context(), "alice")
		if err == nil && len(sources) == 1 {
			source = sources[0]
		}
		return err
	}); err != nil || source.ID == "" || source.Name != "Club" || source.RefreshMinutes != storage.CalendarSubscriptionDefaultRefresh {
		t.Fatalf("subscription = %+v, %v", source, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	count, err := f.h.syncUserCalendarWindow(ctx, "alice", start, start.AddDate(0, 3, 0), source.AccountID, false, true)
	if err != nil || count != 5 {
		t.Fatalf("sync = %d, %v", count, err)
	}
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		titles := waitCalendarEvents(t, db, source.ID)
		if !strings.Contains(strings.Join(titles, "|"), "New Year's Day") {
			t.Errorf("events = %v", titles)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Subscriptions have no calendar list to read again.
	if f.h.userAccountHasProviderCalendar(t.Context(), "alice", source.AccountID) {
		t.Error("Local folders counted as an account with a provider calendar")
	}
}
