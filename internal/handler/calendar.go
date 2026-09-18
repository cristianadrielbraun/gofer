package handler

import (
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
	monthTime := calendarMonthFromRequest(r, uiSettings)
	windowEnd := monthTime.AddDate(0, 1, 0)
	month := views.NewCalendarMonthData(monthTime)
	googleSynced, googleSyncErr := h.syncGoogleCalendarWindow(ctx, userID, monthTime, windowEnd)
	outlookSynced, outlookSyncErr := h.syncOutlookCalendarWindow(ctx, userID, monthTime, windowEnd)
	synced := googleSynced + outlookSynced
	var syncErr error
	if googleSyncErr != nil {
		syncErr = googleSyncErr
	} else if outlookSyncErr != nil {
		syncErr = outlookSyncErr
	}
	if syncErr != nil {
		month.SyncError = true
		month.SyncMessage = "Calendar sync failed: " + syncErr.Error()
	} else {
		month.SyncMessage = fmt.Sprintf("Synced %d event(s)", synced)
	}
	if events, eventsErr := h.db.ListCalendarEvents(ctx, userID, monthTime, windowEnd); eventsErr != nil {
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

func calendarViewEvents(events []storage.CalendarEvent) []views.CalendarEvent {
	result := make([]views.CalendarEvent, 0, len(events))
	for _, event := range events {
		result = append(result, views.CalendarEvent{
			ID:          event.ID,
			SourceName:  event.SourceName,
			SourceColor: event.SourceColor,
			Summary:     event.Summary,
			Location:    event.Location,
			Status:      event.Status,
			AllDay:      event.AllDay,
			StartDate:   event.StartDate,
			EndDate:     event.EndDate,
			StartAt:     event.StartAt,
			EndAt:       event.EndAt,
		})
	}
	return result
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
