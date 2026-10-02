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
		if test.status == http.StatusOK && (!strings.Contains(w.Body.String(), `data-calendar-period="week:2026-09-28"`) || !strings.Contains(w.Body.String(), `data-calendar-view="week"`)) {
			t.Fatal("refresh switched away from the requested week")
		}
	}
}
