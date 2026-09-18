package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
)

func TestDiscoverOutlookCalendarsPaginatesAndNormalizes(t *testing.T) {
	var requests []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer calendar-token" {
			t.Fatalf("authorization = %q, want bearer token", r.Header.Get("Authorization"))
		}
		requests = append(requests, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			if r.URL.Path != "/me/calendars" || r.URL.Query().Get("$top") != "100" {
				t.Fatalf("first calendar request = %s, want /me/calendars with $top=100", r.URL.RequestURI())
			}
			_, _ = fmt.Fprintf(w, `{"value":[{"id":"default","name":"Work","description":"Primary work calendar","timeZone":"Europe/Prague","color":"lightBlue","canEdit":true,"isDefaultCalendar":true}],"@odata.nextLink":%q}`, server.URL+"/me/calendars?page=2")
			return
		}
		if r.URL.Path != "/me/calendars" || r.URL.Query().Get("page") != "2" {
			t.Fatalf("second calendar request = %s, want page 2", r.URL.RequestURI())
		}
		_, _ = fmt.Fprint(w, `{"value":[{"id":"shared","name":"Shared","hexColor":"#12abEF","canEdit":false}]}`)
	}))
	defer server.Close()

	previousBase := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBase })

	calendars, err := discoverOutlookCalendars(t.Context(), "calendar-token")
	if err != nil {
		t.Fatalf("discoverOutlookCalendars() error = %v", err)
	}
	if len(calendars) != 2 {
		t.Fatalf("calendar count = %d, want 2", len(calendars))
	}
	if calendars[0].RemoteID != "default" || !calendars[0].Primary || calendars[0].AccessRole != "owner" || calendars[0].Color != "#5b9bd5" {
		t.Fatalf("primary calendar = %#v", calendars[0])
	}
	if calendars[1].RemoteID != "shared" || calendars[1].Primary || calendars[1].AccessRole != "reader" || calendars[1].Color != "#12abEF" {
		t.Fatalf("shared calendar = %#v", calendars[1])
	}
}

func TestListOutlookCalendarEventsPaginatesAndNormalizes(t *testing.T) {
	var requests []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/calendars/default/calendarView" {
			t.Fatalf("path = %q, want calendar view endpoint", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer calendar-token" {
			t.Fatalf("authorization = %q, want bearer token", r.Header.Get("Authorization"))
		}
		requests = append(requests, r.URL.RequestURI())
		if len(requests) == 1 {
			if r.URL.Query().Get("$orderby") != "start/dateTime" || r.URL.Query().Get("$top") != "1000" {
				t.Fatalf("event query = %v, want ordered paginated query", r.URL.Query())
			}
			if r.URL.Query().Get("startDateTime") == "" || r.URL.Query().Get("endDateTime") == "" {
				t.Fatalf("event query = %v, want a bounded calendar view", r.URL.Query())
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_, _ = fmt.Fprintf(w, `{"value":[{"id":"timed","iCalUId":"timed@example.com","changeKey":"key-1","subject":"Planning","body":{"content":"Discuss roadmap"},"start":{"dateTime":"2026-09-03T09:00:00.0000000","timeZone":"Pacific Standard Time"},"end":{"dateTime":"2026-09-03T10:00:00.0000000","timeZone":"Pacific Standard Time"},"isAllDay":false,"isCancelled":false,"createdDateTime":"2026-09-01T10:00:00Z","lastModifiedDateTime":"2026-09-02T10:00:00Z","organizer":{"emailAddress":{"name":"Person","address":"person@example.com"}},"location":{"displayName":"Room 1"},"attendees":[{"status":"accepted"}],"onlineMeetingUrl":"https://meet.example/timed","seriesMasterId":"series-1"}],"@odata.nextLink":%q}`, server.URL+"/me/calendars/default/calendarView?page=2")
			return
		}
		_, _ = fmt.Fprint(w, `{"value":[{"id":"all-day","subject":"Holiday","start":{"dateTime":"2026-09-04T00:00:00.0000000","timeZone":"UTC"},"end":{"dateTime":"2026-09-05T00:00:00.0000000","timeZone":"UTC"},"isAllDay":true,"isCancelled":false}]}`)
	}))
	defer server.Close()

	previousBase := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBase })

	page, err := listOutlookCalendarEvents(t.Context(), "calendar-token", "default", calendar.EventQuery{
		WindowStart: time.Date(2026, time.September, 1, 0, 0, 0, 0, time.FixedZone("CEST", 2*60*60)),
		WindowEnd:   time.Date(2026, time.October, 1, 0, 0, 0, 0, time.FixedZone("CEST", 2*60*60)),
	})
	if err != nil {
		t.Fatalf("listOutlookCalendarEvents() error = %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("event count = %d, want 2", len(page.Events))
	}
	timed := page.Events[0]
	if timed.StartAt == nil || timed.StartAt.Hour() != 16 || timed.EndAt == nil || timed.Description != "Discuss roadmap" || timed.Location != "Room 1" {
		t.Fatalf("timed event normalization = %#v", timed)
	}
	if timed.OrganizerEmail != "person@example.com" || timed.SeriesRemoteID != "series-1" || string(timed.OnlineMeeting) != `{"url":"https://meet.example/timed"}` {
		t.Fatalf("timed event metadata = %#v", timed)
	}
	allDay := page.Events[1]
	if !allDay.AllDay || allDay.StartDate != "2026-09-04" || allDay.EndDate != "2026-09-05" {
		t.Fatalf("all-day event normalization = %#v", allDay)
	}
	if !strings.Contains(string(timed.Attendees), "accepted") {
		t.Fatalf("attendees = %s, want preserved Graph JSON", timed.Attendees)
	}
}
