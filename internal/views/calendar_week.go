package views

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

const calendarWeekHourHeight = 64

// The calendar's views, in the order the view menu lists them. All but
// Month lay the days out on an hour grid.
var calendarViews = []string{"day", "3day", "workweek", "week", "month"}

// calendarPhoneViews are the views the view menu offers on phones. There the
// hour grid shows one day at a time with the week as a strip above it, which
// is the Day view, so Week and Work week would look the same.
var calendarPhoneViews = []string{"day", "3day", "month"}

// CalendarViews lists the calendar's views: "day", "3day" (three days from
// the date), "workweek" (Monday to Friday), "week" and "month".
func CalendarViews() []string {
	return append([]string(nil), calendarViews...)
}

// CalendarViewKnown reports whether view is one of CalendarViews.
func CalendarViewKnown(view string) bool {
	return slices.Contains(calendarViews, view)
}

// calendarTimelineView reports whether a view lays its days on an hour grid.
func calendarTimelineView(view string) bool {
	return view != "month" && CalendarViewKnown(view)
}

// NewCalendarData lays out view for the date at, with weeks starting on
// weekStart. An unknown view is Month.
func NewCalendarData(view string, at time.Time, weekStart time.Weekday) CalendarMonthData {
	if calendarTimelineView(view) {
		return NewCalendarTimelineData(view, at, weekStart)
	}
	return NewCalendarMonthData(at, weekStart)
}

// NewCalendarWeekData lays out the week holding at, beginning on weekStart.
func NewCalendarWeekData(at time.Time, weekStart time.Weekday) CalendarMonthData {
	return NewCalendarTimelineData("week", at, weekStart)
}

// NewCalendarTimelineData lays out an hour-grid view around the date at; a
// zero at is now. Week is the week holding at, beginning on weekStart. Work
// week is that week's Monday to Friday. 3 days starts on at. Day carries the
// whole week, for the strip that picks the day, and shows at. Every view
// reuses the same cached events, agenda, and sync state as Month.
func NewCalendarTimelineData(view string, at time.Time, weekStart time.Weekday) CalendarMonthData {
	if at.IsZero() {
		at = time.Now()
	}
	data := NewCalendarMonthData(at, weekStart)
	date := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
	weekFirst := date.AddDate(0, 0, -calendarDaysIntoWeek(date, weekStart))
	var dates []time.Time
	switch view {
	case "3day":
		for index := 0; index < 3; index++ {
			dates = append(dates, date.AddDate(0, 0, index))
		}
	case "workweek":
		for index := 0; index < 7; index++ {
			if day := weekFirst.AddDate(0, 0, index); day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
				dates = append(dates, day)
			}
		}
	default:
		view = map[bool]string{true: "day", false: "week"}[view == "day"]
		for index := 0; index < 7; index++ {
			dates = append(dates, weekFirst.AddDate(0, 0, index))
		}
	}
	data.View = view
	data.PeriodKey = view + ":" + dates[0].Format("2006-01-02")
	week := CalendarWeek{}
	today := time.Now().In(at.Location())
	for _, day := range dates {
		week.Days = append(week.Days, CalendarDay{
			Date: day, DayNumber: day.Day(), ISODate: day.Format("2006-01-02"), InMonth: true,
			IsToday:   sameCalendarDate(day, today),
			IsWeekend: day.Weekday() == time.Saturday || day.Weekday() == time.Sunday,
		})
	}
	data.Weeks = []CalendarWeek{week}
	return data
}

func calendarViewURL(view, date string) string {
	if !calendarTimelineView(view) {
		if len(date) >= 7 {
			return calendarMonthURL(date[:7])
		}
		return "/calendar"
	}
	return "/calendar?" + url.Values{"view": {view}, "date": {date}}.Encode()
}

// calendarViewStep is how many days an arrow moves an hour-grid view.
// The zoom slider's levels for the hour grid, from the whole day on screen to the
// largest hours. Fit is separate and not one of them. app.js uses the same numbers
// in CALENDAR_WEEK_ZOOM_LEVELS and _calendarWeekZoomLevel.
const (
	calendarWeekZoomLevels       = 6
	calendarWeekZoomDefaultLevel = 2
)

func calendarViewStep(view string) int {
	switch view {
	case "day":
		return 1
	case "3day":
		return 3
	}
	return 7
}

func calendarNavigationURL(data CalendarMonthData, direction int) string {
	if calendarTimelineView(data.View) {
		date, err := time.ParseInLocation("2006-01-02", data.DateKey, data.Month.Location())
		if err != nil {
			date = data.Weeks[0].Days[0].Date
		}
		date = date.AddDate(0, 0, direction*calendarViewStep(data.View))
		return calendarViewURL(data.View, date.Format("2006-01-02"))
	}
	return calendarMonthURL(data.Month.AddDate(0, direction, 0).Format("2006-01"))
}

