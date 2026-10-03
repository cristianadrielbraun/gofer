package views

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"time"
)

const calendarWeekHourHeight = 64

// Week view reuses the same cached events, agenda, and sync state as Month.
func NewCalendarWeekData(at time.Time) CalendarMonthData {
	if at.IsZero() {
		at = time.Now()
	}
	data := NewCalendarMonthData(at)
	date := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
	start := date.AddDate(0, 0, -((int(date.Weekday()) + 6) % 7))
	end := start.AddDate(0, 0, 6)
	data.View = "week"
	data.PeriodKey = "week:" + start.Format("2006-01-02")
	data.MonthLabel = calendarWeekRangeLabel(start, end)
	week := CalendarWeek{}
	for index := 0; index < 7; index++ {
		day := start.AddDate(0, 0, index)
		week.Days = append(week.Days, CalendarDay{
			Date: day, DayNumber: day.Day(), ISODate: day.Format("2006-01-02"), InMonth: true,
			IsToday:   sameCalendarDate(day, time.Now().In(at.Location())),
			IsWeekend: day.Weekday() == time.Saturday || day.Weekday() == time.Sunday,
		})
	}
	data.Weeks = []CalendarWeek{week}
	return data
}

func calendarWeekRangeLabel(start, end time.Time) string {
	if start.Year() != end.Year() {
		return start.Format("Jan 2, 2006") + " – " + end.Format("Jan 2, 2006")
	}
	if start.Month() != end.Month() {
		return start.Format("Jan 2") + " – " + end.Format("Jan 2, 2006")
	}
	return start.Format("Jan 2") + "–" + end.Format("2, 2006")
}

func calendarViewURL(view, date string) string {
	if view != "week" {
		if len(date) >= 7 {
			return calendarMonthURL(date[:7])
		}
		return "/calendar"
	}
	return "/calendar?" + url.Values{"view": {"week"}, "date": {date}}.Encode()
}

func calendarNavigationURL(data CalendarMonthData, direction int) string {
	if data.View == "week" {
		date, err := time.ParseInLocation("2006-01-02", data.DateKey, data.Month.Location())
		if err != nil {
			date = data.Weeks[0].Days[0].Date
		}
		date = date.AddDate(0, 0, direction*7)
		return calendarViewURL("week", date.Format("2006-01-02"))
	}
	return calendarMonthURL(data.Month.AddDate(0, direction, 0).Format("2006-01"))
}

func calendarNavigationLabel(data CalendarMonthData, direction string) string {
	if data.View == "week" {
		return direction + " week"
	}
	return direction + " month"
}

func calendarViewSwitchClass(active bool) string {
	classes := "relative z-10 inline-flex h-full min-h-0 min-w-0 items-center justify-center rounded-md px-2 text-xs font-medium transition-colors duration-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
	if active {
		return classes + " text-foreground"
	}
	return classes + " text-muted-foreground hover:text-foreground"
}

func calendarViewIndicatorStyle(view string) string {
	transform := "translateX(0)"
	if view == "week" {
		transform = "translateX(calc(100% + 2px))"
	}
	return "width:calc((100% - 6px)/2);transform:" + transform
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
	TimeLabel   string
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
			label := calendarMinuteLabel(startMinute) + "–" + calendarMinuteLabel(endMinute)
			_, startOffset := dayStart.Zone()
			_, endOffset := dayEnd.Add(-time.Nanosecond).Zone()
			if startOffset != endOffset {
				zone, _ := start.Zone()
				label += " " + zone
			}
			blocks = append(blocks, CalendarTimedBlock{Event: event, StartMinute: startMinute, EndMinute: endMinute, TimeLabel: label})
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

func calendarMinuteLabel(minute float64) string {
	value := int(math.Round(minute))
	return fmt.Sprintf("%02d:%02d", value/60, value%60)
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

func calendarTimedBlockTimeLabel(block CalendarTimedBlock) string {
	if block.Columns > 1 {
		return calendarMinuteLabel(block.StartMinute)
	}
	return block.TimeLabel
}

func calendarHourLabel(hour int) string {
	return fmt.Sprintf("%02d:00", hour)
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
