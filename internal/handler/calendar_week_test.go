package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestCalendarWeekRequestUsesExactLocalSevenDayWindow(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/calendar?view=week&date=2026-10-25", nil)
	data := calendarDataFromRequest(r, map[string]string{"timezone": "Europe/Prague"})
	start, end := calendarVisibleWindow(data)
	if data.View != "week" || start.Format(time.RFC3339) != "2026-10-19T00:00:00+02:00" || end.Format(time.RFC3339) != "2026-10-26T00:00:00+01:00" || end.Sub(start) != 169*time.Hour {
		t.Fatalf("incorrect DST week window: %v – %v, %s", start, end, data.View)
	}
}

func TestCalendarRequestStartsWeeksOnTheSavedDay(t *testing.T) {
	// October 25, 2026 is a Sunday: the last day of a Monday-first week and
	// the first of a Sunday-first one.
	for _, test := range []struct{ target, weekStart, start, end string }{
		{"/calendar?view=week&date=2026-10-25", "", "2026-10-19", "2026-10-26"},
		{"/calendar?view=week&date=2026-10-25", "monday", "2026-10-19", "2026-10-26"},
		{"/calendar?view=week&date=2026-10-25", "sunday", "2026-10-25", "2026-11-01"},
		{"/calendar?view=week&date=2026-10-25", "saturday", "2026-10-24", "2026-10-31"},
		{"/calendar?view=week&date=2026-10-25", "someday", "2026-10-19", "2026-10-26"},
		{"/calendar?month=2026-10", "", "2026-09-28", "2026-11-02"},
		{"/calendar?month=2026-10", "sunday", "2026-09-27", "2026-11-01"},
		{"/calendar?month=2026-10", "saturday", "2026-09-26", "2026-11-07"},
		// Day keeps its week up to date, for the strip; 3 days only its own.
		{"/calendar?view=day&date=2026-10-25", "sunday", "2026-10-25", "2026-11-01"},
		{"/calendar?view=3day&date=2026-10-25", "", "2026-10-25", "2026-10-28"},
		{"/calendar?view=workweek&date=2026-10-25", "", "2026-10-19", "2026-10-24"},
	} {
		data := calendarDataFromRequest(httptest.NewRequest(http.MethodGet, test.target, nil), map[string]string{"timezone": "Europe/Prague", "week_start": test.weekStart})
		start, end := calendarVisibleWindow(data)
		if start.Format("2006-01-02") != test.start || end.Format("2006-01-02") != test.end {
			t.Errorf("%s with week_start %q shows %s to %s, want %s to %s", test.target, test.weekStart, start.Format("2006-01-02"), end.Format("2006-01-02"), test.start, test.end)
		}
	}
}

func TestCalendarOpensInSavedViewUnlessLinkNamesOne(t *testing.T) {
	settings := map[string]string{"calendar_view": "week"}
	for target, expected := range map[string]string{
		"/calendar":                           "week",
		"/calendar?month=2026-10":             "month",
		"/calendar?date=2026-10-25":           "month",
		"/calendar?view=month":                "month",
		"/calendar?view=week&date=2026-10-25": "week",
	} {
		if view := calendarDataFromRequest(httptest.NewRequest(http.MethodGet, target, nil), settings).View; view != expected {
			t.Errorf("%s opened %q, want %q", target, view, expected)
		}
	}
	if view := calendarDataFromRequest(httptest.NewRequest(http.MethodGet, "/calendar", nil), map[string]string{"calendar_view": "month"}).View; view != "month" {
		t.Errorf("saved month opened %q", view)
	}
}

func TestHandleCalendarRefreshPreservesWeekView(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "week.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT INTO users (id, username, username_normalized) VALUES ('week-user', 'week-user', 'week-user')`); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}
	for _, test := range []struct {
		view, date string
		status     int
	}{
		{"week", "2026-10-01", http.StatusOK},
		{"week", "invalid", http.StatusBadRequest},
		{"other", "2026-10-01", http.StatusBadRequest},
		{"day", "2026-10-01", http.StatusOK},
		{"3day", "2026-10-01", http.StatusOK},
		{"workweek", "2026-10-01", http.StatusOK},
	} {
		form := url.Values{"view": {test.view}, "date": {test.date}, "month": {"2026-10"}}
		r := httptest.NewRequest(http.MethodPost, "/api/calendar/sync", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "week-user"}))
		w := httptest.NewRecorder()
		h.handleCalendarSync(w, r)
		if w.Code != test.status {
			t.Fatalf("refresh %s %s: status %d, body %s", test.view, test.date, w.Code, w.Body.String())
		}
		period := map[string]string{"week": "week:2026-09-28", "day": "day:2026-09-28", "3day": "3day:2026-10-01", "workweek": "workweek:2026-09-28"}[test.view]
		if test.status == http.StatusOK && (!strings.Contains(w.Body.String(), `data-calendar-period="`+period+`"`) || !strings.Contains(w.Body.String(), `data-calendar-view="`+test.view+`"`)) {
			t.Fatalf("refresh switched away from the requested %s", test.view)
		}
	}
}

// The calendar sidebar in Mail syncs the month it shows through the same
// refresh as the Calendar page, and takes its own view of the result.
func TestHandleCalendarRefreshAnswersTheMailCalendarSidebar(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "sidebar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT INTO users (id, username, username_normalized) VALUES ('sidebar-user', 'sidebar-user', 'sidebar-user')`); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}
	refresh := func(target string) string {
		t.Helper()
		form := url.Values{"view": {"month"}, "date": {"2026-10-14"}, "month": {"2026-10"}}
		r := httptest.NewRequest(http.MethodPost, "/api/calendar/sync", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("HX-Request", "true")
		if target != "" {
			r.Header.Set("HX-Target", target)
		}
		r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "sidebar-user"}))
		w := httptest.NewRecorder()
		h.handleCalendarSync(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("refresh for %q: status %d, body %s", target, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	sidebar := refresh("mail-calendar-sidebar-content")
	if !strings.Contains(sidebar, `id="mail-calendar-sidebar-content"`) || !strings.Contains(sidebar, `data-mail-calendar-month="2026-10"`) {
		t.Fatal("the sidebar should get its own view of the synced month")
	}
	if strings.Contains(sidebar, `id="calendar-main"`) {
		t.Fatal("the sidebar shouldn't get the Calendar page")
	}
	if strings.Contains(sidebar, "data-mail-calendar-auto-sync") {
		t.Fatal("a month just synced shouldn't ask to be synced again")
	}
	if page := refresh("main-content"); !strings.Contains(page, `id="calendar-main"`) {
		t.Fatal("the Calendar page should still get its own view")
	}
}
