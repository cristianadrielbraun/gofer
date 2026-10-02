package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleCalendar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	uiSettings := h.db.GetUISettings(ctx, userID)
	accounts, _ := h.db.GetAccounts(ctx, userID)
	month := calendarDataFromRequest(r, uiSettings)
	sources, sourceErr := h.db.ListSelectedCalendarSources(ctx, userID)
	if sourceErr != nil {
		month.SyncError = true
		month.SyncMessage = "Could not load configured calendars."
	} else {
		applyCalendarSyncSummary(&month, sources)
		windowStart, windowEnd := calendarVisibleWindow(month)
		if r.URL.Query().Get("cache") != "1" {
			for _, source := range sources {
				if calendarSourceNeedsRefresh(source, windowStart, windowEnd, time.Now(), h.calendarSyncInterval(ctx, userID)) {
					month.AutoSync = true
					break
				}
			}
		}
	}
	windowStart, windowEnd := calendarVisibleWindow(month)
	if events, eventsErr := h.db.ListCalendarEvents(ctx, userID, windowStart, windowEnd); eventsErr != nil {
		month.SyncError = true
		month.SyncMessage = "Calendar cache failed: " + eventsErr.Error()
	} else {
		month.Events = calendarViewEvents(events)
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.Header.Get("HX-Target") {
		case "main-content":
			_ = views.CalendarPage(month, uiSettings).Render(ctx, w)
			return
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

func (h *Handler) handleCalendarSync(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	uiSettings := h.db.GetUISettings(ctx, userID)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid calendar refresh request", http.StatusBadRequest)
		return
	}
	values := r.URL.Query()
	for _, field := range []struct{ name, layout string }{{"month", "2006-01"}, {"date", "2006-01-02"}} {
		if value := strings.TrimSpace(r.FormValue(field.name)); value != "" {
			if _, err := time.Parse(field.layout, value); err != nil {
				http.Error(w, "invalid calendar "+field.name, http.StatusBadRequest)
				return
			}
			values.Set(field.name, value)
		}
	}
	if value := strings.TrimSpace(r.FormValue("view")); value != "" {
		if value != "month" && value != "week" {
			http.Error(w, "invalid calendar view", http.StatusBadRequest)
			return
		}
		values.Set("view", value)
	}
	r.URL.RawQuery = values.Encode()
	month := calendarDataFromRequest(r, uiSettings)
	windowStart, windowEnd := calendarVisibleWindow(month)
	synced, syncErr := h.syncCalendarWindow(ctx, userID, windowStart, windowEnd)
	sources, sourceErr := h.db.ListSelectedCalendarSources(ctx, userID)
	applyCalendarSyncSummary(&month, sources)
	if syncErr == nil {
		syncErr = sourceErr
	}
	if syncErr != nil {
		month.SyncError = true
		month.SyncMessage = "Calendar refresh failed: " + syncErr.Error()
	} else if month.HasSources {
		month.SyncMessage = fmt.Sprintf("Synced %d event(s) · Updated %s", synced, time.Now().In(month.Month.Location()).Format("15:04"))
	}
	if events, err := h.db.ListCalendarEvents(ctx, userID, windowStart, windowEnd); err != nil {
		month.SyncError = true
		month.SyncMessage = "Could not load cached calendar events."
	} else {
		month.Events = calendarViewEvents(events)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if month.SyncError {
		w.Header().Set("X-Gofer-Status", "error")
	}
	if err := views.CalendarPage(month, uiSettings).Render(ctx, w); err != nil {
		http.Error(w, "failed to render calendar refresh", http.StatusInternalServerError)
	}
}

func calendarVisibleWindow(month views.CalendarMonthData) (time.Time, time.Time) {
	first := month.Weeks[0].Days[0].Date
	lastWeek := month.Weeks[len(month.Weeks)-1]
	last := lastWeek.Days[len(lastWeek.Days)-1].Date
	return first, last.AddDate(0, 0, 1)
}

func calendarSourceNeedsRefresh(source storage.CalendarSource, start, end, now time.Time, interval time.Duration) bool {
	if source.SyncState == "syncing" {
		// A different period still needs a read after the active window finishes.
		return source.WindowStart == nil || source.WindowEnd == nil || source.WindowStart.After(start) || source.WindowEnd.Before(end)
	}
	if source.SyncState == "failed" && source.NextAttemptAt != nil && source.NextAttemptAt.After(now) {
		return false
	}
	return source.LastSuccessAt == nil || source.WindowStart == nil || source.WindowEnd == nil ||
		source.WindowStart.After(start) || source.WindowEnd.Before(end) || now.Sub(*source.LastSuccessAt) >= interval
}

func applyCalendarSyncSummary(month *views.CalendarMonthData, sources []storage.CalendarSource) {
	month.HasSources = len(sources) > 0
	month.AllSourcesHidden = calendarSourcesAllHidden(sources)
	failed := 0
	statuses := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		statuses = append(statuses, calendarSyncPayload(source, false, true))
		if source.SyncState == "syncing" {
			month.Syncing = true
		}
		if source.SyncState == "failed" {
			failed++
		}
		if source.LastSuccessAt == nil {
			month.PendingSources++
		} else if month.LastSyncedAt == nil || source.LastSuccessAt.Before(*month.LastSyncedAt) {
			month.LastSyncedAt = source.LastSuccessAt
		}
	}
	if encoded, err := json.Marshal(statuses); err == nil {
		month.SyncSourcesJSON = string(encoded)
	}
	if failed > 0 {
		month.SyncError = true
		month.SyncMessage = fmt.Sprintf("%d calendar(s) could not refresh. Retrying automatically.", failed)
	}
}

func calendarViewEvents(events []storage.CalendarEvent) []views.CalendarEvent {
	result := make([]views.CalendarEvent, 0, len(events))
	for _, event := range events {
		result = append(result, calendarViewEvent(event))
	}
	return result
}

func calendarSourcesAllHidden(sources []storage.CalendarSource) bool {
	if len(sources) == 0 {
		return false
	}
	for _, source := range sources {
		if !source.IsHidden {
			return false
		}
	}
	return true
}

func calendarViewEvent(event storage.CalendarEvent) views.CalendarEvent {
	return views.CalendarEvent{
		ID:           event.ID,
		SourceID:     event.SourceID,
		SourceHidden: event.SourceHidden,
		SourceName:   event.SourceName,
		SourceColor:  event.SourceColor,
		Summary:      event.Summary,
		Location:     event.Location,
		Status:       event.Status,
		AllDay:       event.AllDay,
		StartDate:    event.StartDate,
		EndDate:      event.EndDate,
		StartAt:      event.StartAt,
		EndAt:        event.EndAt,
	}
}

func calendarMonthFromRequest(r *http.Request, uiSettings map[string]string) time.Time {
	location := viewsCalendarLocation(uiSettings)
	if value := strings.TrimSpace(r.URL.Query().Get("date")); value != "" {
		if date, err := time.ParseInLocation("2006-01-02", value, location); err == nil {
			return date
		}
	}
	if value := strings.TrimSpace(r.URL.Query().Get("month")); value != "" {
		if month, err := time.ParseInLocation("2006-01", value, location); err == nil {
			return month
		}
	}
	return time.Now().In(location)
}

func calendarDataFromRequest(r *http.Request, uiSettings map[string]string) views.CalendarMonthData {
	at := calendarMonthFromRequest(r, uiSettings)
	if r.URL.Query().Get("view") == "week" {
		return views.NewCalendarWeekData(at)
	}
	return views.NewCalendarMonthData(at)
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
