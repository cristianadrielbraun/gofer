package views

import (
	"sort"
	"strconv"

	"github.com/a-h/templ"
)

// mailSidebarURL is the calendar cache's month around date, drawn for the
// calendar sidebar in Mail.
func mailSidebarURL(date string) string {
	return "/calendar?view=month&date=" + date + "&cache=1"
}

func mailSidebarSelectedDay(month CalendarMonthData) CalendarDay {
	for _, week := range month.Weeks {
		for _, day := range week.Days {
			if day.ISODate == month.DateKey {
				return day
			}
		}
	}
	return CalendarDay{
		Date:      month.Month,
		ISODate:   month.Month.Format("2006-01-02"),
		DayNumber: 1,
		InMonth:   true,
	}
}

func mailSidebarDayEvents(day CalendarDay, events []CalendarEvent) []CalendarEvent {
	found := calendarEventsForDay(day, events)
	result := make([]CalendarEvent, 0, len(found))

	for _, event := range found {
		if !event.SourceHidden {
			result = append(result, event)
		}
	}

	sort.SliceStable(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.AllDay != b.AllDay {
			return a.AllDay
		}
		if a.StartAt == nil || b.StartAt == nil {
			return a.StartAt != nil
		}
		return a.StartAt.Before(*b.StartAt)
	})

	return result
}

// mailSidebarLinkAttributes load date's month into the calendar sidebar
// without leaving Mail. mail-calendar-sidebar.js animates the change.
func mailSidebarLinkAttributes(date string) templ.Attributes {
	return templ.Attributes{
		"hx-get":    mailSidebarURL(date),
		"hx-target": "#mail-calendar-sidebar-content",
		"hx-swap":   "outerHTML",
		// A newer pick replaces one still loading.
		"hx-sync": "#mail-calendar-sidebar-content:replace",
	}
}

// mailSidebarArrowAttributes is one of the previous and next month arrows.
func mailSidebarArrowAttributes(date, label string) templ.Attributes {
	attributes := mailSidebarLinkAttributes(date)
	attributes["href"] = mailSidebarURL(date)
	attributes["aria-label"] = label
	return attributes
}

// mailSidebarDayClass draws a day like the Calendar page does: today is
// circled in the theme's primary color and the picked day is filled with it.
func mailSidebarDayClass(day CalendarDay, selected string) string {
	classes := "relative flex h-9 flex-col items-center justify-center gap-0.5 rounded-md text-xs font-semibold tabular-nums transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
	switch {
	case day.ISODate == selected:
		return classes + " bg-primary text-primary-foreground shadow-sm"
	case day.IsToday:
		return classes + " text-primary ring-1 ring-inset ring-primary hover:bg-accent"
	case !day.InMonth:
		return classes + " text-muted-foreground/45 hover:bg-accent"
	default:
		return classes + " text-foreground hover:bg-accent"
	}
}

// mailSidebarDayCurrent marks today for screen readers.
func mailSidebarDayCurrent(day CalendarDay) string {
	if day.IsToday {
		return "date"
	}
	return "false"
}

// Bounds of the calendar sidebar's width, as its resize grip allows.
const (
	mailCalendarMinWidth     = 260
	mailCalendarMaxWidth     = 450
	mailCalendarDefaultWidth = 300
)

// mailCalendarWidthStyle draws the calendar sidebar at its saved width. A
// missing or unreadable width is the default; one outside the grip's bounds
// is brought inside them.
func mailCalendarWidthStyle(uiSettings map[string]string) templ.SafeCSS {
	width, err := strconv.Atoi(uiSettings["mail_calendar_width"])
	if err != nil {
		width = mailCalendarDefaultWidth
	}
	width = max(mailCalendarMinWidth, min(mailCalendarMaxWidth, width))
	return templ.SafeCSS("--mail-calendar-width: " + strconv.Itoa(width) + "px;")
}
