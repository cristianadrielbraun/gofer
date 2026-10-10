package views

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCalendarAgendaHasNoEightEventLimit(t *testing.T) {
	start := time.Now().UTC().AddDate(0, 0, 1)
	month := NewCalendarMonthData(start)
	for index := 0; index < 12; index++ {
		at := start.Add(time.Duration(index) * time.Hour)
		end := at.Add(time.Hour)
		month.Events = append(month.Events, CalendarEvent{ID: fmt.Sprint(index), Summary: "Upcoming", StartAt: &at, EndAt: &end})
	}
	if events := calendarAgendaEvents(month); len(events) != 12 {
		t.Fatalf("upcoming event count = %d, want all 12", len(events))
	}
}

func TestCalendarAgendaListsEveryEventInTheView(t *testing.T) {
	// Today is mid-month; one event is earlier this month and one is on a leading day
	// from the previous month that the grid shows. Both are listed, not just what is
	// still to come.
	today := time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC)
	month := NewCalendarMonthData(today)
	earlier := time.Date(2026, time.October, 2, 9, 0, 0, 0, time.UTC)
	leading := time.Date(2026, time.September, 29, 9, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{earlier, leading} {
		start, end := at, at.Add(time.Hour)
		month.Events = append(month.Events, CalendarEvent{ID: at.Format("0102"), Summary: "Earlier", StartAt: &start, EndAt: &end})
	}
	if got := len(calendarAgendaEvents(month)); got != 2 {
		t.Fatalf("agenda events = %d, want both events in the view", got)
	}
	var output bytes.Buffer
	if err := CalendarPage(month, nil).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	listed := 0
	for _, part := range strings.Split(html, "<button")[1:] {
		tag := part[:strings.Index(part, ">")]
		if !strings.Contains(tag, "data-calendar-agenda-event") {
			continue
		}
		if strings.Contains(" "+tag+" ", " hidden ") {
			t.Fatalf("an event in the view was hidden from the agenda: <button%s>", tag)
		}
		listed++
	}
	if listed != 2 {
		t.Fatalf("agenda rows = %d, want 2", listed)
	}
	if !strings.Contains(html, ">All events</h2>") {
		t.Fatal("agenda heading should name all events in the view")
	}
}

func TestCalendarDaySelectionIncludesPastEventsAndOverflow(t *testing.T) {
	prague, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2020, time.October, 25, 8, 0, 0, 0, prague)
	month := NewCalendarMonthData(start)
	for index := 0; index < 12; index++ {
		at := start.Add(time.Duration(index) * time.Hour)
		end := at.Add(30 * time.Minute)
		month.Events = append(month.Events, CalendarEvent{ID: fmt.Sprintf("past-%d", index), Summary: fmt.Sprintf("Past event %d", index), StartAt: &at, EndAt: &end})
	}
	var output bytes.Buffer
	if err := CalendarPage(month, nil).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, expected := range []string{
		`data-calendar-select-day="2020-10-25"`, `data-calendar-day-overflow`, `+9 more`,
		`data-calendar-day-start="2020-10-25T00:00:00+02:00"`, `data-calendar-day-end="2020-10-26T00:00:00+01:00"`,
		`aria-controls="calendar-agenda-list"`, `aria-pressed="false"`,
		`data-calendar-day-selected="false"`, `data-calendar-agenda-clear`, `data-calendar-day-dot`,
		`id="calendar-agenda-heading"`, `data-calendar-today-date=`, `data-calendar-agenda-empty`,
		`data-calendar-event-start=`, `data-calendar-event-end=`,
	} {
		if !strings.Contains(html, expected) {
			t.Errorf("day selection markup missing %q", expected)
		}
	}
	if count := strings.Count(html, "data-calendar-agenda-event"); count != 12 {
		t.Fatalf("cached agenda rows = %d, want all 12 past events available for selection", count)
	}
	if !strings.Contains(html, "Past event 11") || !strings.Contains(html, `hx-get="/api/calendar/events/past-11"`) {
		t.Fatal("overflow event or its details trigger was discarded")
	}
}

func TestCalendarEventsForDayRespectsLocalBoundaries(t *testing.T) {
	prague, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}
	dayStart := time.Date(2026, time.October, 25, 0, 0, 0, 0, prague)
	dayEnd := dayStart.AddDate(0, 0, 1)
	before := dayStart.Add(-time.Hour)
	after := dayEnd.Add(time.Hour)
	for _, test := range []struct {
		name  string
		event CalendarEvent
		want  bool
	}{
		{"all-day spans selected date", CalendarEvent{AllDay: true, StartDate: "2026-10-24", EndDate: "2026-10-26"}, true},
		{"all-day exclusive end", CalendarEvent{AllDay: true, StartDate: "2026-10-24", EndDate: "2026-10-25"}, false},
		{"cross-midnight", CalendarEvent{StartAt: &before, EndAt: &after}, true},
		{"ends at start of day", CalendarEvent{StartAt: &before, EndAt: &dayStart}, false},
		{"point at day start", CalendarEvent{StartAt: &dayStart, EndAt: &dayStart}, true},
		{"point at next day start", CalendarEvent{StartAt: &dayEnd, EndAt: &dayEnd}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := len(calendarEventsForDay(CalendarDay{Date: dayStart}, []CalendarEvent{test.event})) > 0
			if got != test.want {
				t.Fatalf("selected-day membership = %t, want %t", got, test.want)
			}
		})
	}
}
