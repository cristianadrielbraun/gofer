package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func jmapTestEvents(t *testing.T, raw ...string) []jmap.CalendarEvent {
	t.Helper()
	events := make([]jmap.CalendarEvent, 0, len(raw))
	for _, item := range raw {
		var event jmap.CalendarEvent
		if err := json.Unmarshal([]byte(item), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

// jmapTestSeries is a weekly meeting with one occurrence cancelled and one
// moved, ending on the Monday 30 November occurrence itself.
const jmapTestSeries = `{"id": "s1", "@type": "Event", "uid": "planning@example.test", "title": "Planning",
  "start": "2026-10-26T09:00:00", "timeZone": "Europe/Prague", "duration": "PT1H",
  "recurrenceRule": {"frequency": "weekly", "byDay": [{"day": "mo"}], "until": "2026-11-30T09:00:00"},
  "alerts": {"a": {"@type": "Alert", "trigger": {"@type": "OffsetTrigger", "offset": "-PT10M"}}},
  "recurrenceOverrides": {
    "2026-11-02T09:00:00": {"excluded": true},
    "2026-11-09T09:00:00": {"start": "2026-11-09T11:00:00", "title": "Planning (moved)"}
  }}`

func TestJMAPCalendarEventPageExpandsSeries(t *testing.T) {
	events := jmapTestEvents(t, jmapTestSeries,
		`{"id": "e1", "@type": "Event", "uid": "dentist@example.test", "title": "Dentist", "start": "2026-11-12T07:30:00", "timeZone": "Etc/UTC", "duration": "PT30M"}`,
		`{"id": "e2", "@type": "Event", "uid": "later@example.test", "title": "Next year", "start": "2027-02-01T07:30:00", "timeZone": "Etc/UTC", "duration": "PT30M"}`,
		`{"id": "d1", "@type": "Event", "uid": "trip@example.test", "title": "Trip", "start": "2026-11-20T00:00:00", "showWithoutTime": true, "duration": "P3D"}`)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	page, err := jmapCalendarEventPage(events, start, end, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]calendar.RemoteEvent)
	var ids []string
	for _, event := range page.Events {
		byID[event.RemoteID] = event
		ids = append(ids, event.RemoteID)
	}
	slices.Sort(ids)
	key := func(at string) string { return "s1#recurrence=" + url.QueryEscape(at) }
	want := []string{"d1", "e1", key("2026-10-26T08:00:00Z"), key("2026-11-09T08:00:00Z"), key("2026-11-16T08:00:00Z"), key("2026-11-23T08:00:00Z"), key("2026-11-30T08:00:00Z")}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("events = %v\nwant %v", ids, want)
	}
	moved := byID[key("2026-11-09T08:00:00Z")]
	if moved.Summary != "Planning (moved)" || moved.StartAt.Format(time.RFC3339) != "2026-11-09T10:00:00Z" || moved.EndAt.Format(time.RFC3339) != "2026-11-09T11:00:00Z" || moved.SeriesRemoteID != "s1" {
		t.Errorf("moved occurrence = %+v", moved)
	}
	first := byID[key("2026-10-26T08:00:00Z")]
	if first.Summary != "Planning" || first.StartTimeZone != "Europe/Prague" || first.ReminderMinutes == nil || *first.ReminderMinutes != 10 || first.SeriesRemoteID != "s1" {
		t.Errorf("first occurrence = %+v", first)
	}
	// After the change from summer time the meeting is still at 9:00 there.
	if later := byID[key("2026-11-16T08:00:00Z")]; later.StartAt.Format(time.RFC3339) != "2026-11-16T08:00:00Z" {
		t.Errorf("occurrence after the clock change = %+v", later)
	}
	if trip := byID["d1"]; !trip.AllDay || trip.StartDate != "2026-11-20" || trip.EndDate != "2026-11-23" {
		t.Errorf("all-day event = %+v", trip)
	}
	if first.ETag == "" || first.ETag != moved.ETag || first.ETag == byID["e1"].ETag {
		t.Errorf("etags = %q %q %q", first.ETag, moved.ETag, byID["e1"].ETag)
	}

	// A window that starts during an occurrence still holds it.
	page, err = jmapCalendarEventPage(events, time.Date(2026, 11, 16, 8, 30, 0, 0, time.UTC), time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC), time.UTC)
	if err != nil || len(page.Events) != 1 || page.Events[0].RemoteID != key("2026-11-16T08:00:00Z") {
		t.Fatalf("partial window = %+v, %v", page.Events, err)
	}
}