// calendarNavigationLabel names the previous or next arrow; direction is
// "previous" or "next".
func calendarNavigationLabel(ctx context.Context, data CalendarMonthData, direction string) string {
	switch data.View {
	case "day":
		return i18n.T(ctx, "calendar.nav."+direction+"_day")
	case "3day":
		return i18n.T(ctx, "calendar.nav."+direction+"_3days")
	case "week", "workweek":
		return i18n.T(ctx, "calendar.nav."+direction+"_week")
	}
	return i18n.T(ctx, "calendar.nav."+direction+"_month")
}

// calendarViewTriggerClass makes the view menu's trigger look like the Today
// button and the arrows beside it, rather than a form field. It is only as
// wide as the open view's name; its menu grows to fit every name.
const calendarViewTriggerClass = "h-8 w-auto gap-1.5 border-border px-2.5 text-xs font-semibold text-foreground shadow-xs hover:bg-accent md:text-xs dark:border-input " + raisedButtonClass

// calendarViewOnPhones reports whether the view menu offers view on phones.
func calendarViewOnPhones(view string) bool {
	return slices.Contains(calendarPhoneViews, view)
}

// calendarTimelineColumns is how many day columns the hour grid shows: one
// in Day, which carries its week only for the strip.
func calendarTimelineColumns(data CalendarMonthData) int {
	if data.View == "day" {
		return 1
	}
	return len(data.Weeks[0].Days)
}

// calendarTimelineGridStyle gives the hour grid its column counts.
func calendarTimelineGridStyle(data CalendarMonthData) string {
	return fmt.Sprintf("--calendar-columns:%d;--calendar-days:%d", calendarTimelineColumns(data), len(data.Weeks[0].Days))
}

// calendarShownDay is the day Day view shows, and the one a phone shows
// first in Week and Work week: the date asked for, else today, else the
// first day.
func calendarShownDay(data CalendarMonthData) string {
	days := data.Weeks[0].Days
	if data.View == "day" {
		return data.DateKey
	}
	for _, day := range days {
		if day.ISODate == data.TodayDateKey {
			return day.ISODate
		}
	}
	return days[0].ISODate
}

func calendarViewCurrent(active bool) string {
	if active {
		return "page"
	}
	return "false"
}

func calendarViewSyncValues(data CalendarMonthData) string {
	return fmt.Sprintf(`{"month":%q,"view":%q,"date":%q}`, data.MonthKey, data.View, data.DateKey)
}

func calendarAllDayEvents(day CalendarDay, events []CalendarEvent) []CalendarEvent {
	var result []CalendarEvent
	for _, event := range calendarEventsForDay(day, events) {
		if event.AllDay {
			result = append(result, event)
		}
	}
	return result
}

type CalendarTimedBlock struct {
	Event       CalendarEvent
	StartMinute float64
	EndMinute   float64
	Column      int
	Columns     int
	// Zone names the time zone when the day changes offset, so the times
	// say which side of the change they are on.
	Zone string
}

// Split at local midnights and timezone changes. In particular, an event
// crossing the repeated DST hour must not get a negative wall-clock height.
func calendarTimedBlocks(day CalendarDay, events []CalendarEvent) []CalendarTimedBlock {
	dayStart := day.Date
	dayEnd := dayStart.AddDate(0, 0, 1)
	var blocks []CalendarTimedBlock
	for _, event := range calendarEventsForDay(day, events) {
		if event.AllDay || event.StartAt == nil || event.EndAt == nil || event.EndAt.Before(*event.StartAt) {
			continue
		}
		start, end := event.StartAt.In(dayStart.Location()), event.EndAt.In(dayStart.Location())
		if start.Before(dayStart) {
			start = dayStart
		}
		if end.After(dayEnd) {
			end = dayEnd
		}
		for {
			partEnd := end
			_, zoneEnd := start.ZoneBounds()
			if !zoneEnd.IsZero() && zoneEnd.After(start) && zoneEnd.Before(partEnd) {
				partEnd = zoneEnd
			}
			startMinute := calendarWallMinute(start)
			endMinute := startMinute
			if partEnd.After(start) {
				// Read the end in the segment's offset, not the following zone.
				_, offset := start.Zone()
				localEnd := partEnd.In(time.FixedZone("", offset))
				if !sameCalendarDate(localEnd, dayStart) {
					endMinute = 1440
				} else {
					endMinute = calendarWallMinute(localEnd)
				}
			}
			zone := ""
			_, startOffset := dayStart.Zone()
			_, endOffset := dayEnd.Add(-time.Nanosecond).Zone()
			if startOffset != endOffset {
				zone, _ = start.Zone()
			}
			blocks = append(blocks, CalendarTimedBlock{Event: event, StartMinute: startMinute, EndMinute: endMinute, Zone: zone})
			if !partEnd.Before(end) {
				break
			}
			start = partEnd
		}
	}
	sort.SliceStable(blocks, func(i, j int) bool {
		if blocks[i].StartMinute != blocks[j].StartMinute {
			return blocks[i].StartMinute < blocks[j].StartMinute
		}
		if blocks[i].EndMinute != blocks[j].EndMinute {
			return blocks[i].EndMinute > blocks[j].EndMinute
		}
		return blocks[i].Event.ID < blocks[j].Event.ID
	})
	// Connected overlap groups share a column count. Touching endpoints do
	// not overlap; short events reserve enough space to remain clickable.
	for first := 0; first < len(blocks); {
		last := first
		groupEnd := float64(-1)
		var columnEnds []float64
		for last < len(blocks) && (last == first || calendarBlockStart(blocks[last]) < groupEnd) {
			block := &blocks[last]
			start, end := calendarBlockStart(*block), calendarBlockEnd(*block)
			column := 0
			for column < len(columnEnds) && columnEnds[column] > start {
				column++
			}
			if column == len(columnEnds) {
				columnEnds = append(columnEnds, end)
			} else {
				columnEnds[column] = end
			}
			block.Column = column
			groupEnd = math.Max(groupEnd, end)
			last++
		}
		for index := first; index < last; index++ {
			blocks[index].Columns = len(columnEnds)
		}
		first = last
	}
	return blocks
}

