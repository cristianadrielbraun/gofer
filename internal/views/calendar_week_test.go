package views

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

func TestCalendarWeekBuildsMondayFirstAcrossMonthAndYear(t *testing.T) {
	for _, test := range []struct{ date, first, last, label string }{
		{"2026-10-02", "2026-09-28", "2026-10-04", "Sep 28 – Oct 4, 2026"},
		{"2027-01-01", "2026-12-28", "2027-01-03", "Dec 28, 2026 – Jan 3, 2027"},
		{"2026-10-05", "2026-10-05", "2026-10-11", "Oct 5–11, 2026"},
	} {
		at, _ := time.Parse("2006-01-02", test.date)
		week := NewCalendarWeekData(at)
		if len(week.Weeks) != 1 || len(week.Weeks[0].Days) != 7 {
			t.Fatal("week must contain exactly seven days")
		}
		days := week.Weeks[0].Days
		if days[0].ISODate != test.first || days[6].ISODate != test.last || week.MonthLabel != test.label {
			t.Fatalf("week for %s = %s to %s, label %q", test.date, days[0].ISODate, days[6].ISODate, week.MonthLabel)
		}
		if week.View != "week" || week.DateKey != test.date || week.PeriodKey != "week:"+test.first {
			t.Fatalf("incorrect view state: %#v", week)
		}
		if strings.Contains(calendarNavigationURL(week, 1), "month=") || !strings.Contains(calendarNavigationURL(week, 1), "view=week") {
			t.Fatal("week navigation lost its view")
		}
		if next := calendarNavigationURL(week, 1); !strings.Contains(next, "date="+at.AddDate(0, 0, 7).Format("2006-01-02")) {
			t.Fatalf("week navigation lost the focused weekday: %s", next)
		}
	}
}

func calendarTestTimedEvent(id string, day time.Time, startMinute, endMinute int) CalendarEvent {
	start := day.Add(time.Duration(startMinute) * time.Minute)
	end := day.Add(time.Duration(endMinute) * time.Minute)
	return CalendarEvent{ID: id, Summary: id, StartAt: &start, EndAt: &end}
}

func TestCalendarWeekOverlapsUseSeparateColumns(t *testing.T) {
	day := CalendarDay{Date: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	blocks := calendarTimedBlocks(day, []CalendarEvent{
		calendarTestTimedEvent("a", day.Date, 9*60, 11*60),
		calendarTestTimedEvent("b", day.Date, 9*60+30, 10*60),
		calendarTestTimedEvent("c", day.Date, 10*60, 11*60),
		calendarTestTimedEvent("d", day.Date, 11*60, 12*60),
	})
	if len(blocks) != 4 {
		t.Fatalf("block count = %d", len(blocks))
	}
	if blocks[0].Column != 0 || blocks[1].Column != 1 || blocks[2].Column != 1 || blocks[3].Column != 0 {
		t.Fatalf("incorrect overlap columns: %#v", blocks)
	}
	for _, block := range blocks[:3] {
		if block.Columns != 2 {
			t.Fatalf("overlap group columns = %d", block.Columns)
		}
	}
	if blocks[3].Columns != 1 {
		t.Fatal("touching endpoints should not overlap")
	}
	if style := calendarTimedBlockStyle(blocks[0]); !strings.Contains(style, "top:576.00px;height:128.00px") || !strings.Contains(style, "50.0000%") {
		t.Fatalf("incorrect event geometry: %s", style)
	}
}

func TestCalendarWeekClipsMidnightAndKeepsPointEventsClickable(t *testing.T) {
	day := CalendarDay{Date: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	blocks := calendarTimedBlocks(day, []CalendarEvent{
		calendarTestTimedEvent("overnight", day.Date, -60, 30),
		calendarTestTimedEvent("tomorrow", day.Date, 23*60, 25*60),
		calendarTestTimedEvent("point", day.Date, 1439, 1439),
		calendarTestTimedEvent("ends-at-midnight", day.Date, -60, 0),
		calendarTestTimedEvent("next-day-point", day.Date, 1440, 1440),
	})
	if len(blocks) != 3 || blocks[0].StartMinute != 0 || blocks[0].EndMinute != 30 || blocks[1].EndMinute != 1440 {
		t.Fatalf("incorrect midnight clipping: %#v", blocks)
	}
	for _, block := range blocks {
		if calendarBlockStart(block) < 0 || calendarBlockEnd(block) > 1440 || calendarBlockEnd(block)-calendarBlockStart(block) < 22.5 {
			t.Fatalf("event outside grid or too small to click: %#v", block)
		}
	}
}

func TestCalendarWeekSplitsTimezoneTransitions(t *testing.T) {
	prague, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		start, end string
		minutes    float64
	}{
		{"2026-10-25T02:30:00+02:00", "2026-10-25T02:30:00+01:00", 60},
		{"2026-03-29T01:30:00+01:00", "2026-03-29T03:30:00+02:00", 60},
	} {
		start, _ := time.Parse(time.RFC3339, test.start)
		end, _ := time.Parse(time.RFC3339, test.end)
		local := start.In(prague)
		day := CalendarDay{Date: time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, prague)}
		blocks := calendarTimedBlocks(day, []CalendarEvent{{ID: "dst", StartAt: &start, EndAt: &end}})
		if len(blocks) != 2 {
			t.Fatalf("expected two DST segments, got %#v", blocks)
		}
		var minutes float64
		for _, block := range blocks {
			if block.EndMinute < block.StartMinute || !strings.Contains(block.TimeLabel, "CET") && !strings.Contains(block.TimeLabel, "CEST") {
				t.Fatalf("invalid DST segment: %#v", block)
			}
			minutes += block.EndMinute - block.StartMinute
		}
		if math.Abs(minutes-test.minutes) > 0.01 {
			t.Fatalf("DST event duration = %f, want %f", minutes, test.minutes)
		}
	}
}

