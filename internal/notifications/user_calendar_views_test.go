package notifications

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func seedUserCalendarViews(t *testing.T, f *userStorageFixture) {
	t.Helper()
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			if err := db.ReplaceCalendarSources(t.Context(), owner, f.accounts[owner].ID, "caldav", []storage.CalendarSource{
				{ID: "same-calendar-id", RemoteID: "/primary/", Name: owner + " private calendar", IsSelected: true, IsPrimary: true, AccessRole: "owner"},
				{ID: "second-calendar-id", RemoteID: "/second/", Name: owner + " secondary calendar", AccessRole: "owner"},
			}); err != nil {
				return err
			}
			start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			return db.ReplaceCalendarEvents(t.Context(), owner, "same-calendar-id", []storage.CalendarEvent{
				{ID: "same-event-id", RemoteID: "remote-event", ICalUID: "same-uid", Summary: owner + " private appointment", AllDay: true, StartDate: "2026-10-15", EndDate: "2026-10-16"},
			}, start, start.AddDate(0, 1, 0))
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUserCalendarViewsHTTPCachedOwnerIsolationAndFragments(t *testing.T) {
	f := newUserStorageFixture(t)
	seedUserCalendarViews(t, f)
	for _, owner := range []string{"alice", "bob"} {
		other := "bob"
		if owner == "bob" {
			other = "alice"
		}
		for _, path := range []string{"/calendar?month=2026-10&cache=1", "/calendar?date=2026-10-15&view=week&cache=1"} {
			for _, target := range []string{"", "main-content", "calendar-main", "mail-list", "app-shell"} {
				request := httptest.NewRequest("GET", path, nil)
				request.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions[owner].Token})
				if target != "" {
					request.Header.Set("HX-Request", "true")
					request.Header.Set("HX-Target", target)
				}
				response := httptest.NewRecorder()
				f.http.ServeHTTP(response, request)
				if response.Code != 200 || !strings.Contains(response.Body.String(), owner+" private appointment") || strings.Contains(response.Body.String(), other+" private") || strings.Contains(response.Body.String(), other+"@example.com") {
					t.Fatal("calendar owner/fragment", owner, path, target, response.Code, response.Body.String())
				}
			}
		}
		response := f.request(owner, "GET", "/api/calendar/guest-suggestions?guests=private", "")
		if response.Code != 200 || !strings.Contains(response.Body.String(), owner+"-contact@example.com") || strings.Contains(response.Body.String(), other+"-contact@example.com") || response.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("calendar guest ownership", owner, response.Code, response.Body.String())
		}
		if response := f.request(owner, "GET", "/api/calendar/guest-suggestions?guests=x", ""); response.Code != 200 || response.Body.Len() != 0 {
			t.Fatal("short guest query", response.Code, response.Body.String())
		}
	}
	for _, table := range []string{"calendar_sources", "calendar_events"} {
		var count int
		if err := f.system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("calendar central fallback", table, count, err)
		}
	}
}

func TestUserCalendarViewsHTTPRenderReleasesDatabaseLease(t *testing.T) {
	for _, path := range []string{"/calendar?month=2026-10&cache=1", "/api/calendar/guest-suggestions?guests=private"} {
		t.Run(path, func(t *testing.T) {
			f := newUserStorageFixture(t)
			seedUserCalendarViews(t, f)
			writer := &blockedContactViewWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
			var once sync.Once
			release := func() { once.Do(func() { close(writer.release) }) }
			defer release()
			request := httptest.NewRequest("GET", path, nil).WithContext(t.Context())
			request.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
			done := make(chan struct{})
			go func() { f.http.ServeHTTP(writer, request); close(done) }()
			select {
			case <-writer.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("calendar render did not start")
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := f.routing.WithUser(ctx, "bob", func(db *storage.DB) error {
				_, err := db.GetCalendarEvent(ctx, "bob", "same-event-id")
				return err
			}); err != nil {
				t.Fatal("render retained sole cache slot", err)
			}
			release()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("calendar render did not finish")
			}
			if writer.Code != 200 || !strings.Contains(writer.Body.String(), "alice") || strings.Contains(writer.Body.String(), "bob private") || strings.Contains(writer.Body.String(), "bob-contact@example.com") {
				t.Fatal("copied calendar render changed", writer.Code, writer.Body.String())
			}
		})
	}
}
