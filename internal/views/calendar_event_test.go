package views

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestCalendarEventDetailsDateAndTimeZone(t *testing.T) {
	prague, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.October, 24, 23, 30, 0, 0, time.UTC)
	end := start.Add(3 * time.Hour)
	for _, test := range []struct {
		name, date, zone string
		event            CalendarEvent
	}{
		{"single all-day", "Saturday, October 24, 2026", "", CalendarEvent{AllDay: true, StartDate: "2026-10-24", EndDate: "2026-10-25"}},
		{"all-day exclusive end across DST", "Sat, Oct 24, 2026 – Mon, Oct 26, 2026", "", CalendarEvent{AllDay: true, StartDate: "2026-10-24", EndDate: "2026-10-27"}},
		{"timed across DST", "Sunday, October 25, 2026", "Europe/Prague (CEST → CET)", CalendarEvent{StartAt: &start, EndAt: &end}},
		{"instant", "Sunday, October 25, 2026", "Europe/Prague (CEST)", CalendarEvent{StartAt: &start, EndAt: &start}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := calendarEventDetailsDate(test.event, prague); got != test.date {
				t.Fatalf("date = %q, want %q", got, test.date)
			}
			if got := calendarEventDetailsTimeZone(test.event, prague); got != test.zone {
				t.Fatalf("timezone = %q, want %q", got, test.zone)
			}
		})
	}
	crossDayEnd := start.Add(24 * time.Hour)
	if got := calendarEventDetailsDate(CalendarEvent{StartAt: &start, EndAt: &crossDayEnd}, prague); got != "Sun, Oct 25, 2026 – Mon, Oct 26, 2026" {
		t.Fatalf("cross-day date = %q", got)
	}
}

func TestCalendarEventDialogRendersReadOnlyDetails(t *testing.T) {
	start := time.Date(2026, time.October, 3, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	details := CalendarEventDetails{
		Event:       CalendarEvent{ID: "event-id", Summary: "Planning <script>alert(1)</script>", SourceName: "Work", SourceColor: "#4285f4", Location: "Room 2", StartAt: &start, EndAt: &end},
		Description: "First line\n" + strings.Repeat("Complete description. ", 25) + "Last line",
		Organizer:   CalendarEventParticipant{Name: "Organizer", Email: "organizer@example.com"},
		Attendees:   []CalendarEventParticipant{{Name: "Guest", Email: "guest@example.com", ResponseStatus: "accepted", Optional: true}},
	}
	var output bytes.Buffer
	if err := CalendarEventDialog(details, time.UTC).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, expected := range []string{
		`id="calendar-event-details-dialog"`, `aria-labelledby="calendar-event-details-title"`,
		`Planning &lt;script&gt;alert(1)&lt;/script&gt;`, `Saturday, October 3, 2026`, `09:00–10:00`,
		`Work`, `Room 2`, `organizer@example.com`, `guest@example.com`, `Accepted`, `Optional`, `Last line`,
		`overflow-y-auto`, `w-[calc(100vw_-_2rem)]`, `Read-only · Cached event`, `data-tui-dialog-close`,
	} {
		if !strings.Contains(html, expected) {
			t.Errorf("dialog missing %q", expected)
		}
	}
	if strings.Contains(html, "<script>") || strings.Contains(html, "Save") || strings.Contains(html, "border-l-2") {
		t.Fatal("dialog includes unsafe HTML, editing actions, or a colored left border")
	}

	output.Reset()
	if err := CalendarEventDialog(CalendarEventDetails{Event: CalendarEvent{AllDay: true, StartDate: "2026-10-03", EndDate: "2026-10-04"}}, time.UTC).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	for _, unexpected := range []string{"Location", "Organizer", "Description", "Guests (", "UTC"} {
		if strings.Contains(output.String(), unexpected) {
			t.Errorf("empty detail still shows %q", unexpected)
		}
	}
	if !strings.Contains(output.String(), "Untitled event") || !strings.Contains(output.String(), "All day") {
		t.Fatal("missing untitled or all-day fallback")
	}
}

func TestCalendarEventsHaveSharedAccessibleDetailsTriggers(t *testing.T) {
	tomorrow := time.Now().UTC().AddDate(0, 0, 1)
	start := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	month := NewCalendarMonthData(start)
	month.Events = []CalendarEvent{{ID: "event-id", Summary: "Planning", StartAt: &start, EndAt: &end}}
	var output bytes.Buffer
	if err := CalendarPage(month, nil).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	for _, attribute := range []string{`hx-get="/api/calendar/events/event-id"`, `hx-target="#app-pane-dialogs"`, `aria-haspopup="dialog"`, `data-calendar-event-trigger`} {
		if got := strings.Count(output.String(), attribute); got != 2 {
			t.Errorf("attribute %q count = %d, want grid and agenda triggers", attribute, got)
		}
	}
}