func TestJMAPCalendarEventPageExpandsAllDaySeries(t *testing.T) {
	events := jmapTestEvents(t, `{"id": "b1", "@type": "Event", "uid": "bins@example.test", "title": "Bins out", "start": "2026-11-03T00:00:00", "showWithoutTime": true,
	  "recurrenceRule": {"frequency": "weekly", "count": 3}}`)
	page, err := jmapCalendarEventPage(events, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	var dates []string
	for _, event := range page.Events {
		if !event.AllDay || event.SeriesRemoteID != "b1" {
			t.Errorf("occurrence = %+v", event)
		}
		dates = append(dates, event.StartDate+"/"+event.EndDate)
	}
	slices.Sort(dates)
	if !slices.Equal(dates, []string{"2026-11-03/2026-11-04", "2026-11-10/2026-11-11", "2026-11-17/2026-11-18"}) {
		t.Errorf("dates = %v", dates)
	}
}

func TestJMAPCalendarSourcesKeepRightsAndDefault(t *testing.T) {
	sources := jmapCalendarSources("owner", "acc", []jmap.Calendar{
		{ID: "c1", Name: "Shared", MyRights: jmap.CalendarRights{MayReadItems: true}},
		{ID: "c2", Name: "Home", IsDefault: true, TimeZone: "Europe/Prague", MyRights: jmap.CalendarRights{MayReadItems: true, MayWriteAll: true}},
		{ID: "c3", Name: "Busy only", MyRights: jmap.CalendarRights{MayReadFreeBusy: true}},
	})
	if len(sources) != 2 || sources[0].AccessRole != "reader" || sources[0].IsSelected || sources[1].AccessRole != "writer" || !sources[1].IsPrimary || !sources[1].IsSelected || sources[1].Provider != "jmap" || sources[1].TimeZone != "Europe/Prague" {
		t.Fatalf("sources = %+v", sources)
	}
	if calendarSourceWritable(sources[1]) {
		t.Error("JMAP calendars are read-only for now")
	}
}

// jmapCalendarsTestServer serves a JMAP account with two calendars to any
// username with the password; events are JSON keyed by id, all in calendar
// "c1", and state is the account's event state.
type jmapCalendarsTestServer struct {
	password string
	mu       sync.Mutex
	state    string
	events   map[string]string
}

func (f *jmapCalendarsTestServer) set(state string, events map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state, f.events = state, events
}

func (f *jmapCalendarsTestServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pass, ok := r.BasicAuth(); !ok || pass != f.password {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/.well-known/jmap" {
			_, _ = w.Write([]byte(`{"capabilities": {"urn:ietf:params:jmap:core": {}, "urn:ietf:params:jmap:mail": {}, "urn:ietf:params:jmap:calendars": {}},
			  "accounts": {"a1": {"name": "lena"}}, "primaryAccounts": {"urn:ietf:params:jmap:mail": "a1", "urn:ietf:params:jmap:calendars": "a1"},
			  "apiUrl": "http://` + r.Host + `/api/", "state": "s"}`))
			return
		}
		var request struct {
			MethodCalls [][]json.RawMessage `json:"methodCalls"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		var name string
		var args struct {
			IDs    []string          `json:"ids"`
			Filter map[string]string `json:"filter"`
		}
		_ = json.Unmarshal(request.MethodCalls[0][0], &name)
		_ = json.Unmarshal(request.MethodCalls[0][1], &args)
		f.mu.Lock()
		defer f.mu.Unlock()
		var answer any
		switch name {
		case "Calendar/get":
			answer = map[string]any{"accountId": "a1", "state": "c", "list": []any{
				map[string]any{"id": "c1", "name": "Personal", "isDefault": true, "timeZone": "Europe/Prague", "myRights": map[string]bool{"mayReadItems": true, "mayWriteAll": true}},
				map[string]any{"id": "c2", "name": "Holidays", "myRights": map[string]bool{"mayReadItems": true}},
			}}
		case "CalendarEvent/get":
			list := []json.RawMessage{}
			for _, id := range args.IDs {
				if event, ok := f.events[id]; ok {
					list = append(list, json.RawMessage(event))
				}
			}
			answer = map[string]any{"accountId": "a1", "state": f.state, "list": list, "notFound": []string{}}
		case "CalendarEvent/query":
			ids := []string{}
			if args.Filter["inCalendar"] == "c1" {
				for id := range f.events {
					ids = append(ids, id)
				}
				slices.Sort(ids)
			}
			answer = map[string]any{"accountId": "a1", "ids": ids, "position": 0, "total": len(ids)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{name, answer, "0"}}, "sessionState": "s"})
	})
}

func TestJMAPCalendarDiscoveryAndSyncForTheSharedStore(t *testing.T) {
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	h, db := newAccountOwnershipTestHandler(t)
	fake := &jmapCalendarsTestServer{password: "saved-secret"}
	fake.set("s1", map[string]string{"s1": jmapTestSeries,
		"e1": `{"id": "e1", "@type": "Event", "uid": "dentist@example.test", "title": "Dentist", "start": "2026-11-12T07:30:00", "timeZone": "Etc/UTC", "duration": "PT30M"}`})
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	address, _ := url.Parse(server.URL)
	host := address.Hostname()
	port, _ := strconv.Atoi(address.Port())
	if err := db.AddPlaintextTransportException(t.Context(), "http", host, port, "administrator"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPrivateTargetException(t.Context(), "http", host, port, "administrator"); err != nil {
		t.Fatal(err)
	}
	account, err := h.accountStore.CreateAccount(t.Context(), "owner", &models.CreateAccountRequest{
		Provider: "jmap", EmailAddress: "lena@example.test", DisplayName: "Lena",
		JMAPSessionURL: server.URL + "/.well-known/jmap", Username: "lena@example.test", Password: "saved-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Calendars the account had over CalDAV give way to the JMAP ones.
	if err := db.ReplaceCalendarSources(t.Context(), "owner", account.ID, storage.CalendarSourceProviderCalDAV, []storage.CalendarSource{{RemoteID: "https://dav.example.test/cal/", Name: "Old", IsPrimary: true, IsSelected: true}}); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"calendar_protocol": {"jmap"}}
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/"+account.ID+"/calendar/discover", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", account.ID)
	rec := httptest.NewRecorder()
	h.handleDiscoverAccountCalendars(rec, ownerRequest(req))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Discovered 2 JMAP calendar(s)") {
		t.Fatalf("discovery = %d %s", rec.Code, rec.Body.String())
	}
	sources, err := db.ListCalendarSourcesForAccount(t.Context(), "owner", account.ID)
	if err != nil || len(sources) != 2 {
		t.Fatalf("sources = %+v, %v", sources, err)
	}
	for _, source := range sources {
		if source.Provider != "jmap" || source.IsSelected != (source.RemoteID == "c1") {
			t.Fatalf("source = %+v", source)
		}
	}

	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	count, err := h.syncCalendarScopedWindow(t.Context(), "owner", start, end, "", account.ID)
	if err != nil || count != 6 {
		t.Fatalf("sync = %d, %v", count, err)
	}
	events, err := db.ListCalendarEvents(t.Context(), "owner", start, end)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, event := range events {
		titles = append(titles, event.Summary)
	}
	slices.Sort(titles)
	if !slices.Equal(titles, []string{"Dentist", "Planning", "Planning", "Planning", "Planning", "Planning (moved)"}) {
		t.Fatalf("titles = %v", titles)
	}

	// The quick check asks the account's event state.
	credentials := map[string]calendarCredentials{}
	selected := sources[0]
	if selected.RemoteID != "c1" {
		selected = sources[1]
	}
	if tag := h.calendarSourceChangeTag(t.Context(), selected, credentials); tag != "jmap:s1" {
		t.Fatalf("change tag = %q", tag)
	}
	changed, err := h.calendarSourceChanged(t.Context(), selected, calendarChangeMark{tag: "jmap:s1", since: time.Now()}, credentials)
	if err != nil || changed {
		t.Fatalf("unchanged calendar = %v, %v", changed, err)
	}
	fake.set("s2", map[string]string{"e1": `{"id": "e1", "@type": "Event", "uid": "dentist@example.test", "title": "Dentist", "start": "2026-11-12T07:30:00", "timeZone": "Etc/UTC", "duration": "PT30M"}`})
	if changed, err := h.calendarSourceChanged(t.Context(), selected, calendarChangeMark{tag: "jmap:s1", since: time.Now()}, map[string]calendarCredentials{}); err != nil || !changed {
		t.Fatalf("changed calendar = %v, %v", changed, err)
	}
	if _, err := h.syncCalendarScopedWindow(t.Context(), "owner", start, end, "", account.ID); err != nil {
		t.Fatal(err)
	}
	if events, _ := db.ListCalendarEvents(t.Context(), "owner", start, end); len(events) != 1 || events[0].Summary != "Dentist" {
		t.Fatalf("events after the series was deleted = %+v", events)
	}
}

// TestJMAPCalendarLiveExpansion saves a series on a real server and reads
// it back as Gofer's sync does; it skips unless GOFER_JMAP_TEST_URL, _USER
// and _PASSWORD are set.
func TestJMAPCalendarLiveExpansion(t *testing.T) {
	sessionURL, user, password := os.Getenv("GOFER_JMAP_TEST_URL"), os.Getenv("GOFER_JMAP_TEST_USER"), os.Getenv("GOFER_JMAP_TEST_PASSWORD")
	if sessionURL == "" || user == "" || password == "" {
		t.Skip("GOFER_JMAP_TEST_URL, GOFER_JMAP_TEST_USER and GOFER_JMAP_TEST_PASSWORD not set")
	}
	ctx := context.Background()
	cfg := &models.AccountConfig{Provider: "jmap", JMAPSessionURL: sessionURL, Username: user, JMAPAllowPlaintext: true, JMAPAllowPrivate: true}
	calendars, err := mail.JMAPCalendars(ctx, cfg, password)
	if err != nil || len(calendars) == 0 {
		t.Fatalf("calendars = %+v, %v", calendars, err)
	}
	sources := jmapCalendarSources("owner", "acc", calendars)
	var source storage.CalendarSource
	for _, candidate := range sources {
		if candidate.IsPrimary {
			source = candidate
		}
	}
	uid := "gofer-live-expansion-" + time.Now().UTC().Format("20060102T150405") + "@example.test"
	ics := strings.Join([]string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//Gofer//test//EN",
		"BEGIN:VEVENT", "UID:" + uid, "DTSTAMP:20261024T100000Z",
		"DTSTART;TZID=Europe/Prague:20261026T090000", "DTEND;TZID=Europe/Prague:20261026T100000",
		"RRULE:FREQ=WEEKLY;BYDAY=MO;UNTIL=20261130T080000Z", "EXDATE;TZID=Europe/Prague:20261102T090000",
		"SUMMARY:Live expansion", "END:VEVENT",
		"BEGIN:VEVENT", "UID:" + uid, "DTSTAMP:20261024T100000Z",
		"RECURRENCE-ID;TZID=Europe/Prague:20261109T090000",
		"DTSTART;TZID=Europe/Prague:20261109T110000", "DTEND;TZID=Europe/Prague:20261109T120000",
		"SUMMARY:Live expansion (moved)", "END:VEVENT",
		"END:VCALENDAR", "",
	}, "\r\n")
	parsed, err := mail.JMAPEventsFromICS(ctx, cfg, password, []byte(ics))
	if err != nil || len(parsed) != 1 {
		t.Fatalf("parse = %+v, %v", parsed, err)
	}
	created, err := mail.CreateJMAPCalendarEvent(ctx, cfg, password, source.RemoteID, parsed[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mail.DeleteJMAPCalendarEvent(ctx, cfg, password, created) })

	page, err := listJMAPCalendarEvents(ctx, source, cfg, password, calendar.EventQuery{WindowStart: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), WindowEnd: time.Date(2026, 12, 10, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, event := range page.Events {
		if event.SeriesRemoteID == created {
			got = append(got, event.StartAt.UTC().Format(time.RFC3339)+" "+event.Summary)
		}
	}
	slices.Sort(got)
	// Stalwart's own expansion lists the moved occurrence twice and leaves
	// out the last; Gofer's has each once.
	want := []string{
		"2026-10-26T08:00:00Z Live expansion",
		"2026-11-09T10:00:00Z Live expansion (moved)",
		"2026-11-16T08:00:00Z Live expansion",
		"2026-11-23T08:00:00Z Live expansion",
		"2026-11-30T08:00:00Z Live expansion",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("occurrences =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
