package handler

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Call only while holding the source write/sync gate and after recording the
// confirmed provider write. Failed reads must not make a saved series retryable.
func (h *Handler) refreshCalendarSeries(ctx context.Context, source storage.CalendarSource, draft calendar.EventDraft) bool {
	refreshCtx, finish := context.WithTimeout(ctx, 20*time.Second)
	defer finish()
	start, end := calendarBackgroundWindow(calendarDraftStart(draft), viewsCalendarLocation(h.db.GetUISettings(ctx, source.UserID)))
	_, err := h.performCalendarSourceSync(refreshCtx, source, start, end, make(map[string]calendarCredentials))
	return err == nil
}

func calendarDraftStart(draft calendar.EventDraft) time.Time {
	if draft.AllDay {
		start, _ := time.Parse("2006-01-02", draft.StartDate)
		return start
	}
	zone, _ := time.LoadLocation(draft.TimeZone)
	return draft.StartAt.In(zone)
}

func parseCalendarRecurrenceDraft(r *http.Request, draft calendar.EventDraft) (calendar.EventDraft, error) {
	// Read only body fields. A query parameter must not introduce recurrence.
	form := r.PostForm
	frequency := form.Get("repeat_frequency")
	if frequency == "" || frequency == "none" {
		for _, name := range []string{"repeat_interval", "repeat_end", "repeat_until", "repeat_count"} {
			if form.Get(name) != "" {
				return draft, fmt.Errorf("choose a repeat frequency before configuring a series")
			}
		}
		return draft, nil
	}
	switch frequency {
	case "daily", "weekly", "monthly", "yearly":
	default:
		return draft, fmt.Errorf("choose a valid repeat frequency")
	}
	interval, err := strconv.Atoi(form.Get("repeat_interval"))
	if err != nil || interval < 1 || interval > 99 {
		return draft, fmt.Errorf("repeat every 1 to 99 days, weeks, months, or years")
	}
	repeat := &calendar.RecurrenceDraft{Frequency: frequency, Interval: interval}
	start := calendarDraftStart(draft)
	switch form.Get("repeat_end") {
	case "never":
		if form.Get("repeat_until") != "" || form.Get("repeat_count") != "" {
			return draft, fmt.Errorf("a series without an end cannot have an end date or occurrence count")
		}
	case "until":
		until, err := time.Parse("2006-01-02", form.Get("repeat_until"))
		if err != nil || until.Year() < 1 || form.Get("repeat_until") < start.Format("2006-01-02") || form.Get("repeat_count") != "" {
			return draft, fmt.Errorf("the repeat end date must be on or after the event's start date")
		}
		repeat.Until = form.Get("repeat_until")
		if !draft.AllDay {
			local, _ := time.ParseInLocation("2006-01-02", repeat.Until, start.Location())
			if local.AddDate(0, 0, 1).Add(-time.Second).UTC().Year() > 9999 {
				return draft, fmt.Errorf("choose a repeat end date within the supported date range")
			}
		}
	case "count":
		count, err := strconv.Atoi(form.Get("repeat_count"))
		if err != nil || count < 1 || count > 999 || form.Get("repeat_until") != "" {
			return draft, fmt.Errorf("choose 1 to 999 occurrences, including the first event")
		}
		repeat.Count = count
	default:
		return draft, fmt.Errorf("choose when the series ends")
	}
	if repeat.Count > 0 && calendarRecurrenceDate(start, *repeat, repeat.Count-1).Year() > 9999 {
		return draft, fmt.Errorf("the series extends beyond the supported date range; reduce its interval or count")
	}
	draft.Recurrence = repeat
	return draft, nil
}

// Monthly/yearly patterns use the same date, falling back to the last day in a
// shorter month (including February 29). Keep RRULE and Graph semantics aligned.
func calendarRecurrenceDate(start time.Time, repeat calendar.RecurrenceDraft, index int) time.Time {
	n := repeat.Interval * index
	switch repeat.Frequency {
	case "daily":
		return start.AddDate(0, 0, n)
	case "weekly":
		return start.AddDate(0, 0, 7*n)
	case "yearly":
		n *= 12
	}
	month := time.Date(start.Year(), start.Month()+time.Month(n), 1, start.Hour(), start.Minute(), start.Second(), 0, start.Location())
	day := min(start.Day(), time.Date(month.Year(), month.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day())
	return month.AddDate(0, 0, day-1)
}

// Google Calendar and CalDAV use RFC 5545 rules. Timed UNTIL is UTC, while
// all-day UNTIL remains a DATE. End dates include the entire selected local day.
// https://developers.google.com/workspace/calendar/api/concepts/events-calendars
func calendarRecurrenceRule(draft calendar.EventDraft) string {
	r := draft.Recurrence
	start := calendarDraftStart(draft)
	rule := fmt.Sprintf("FREQ=%s;INTERVAL=%d", strings.ToUpper(r.Frequency), r.Interval)
	switch r.Frequency {
	case "weekly":
		rule += ";BYDAY=" + []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}[start.Weekday()] + ";WKST=MO"
	case "monthly", "yearly":
		if r.Frequency == "yearly" {
			rule += fmt.Sprintf(";BYMONTH=%d", start.Month())
		}
		if start.Day() > 28 {
			var days []string
			for day := 28; day <= start.Day(); day++ {
				days = append(days, strconv.Itoa(day))
			}
			rule += ";BYMONTHDAY=" + strings.Join(days, ",") + ";BYSETPOS=-1"
		} else {
			rule += fmt.Sprintf(";BYMONTHDAY=%d", start.Day())
		}
	}
	if r.Count > 0 {
		rule += fmt.Sprintf(";COUNT=%d", r.Count)
	} else if r.Until != "" {
		until, _ := time.ParseInLocation("2006-01-02", r.Until, start.Location())
		if draft.AllDay {
			rule += ";UNTIL=" + until.Format("20060102")
		} else {
			rule += ";UNTIL=" + until.AddDate(0, 0, 1).Add(-time.Second).UTC().Format("20060102T150405Z")
		}
	}
	return rule
}

// Graph accepts a pattern plus an inclusive, local-date range.
// https://learn.microsoft.com/en-us/graph/api/resources/recurrencepattern
// https://learn.microsoft.com/en-us/graph/api/resources/recurrencerange
func calendarOutlookRecurrence(draft calendar.EventDraft) map[string]any {
	r, start := draft.Recurrence, calendarDraftStart(draft)
	pattern := map[string]any{"type": r.Frequency, "interval": r.Interval}
	switch r.Frequency {
	case "weekly":
		pattern["daysOfWeek"] = []string{strings.ToLower(start.Weekday().String())}
		pattern["firstDayOfWeek"] = "monday"
	case "monthly":
		pattern["type"], pattern["dayOfMonth"] = "absoluteMonthly", start.Day()
	case "yearly":
		pattern["type"], pattern["dayOfMonth"], pattern["month"] = "absoluteYearly", start.Day(), int(start.Month())
	}
	rangeValue := map[string]any{"type": "noEnd", "startDate": start.Format("2006-01-02"), "recurrenceTimeZone": draft.TimeZone}
	if r.Count > 0 {
		rangeValue["type"], rangeValue["numberOfOccurrences"] = "numbered", r.Count
	} else if r.Until != "" {
		rangeValue["type"], rangeValue["endDate"] = "endDate", r.Until
	}
	return map[string]any{"pattern": pattern, "range": rangeValue}
}
