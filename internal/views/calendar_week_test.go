package views

import (
	"bytes"
	"context"
	"math"
	"slices"
	"strconv"
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
		week := NewCalendarWeekData(at, time.Monday)
		if len(week.Weeks) != 1 || len(week.Weeks[0].Days) != 7 {
			t.Fatal("week must contain exactly seven days")
		}
		days := week.Weeks[0].Days
		if days[0].ISODate != test.first || days[6].ISODate != test.last || calendarPeriodLabel(context.Background(), week) != test.label {
			t.Fatalf("week for %s = %s to %s, label %q", test.date, days[0].ISODate, days[6].ISODate, calendarPeriodLabel(context.Background(), week))
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
			if block.EndMinute < block.StartMinute || block.Zone != "CET" && block.Zone != "CEST" {
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
	data := NewCalendarWeekData(at, time.Monday)
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
		`data-calendar-select-day="2026-09-28"`, `data-calendar-all-day-column="2026-09-28"`, `data-calendar-day-dot`,
		`top:576.00px;height:64.00px`, `9:00 AM–10:00 AM`, `hx-get="/api/calendar/events/Planning"`,
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

func TestCalendarViewMenuLooksLikeAButtonAndLinksEveryView(t *testing.T) {
	for _, view := range CalendarViews() {
		data := NewCalendarData(view, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Monday)
		var out bytes.Buffer
		if err := CalendarMainContent(data).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		html := out.String()
		start := strings.Index(html, `data-calendar-view-nav`)
		if start < 0 {
			t.Fatal("missing view menu")
		}
		menu := html[start:strings.Index(html, `</header>`)]
		for _, want := range []string{`data-calendar-view-select`, `aria-label="Calendar view"`, `value="` + view + `"`, `mobile-sheet`} {
			if !strings.Contains(menu, want) {
				t.Errorf("%s view menu missing %q", view, want)
			}
		}
		// Styled like the Today button beside it, not like a form field.
		trigger := menu[strings.Index(menu, `class="select-trigger`):]
		classes := strings.Fields(trigger[len(`class="`) : strings.Index(trigger[len(`class="`):], `"`)+len(`class="`)])
		for _, want := range append([]string{"h-8", "font-semibold", "text-foreground", "shadow-xs"}, strings.Fields(raisedButtonClass)...) {
			if !slices.Contains(classes, want) {
				t.Errorf("%s view menu trigger lacks %s", view, want)
			}
		}
		if slices.Contains(classes, "bg-input/30") {
			t.Errorf("%s view menu trigger still has the form field background", view)
		}
		if !strings.Contains(menu, `data-tui-selectbox-value="`+view+`" data-tui-selectbox-selected="true"`) && !selectedItem(menu, view) {
			t.Errorf("%s is not the chosen item in its own menu", view)
		}
		// Every view has an item and a link to it, in order; only Day, 3 days and
		// Month are offered on phones.
		at := 0
		for _, other := range CalendarViews() {
			item := strings.Index(menu[at:], `data-tui-selectbox-value="`+other+`"`)
			if item < 0 {
				t.Fatalf("%s menu is missing %s, or has it out of order", view, other)
			}
			at += item
			tag := menu[at : at+strings.Index(menu[at:], ">")]
			phone := other == "day" || other == "3day" || other == "month"
			if !strings.Contains(tag, `data-calendar-view-phone="`+strconv.FormatBool(phone)+`"`) {
				t.Errorf("%s item offered on phones should be %v: %s", other, phone, tag)
			}
			link := `href="` + strings.ReplaceAll(calendarViewURL(other, data.DateKey), "&", "&amp;") + `"`
			if !strings.Contains(menu, link) || !strings.Contains(menu, `data-calendar-view-switch="`+other+`"`) {
				t.Errorf("%s menu has no link to %s", view, other)
			}
		}
	}
}

func TestCalendarResponsiveHeightAndZoomMarkup(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, data := range []CalendarMonthData{NewCalendarMonthData(at, time.Monday), NewCalendarWeekData(at, time.Monday)} {
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
			for _, want := range []string{`data-calendar-week-zoom="fit"`, `data-calendar-week-zoom="in"`, `data-calendar-week-zoom="out"`, `data-calendar-week-zoom-slider`, `aria-label="Week timeline zoom"`, `data-calendar-week-hour-label="23"`, `data-calendar-week-hour="0"`, `data-calendar-week-timeline`, `data-calendar-week-header`} {
				if !strings.Contains(html, want) {
					t.Errorf("missing responsive Week markup %q", want)
				}
			}
		} else if !strings.Contains(html, `grid-template-rows:repeat(5,minmax(0,1fr))`) || !strings.Contains(html, `data-calendar-month-grid`) {
			t.Fatal("Month rows do not stretch to the available height")
		}
	}
}

func TestCalendarMoveAttributesAndResize(t *testing.T) {
	location := time.UTC
	day := CalendarDay{Date: time.Date(2026, 11, 3, 0, 0, 0, 0, location)}
	start, end := day.Date.Add(9*time.Hour), day.Date.Add(10*time.Hour)
	event := CalendarEvent{ID: "event", StartAt: &start, EndAt: &end}
	if len(calendarMoveAttributes(event)) != 0 || calendarBlockResizable(CalendarTimedBlock{Event: event}, day) {
		t.Fatal("an event that can't move is draggable")
	}
	event.Movable, event.Occurrence = true, true
	attributes := calendarMoveAttributes(event)
	if attributes["data-calendar-move"] != "event" || attributes["data-calendar-move-occurrence"] != "true" || attributes["data-calendar-move-start"] != "2026-11-03T09:00:00Z" {
		t.Fatalf("attributes: %v", attributes)
	}
	if !calendarBlockResizable(CalendarTimedBlock{Event: event}, day) {
		t.Fatal("an event within the day can't be resized")
	}
	late := day.Date.Add(26 * time.Hour)
	event.EndAt = &late
	if calendarBlockResizable(CalendarTimedBlock{Event: event}, day) {
		t.Fatal("an event ending the next day can be resized on its first day")
	}
}

// A point event shows only its time, not a range from it to itself.
func TestCalendarWeekPointEventShowsOnlyItsTime(t *testing.T) {
	ctx := context.Background()
	day := CalendarDay{Date: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	blocks := calendarTimedBlocks(day, []CalendarEvent{calendarTestTimedEvent("point", day.Date, 14*60, 14*60)})
	if len(blocks) != 1 {
		t.Fatalf("blocks = %#v", blocks)
	}
	if got, want := calendarTimedBlockRange(ctx, blocks[0]), calendarMinuteLabel(ctx, 14*60); got != want {
		t.Fatalf("point event label = %q, want %q", got, want)
	}
	ranged := calendarTimedBlocks(day, []CalendarEvent{calendarTestTimedEvent("meeting", day.Date, 14*60, 15*60)})
	if got := calendarTimedBlockRange(ctx, ranged[0]); got == calendarMinuteLabel(ctx, 14*60) {
		t.Fatalf("event with a length lost its end: %q", got)
	}
}

func TestCalendarWeekStartsOnTheChosenDay(t *testing.T) {
	for _, test := range []struct {
		weekStart         time.Weekday
		date, first, last string
	}{
		{time.Sunday, "2026-10-02", "2026-09-27", "2026-10-03"},
		// A Sunday opens its own week, not the one that ended on it.
		{time.Sunday, "2026-10-04", "2026-10-04", "2026-10-10"},
		{time.Saturday, "2026-10-02", "2026-09-26", "2026-10-02"},
		{time.Saturday, "2027-01-01", "2026-12-26", "2027-01-01"},
	} {
		at, _ := time.Parse("2006-01-02", test.date)
		week := NewCalendarWeekData(at, test.weekStart)
		days := week.Weeks[0].Days
		if len(days) != 7 || days[0].ISODate != test.first || days[6].ISODate != test.last || days[0].Date.Weekday() != test.weekStart {
			t.Fatalf("%s-first week of %s = %s to %s, want %s to %s", test.weekStart, test.date, days[0].ISODate, days[6].ISODate, test.first, test.last)
		}
		if week.DateKey != test.date || week.PeriodKey != "week:"+test.first {
			t.Fatalf("%s-first week of %s has date %s and period %s", test.weekStart, test.date, week.DateKey, week.PeriodKey)
		}
		var output bytes.Buffer
		if err := CalendarWeekGrid(week).Render(t.Context(), &output); err != nil {
			t.Fatal(err)
		}
		if html := output.String(); strings.Index(html, `data-calendar-timed-column="`+test.first+`"`) < 0 || strings.Index(html, `data-calendar-timed-column="`+test.first+`"`) > strings.Index(html, `data-calendar-timed-column="`+test.last+`"`) {
			t.Fatalf("%s-first week columns do not run %s to %s", test.weekStart, test.first, test.last)
		}
	}
}

func TestCalendarTimelineViewsLayOutTheirDays(t *testing.T) {
	// October 4, 2026 is a Sunday.
	for _, test := range []struct {
		view, date     string
		weekStart      time.Weekday
		first, last    string
		count          int
		period, label  string
		previous, next string
		previousLabel  string
	}{
		{"workweek", "2026-10-04", time.Monday, "2026-09-28", "2026-10-02", 5, "workweek:2026-09-28", "Sep 28 – Oct 2, 2026", "2026-09-27", "2026-10-11", "Previous week"},
		// A Sunday-first week holding a Sunday has its work days after it.
		{"workweek", "2026-10-04", time.Sunday, "2026-10-05", "2026-10-09", 5, "workweek:2026-10-05", "Oct 5–9, 2026", "2026-09-27", "2026-10-11", "Previous week"},
		{"3day", "2026-12-31", time.Monday, "2026-12-31", "2027-01-02", 3, "3day:2026-12-31", "Dec 31, 2026 – Jan 2, 2027", "2026-12-28", "2027-01-03", "Previous 3 days"},
		{"day", "2026-10-01", time.Monday, "2026-09-28", "2026-10-04", 7, "day:2026-09-28", "Thursday, October 1, 2026", "2026-09-30", "2026-10-02", "Previous day"},
		{"day", "2026-10-01", time.Saturday, "2026-09-26", "2026-10-02", 7, "day:2026-09-26", "Thursday, October 1, 2026", "2026-09-30", "2026-10-02", "Previous day"},
	} {
		at, _ := time.Parse("2006-01-02", test.date)
		data := NewCalendarData(test.view, at, test.weekStart)
		days := data.Weeks[0].Days
		if data.View != test.view || len(days) != test.count || days[0].ISODate != test.first || days[len(days)-1].ISODate != test.last {
			t.Fatalf("%s around %s = %s, %d days %s to %s", test.view, test.date, data.View, len(days), days[0].ISODate, days[len(days)-1].ISODate)
		}
		if data.PeriodKey != test.period || data.DateKey != test.date {
			t.Errorf("%s around %s has period %s and date %s", test.view, test.date, data.PeriodKey, data.DateKey)
		}
		ctx := context.Background()
		if label := calendarPeriodLabel(ctx, data); label != test.label {
			t.Errorf("%s around %s is titled %q, want %q", test.view, test.date, label, test.label)
		}
		if previous, next := calendarNavigationURL(data, -1), calendarNavigationURL(data, 1); previous != "/calendar?date="+test.previous+"&view="+test.view || next != "/calendar?date="+test.next+"&view="+test.view {
			t.Errorf("%s around %s moves to %s and %s", test.view, test.date, previous, next)
		}
		if label := calendarNavigationLabel(ctx, data, "previous"); label != test.previousLabel {
			t.Errorf("%s previous arrow is %q", test.view, label)
		}
	}
	if data := NewCalendarData("year", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Monday); data.View != "month" {
		t.Errorf("an unknown view opened %s", data.View)
	}
}

func TestCalendarDayViewShowsOneDayOfItsWeek(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	data := NewCalendarData("day", at, time.Monday)
	data.Events = []CalendarEvent{calendarTestTimedEvent("Lunch", at, 720, 780)}
	var out bytes.Buffer
	if err := CalendarMainContent(data).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		`data-calendar-view="day"`, `--calendar-columns:1;--calendar-days:7`,
		// The whole week is there for the strip; only the day is marked shown.
		`data-calendar-select-day="2026-09-28"`, `data-calendar-select-day="2026-10-04"`,
		`data-calendar-timed-column="2026-10-01" data-calendar-shown-day`, `data-calendar-all-day-column="2026-10-01" data-calendar-shown-day`,
		`aria-label="Next day"`, `href="/calendar?date=2026-10-02&amp;view=day"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("Day view is missing %q", want)
		}
	}
	if count := strings.Count(html, "data-calendar-shown-day"); count != 3 {
		t.Errorf("Day view marks %d nodes as shown, want the day's header, all-day and timed columns", count)
	}
	// A phone's week starts on today when it is in the week, else on the first day.
	week := NewCalendarData("week", at, time.Monday)
	if shown := calendarShownDay(week); shown != "2026-09-28" {
		t.Errorf("a past week shows %s first on phones", shown)
	}
	week = NewCalendarData("week", time.Now(), time.Monday)
	if shown := calendarShownDay(week); shown != week.TodayDateKey {
		t.Errorf("this week shows %s first on phones, not today", shown)
	}
}