func calendarWallMinute(at time.Time) float64 {
	return float64(at.Hour()*60+at.Minute()) + float64(at.Second())/60
}

func calendarMinuteLabel(ctx context.Context, minute float64) string {
	value := int(math.Round(minute))
	return i18n.Dates(ctx).ClockTime(value/60, value%60)
}

func calendarBlockStart(block CalendarTimedBlock) float64 {
	return math.Min(block.StartMinute, 1440-22.5)
}

func calendarBlockEnd(block CalendarTimedBlock) float64 {
	return math.Min(1440, math.Max(block.EndMinute, calendarBlockStart(block)+22.5))
}

func calendarTimedBlockStyle(block CalendarTimedBlock) string {
	start, end := calendarBlockStart(block), calendarBlockEnd(block)
	return fmt.Sprintf("top:%.2fpx;height:%.2fpx;left:calc(%.4f%% + 2px);width:calc(%.4f%% - 4px);", start*calendarWeekHourHeight/60, (end-start)*calendarWeekHourHeight/60, float64(block.Column)*100/float64(block.Columns), 100/float64(block.Columns))
}

func calendarTimedBlockShort(block CalendarTimedBlock) bool {
	return calendarBlockEnd(block)-calendarBlockStart(block) < 45
}

// calendarTimedBlockRange is a block's times, with its zone on days that
// change offset.
func calendarTimedBlockRange(ctx context.Context, block CalendarTimedBlock) string {
	// An event that ends when it starts shows only its time.
	label := calendarMinuteLabel(ctx, block.StartMinute)
	if event := block.Event; event.StartAt == nil || event.EndAt == nil || !event.EndAt.Equal(*event.StartAt) {
		label = i18n.T(ctx, "date.pattern.time_range", "start", label, "end", calendarMinuteLabel(ctx, block.EndMinute))
	}
	if block.Zone != "" {
		label += " " + block.Zone
	}
	return label
}

// calendarTimedBlockTimeLabel is the time shown inside a block: only its
// start when it shares the column with others.
func calendarTimedBlockTimeLabel(ctx context.Context, block CalendarTimedBlock) string {
	if block.Columns > 1 {
		return calendarMinuteLabel(ctx, block.StartMinute)
	}
	return calendarTimedBlockRange(ctx, block)
}

func calendarHourLabel(ctx context.Context, hour int) string {
	return i18n.Dates(ctx).HourLabel(hour)
}

func calendarWeekColumnClass(day CalendarDay) string {
	classes := "relative min-w-0 border-r border-border/70 data-[calendar-day-selected=true]:bg-primary/[0.06]"
	if day.IsToday {
		classes += " bg-primary/[0.035]"
	} else if day.IsWeekend {
		classes += " bg-muted/20"
	}
	return classes
}

// calendarBlockResizable reports whether a movable event lies within this day,
// so its end can be dragged.
func calendarBlockResizable(block CalendarTimedBlock, day CalendarDay) bool {
	event := block.Event
	if !event.Movable || event.StartAt == nil || event.EndAt == nil {
		return false
	}
	return !event.StartAt.Before(day.Date) && !event.EndAt.After(day.Date.AddDate(0, 0, 1))
}