func TestCalendarWeekRendersAllDayRowTimelineAndDetails(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	data := NewCalendarWeekData(at)
	data.AutoSync = true
	data.Events = []CalendarEvent{
		calendarTestTimedEvent("Planning", at, 540, 600),
		{ID: "holiday", Summary: "Holiday", AllDay: true, StartDate: "2026-09-30", EndDate: "2026-10-02"},
	}
	var out bytes.Buffer
	if err := CalendarPage(data, nil).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, expected := range []string{
		`data-calendar-view="week"`, `data-calendar-period="week:2026-09-28"`,
		`data-calendar-week-scroll`, `data-calendar-week-grid`, `data-calendar-all-day-row`,
		`data-calendar-timed-column="2026-10-04"`, `aria-label="Next week"`, `aria-label="Previous week"`,
		`data-calendar-view-switch="month"`, `data-calendar-view-switch="week"`,
		`data-calendar-select-day="2026-09-28"`, `Back to week`,
		`top:576.00px;height:64.00px`, `09:00–10:00`, `hx-get="/api/calendar/events/Planning"`,
		`&#34;view&#34;:&#34;week&#34;`, `&#34;date&#34;:&#34;2026-10-01&#34;`,
	} {
		if !strings.Contains(html, expected) {
			t.Errorf("week view missing %q", expected)
		}
	}
	if count := strings.Count(html, "data-calendar-week-all-day"); count != 2 {
		t.Errorf("exclusive all-day range rendered %d cells, want 2", count)
	}
	if strings.Contains(html, "border-l-2") || strings.Contains(html, "border-left-color") {
		t.Fatal("week events reintroduced colored left borders")
	}
}

func TestCalendarViewSwitchMatchesAppTabStructure(t *testing.T) {
	for _, view := range []string{"month", "week"} {
		data := NewCalendarMonthData(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
		data.View = view
		var out bytes.Buffer
		if err := CalendarMainContent(data).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		html := out.String()
		start := strings.Index(html, `<nav aria-label="Calendar view"`)
		if start < 0 {
			t.Fatal("missing view tabs")
		}
		end := strings.Index(html[start:], `</nav>`)
		if end < 0 {
			t.Fatal("missing view tabs")
		}
		nav := html[start : start+end]
		for _, want := range []string{
			`data-calendar-view-nav`, `grid-cols-2 gap-0.5 rounded-lg`, `p-0.5`,
			`data-calendar-view-indicator`, `top-0.5 bottom-0.5 left-0.5 rounded-md`,
			`transition-transform duration-200 ease-out`, `relative z-10 inline-flex h-full min-h-0`,
			`hx-swap="outerHTML"`, `aria-current="page"`, calendarViewIndicatorStyle(view),
		} {
			if !strings.Contains(nav, want) {
				t.Errorf("%s tabs missing %q", view, want)
			}
		}
		if strings.Contains(nav, "bg-primary/10") {
			t.Fatal("active tab must use the shared sliding indicator, not its own background")
		}
	}
}

func TestCalendarResponsiveHeightAndZoomMarkup(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, data := range []CalendarMonthData{NewCalendarMonthData(at), NewCalendarWeekData(at)} {
		var output bytes.Buffer
		if err := CalendarMainContent(data).Render(t.Context(), &output); err != nil {
			t.Fatal(err)
		}
		html := output.String()
		if !strings.Contains(html, "data-calendar-layout-footer") || strings.Contains(html, "min-h-[32rem]") || strings.Contains(html, "height:1536px") {
			t.Fatal("Calendar still has a fixed-height timeline or an unbounded month grid")
		}
		for _, want := range []string{`data-calendar-navigate="-1"`, `data-calendar-navigate="1"`, `data-calendar-surface`} {
			if !strings.Contains(html, want) {
				t.Errorf("missing Calendar navigation animation hook %q", want)
			}
		}
		if data.View == "week" {
			for _, want := range []string{`data-calendar-week-zoom="fit"`, `data-calendar-week-zoom="in"`, `data-calendar-week-zoom="out"`, `data-calendar-week-hour-label="23"`, `data-calendar-week-hour="0"`, `data-calendar-week-timeline`, `data-calendar-week-header`} {
				if !strings.Contains(html, want) {
					t.Errorf("missing responsive Week markup %q", want)
				}
			}
		} else if !strings.Contains(html, `grid-template-rows:repeat(5,minmax(0,1fr))`) || !strings.Contains(html, `data-calendar-month-grid`) {
			t.Fatal("Month rows do not stretch to the available height")
		}
	}
}
