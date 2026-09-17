package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleCalendar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	uiSettings := h.db.GetUISettings(ctx, userID)
	accounts, _ := h.db.GetAccounts(ctx, userID)
	month := views.NewCalendarMonthData(calendarMonthFromRequest(r, uiSettings))

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.Header.Get("HX-Target") {
		case "calendar-main":
			_ = views.CalendarMainPartial(month).Render(ctx, w)
			return
		case "mail-list":
			_ = views.CalendarAppPartial(accounts, month, uiSettings).Render(ctx, w)
			return
		case "app-shell":
			_ = views.CalendarShell(accounts, month, uiSettings).Render(ctx, w)
			return
		}
	}

	if err := views.CalendarLayout(accounts, month, uiSettings).Render(ctx, w); err != nil {
		http.Error(w, "failed to render calendar", http.StatusInternalServerError)
	}
}

func calendarMonthFromRequest(r *http.Request, uiSettings map[string]string) time.Time {
	location := viewsCalendarLocation(uiSettings)
	if value := strings.TrimSpace(r.URL.Query().Get("month")); value != "" {
		if month, err := time.ParseInLocation("2006-01", value, location); err == nil {
			return month
		}
	}
	return time.Now().In(location)
}

func viewsCalendarLocation(uiSettings map[string]string) *time.Location {
	timezone := strings.TrimSpace(uiSettings["timezone"])
	if timezone != "" && timezone != "local" {
		if location, err := time.LoadLocation(timezone); err == nil {
			return location
		}
	}
	return time.Local
}
