package views

import "sort"

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

func mailSidebarDayStyle(day CalendarDay, selected string) string {
	if day.ISODate == selected {
		return "background: var(--primary); color: var(--primary-foreground);"
	}
	if day.IsToday {
		return "outline: 1px solid var(--primary);"
	}
	if !day.InMonth {
		return "opacity: .45;"
	}
	return ""
}
