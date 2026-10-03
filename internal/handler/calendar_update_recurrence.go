package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	ical "github.com/emersion/go-ical"
)

// Confirmation requires the exact supported repeat settings. Ordinary edits
// and deletion keep the strict single-event gate.
func calendarUpdateSavedRestriction(event calendar.RemoteEvent, draft calendar.EventDraft) string {
	if draft.Recurrence != nil {
		if !calendarRecurrenceMatchesDraft(event.Recurrence, draft) {
			return "The provider did not confirm the requested repeat settings."
		}
		event.Recurrence = nil
	}
	return calendarUpdateRestriction(event)
}

func (remote googleCalendarUpdateEvent) savedRestriction(draft calendar.EventDraft) string {
	if draft.Recurrence != nil {
		raw, _ := json.Marshal(remote.Recurrence)
		if !calendarRecurrenceMatchesDraft(raw, draft) {
			return "Google did not confirm the requested repeat settings."
		}
		remote.Recurrence = nil
	}
	return remote.restriction()
}

func (remote outlookCalendarUpdateEvent) savedRestriction(draft calendar.EventDraft) string {
	if draft.Recurrence != nil {
		if remote.Type != "seriesMaster" || !calendarRecurrenceMatchesDraft(remote.Recurrence, draft) {
			return "Microsoft did not confirm the requested recurring series."
		}
		remote.Type, remote.Recurrence = "singleInstance", nil
	}
	return remote.restriction()
}

func calendarRecurrenceMatchesDraft(raw json.RawMessage, draft calendar.EventDraft) bool {
	if draft.Recurrence == nil {
		return !calendarUpdateHasDetails(raw)
	}
	var lines []string
	if json.Unmarshal(raw, &lines) == nil {
		return len(lines) == 1 && strings.HasPrefix(lines[0], "RRULE:") && calendarRecurrenceRuleMatches(lines[0][6:], draft)
	}
	// Graph may include default/irrelevant pattern fields. Compare the fields
	// that determine this requested pattern/range rather than raw JSON bytes.
	var graph struct {
		Pattern struct {
			Type           string   `json:"type"`
			Interval       int      `json:"interval"`
			DayOfMonth     int      `json:"dayOfMonth"`
			Month          int      `json:"month"`
			DaysOfWeek     []string `json:"daysOfWeek"`
			FirstDayOfWeek string   `json:"firstDayOfWeek"`
		} `json:"pattern"`
		Range struct {
			Type                string `json:"type"`
			StartDate           string `json:"startDate"`
			EndDate             string `json:"endDate"`
			RecurrenceTimeZone  string `json:"recurrenceTimeZone"`
			NumberOfOccurrences int    `json:"numberOfOccurrences"`
		} `json:"range"`
	}
	if json.Unmarshal(raw, &graph) != nil {
		return false
	}
	r, start := draft.Recurrence, calendarDraftStart(draft)
	pattern := graph.Pattern
	if pattern.Interval != r.Interval || graph.Range.StartDate != start.Format("2006-01-02") {
		return false
	}
	if !draft.AllDay && graph.Range.RecurrenceTimeZone != "" {
		actual, err := calendarSeriesLocation(graph.Range.RecurrenceTimeZone)
		if err != nil || actual.String() != draft.TimeZone {
			return false
		}
	}
	switch r.Frequency {
	case "daily":
		if pattern.Type != "daily" {
			return false
		}
	case "weekly":
		if pattern.Type != "weekly" || len(pattern.DaysOfWeek) != 1 || !strings.EqualFold(pattern.DaysOfWeek[0], start.Weekday().String()) || (r.Interval > 1 && !strings.EqualFold(pattern.FirstDayOfWeek, "monday")) {
			return false
		}
	case "monthly":
		if pattern.Type != "absoluteMonthly" || pattern.DayOfMonth != start.Day() {
			return false
		}
	case "yearly":
		if pattern.Type != "absoluteYearly" || pattern.DayOfMonth != start.Day() || pattern.Month != int(start.Month()) {
			return false
		}
	default:
		return false
	}
	if r.Count > 0 {
		return graph.Range.Type == "numbered" && graph.Range.NumberOfOccurrences == r.Count
	}
	if r.Until != "" {
		return graph.Range.Type == "endDate" && graph.Range.EndDate == r.Until
	}
	return graph.Range.Type == "noEnd"
}

func calendarRecurrenceRuleMatches(value string, draft calendar.EventDraft) bool {
	normalize := func(value string) map[string]string {
		parts := make(map[string]string)
		for _, part := range strings.Split(strings.ToUpper(value), ";") {
			key, value, ok := strings.Cut(part, "=")
			if !ok || key == "" || value == "" || parts[key] != "" {
				return nil
			}
			values := strings.Split(value, ",")
			sort.Strings(values)
			parts[key] = strings.Join(values, ",")
		}
		if parts["INTERVAL"] == "" {
			parts["INTERVAL"] = "1"
		}
		if until := parts["UNTIL"]; until != "" {
			date, err := calendarSeriesUntil(until, draft)
			if err != nil {
				return nil
			}
			parts["UNTIL"] = date
		}
		if draft.Recurrence.Frequency == "weekly" && draft.Recurrence.Interval > 1 {
			if parts["WKST"] == "" {
				parts["WKST"] = "MO" // RFC 5545 default.
			}
		} else {
			delete(parts, "WKST") // Week boundaries do not affect these patterns.
		}
		start := calendarDraftStart(draft)
		if parts["FREQ"] == "WEEKLY" && parts["BYDAY"] == "" {
			parts["BYDAY"] = []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}[start.Weekday()]
		}
		if parts["FREQ"] == "YEARLY" && parts["BYMONTH"] == "" {
			parts["BYMONTH"] = fmt.Sprint(int(start.Month()))
		}
		if (parts["FREQ"] == "MONTHLY" || parts["FREQ"] == "YEARLY") && parts["BYMONTHDAY"] == "" && start.Day() <= 28 {
			parts["BYMONTHDAY"] = fmt.Sprint(start.Day())
		}
		return parts
	}
	actual := normalize(value)
	return actual != nil && reflect.DeepEqual(actual, normalize(calendarRecurrenceRule(draft)))
}

func calendarUpdateCalDAVConfirmedEvent(cal *ical.Calendar, headers http.Header, endpoint, etag string, location *time.Location, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
	var recurrence json.RawMessage
	if draft.Recurrence != nil {
		if len(cal.Events()) != 1 {
			return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm one recurring series")
		}
		props := cal.Events()[0].Props
		rules := props["RRULE"]
		if len(rules) != 1 || !calendarRecurrenceRuleMatches(rules[0].Value, draft) {
			return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm the requested repeat settings")
		}
		recurrence, _ = json.Marshal([]string{"RRULE:" + rules[0].Value})
		// Temporarily exclude the confirmed new RRULE from the single-event
		// safety checks. Exceptions, guests, scheduling and online meetings
		// remain forbidden, and the normal sync parser still requires expansion.
		delete(props, "RRULE")
		defer func() { props["RRULE"] = rules }()
	}
	if reason := calendarUpdateCalDAVRestriction(cal, headers); reason != "" {
		return calendar.RemoteEvent{}, fmt.Errorf("CalDAV did not confirm a supported updated event: %s", reason)
	}
	event, err := normalizeCalDAVEvent(cal.Events()[0], endpoint, etag, location)
	if draft.Recurrence != nil {
		event.Recurrence = recurrence
	}
	return event, err
}
