package views

import (
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

type CalendarDay struct {
	Date      time.Time
	DayNumber int
	ISODate   string
	InMonth   bool
	IsToday   bool
	IsWeekend bool
}

type CalendarWeek struct {
	Days []CalendarDay
}

type CalendarMonthData struct {
	Month            time.Time
	MonthLabel       string
	MonthKey         string
	PreviousMonthKey string
	NextMonthKey     string
	TodayMonthKey    string
	Weeks            []CalendarWeek
	Events           []CalendarEvent
	SyncMessage      string
	SyncError        bool
}

type CalendarEvent struct {
	ID          string
	SourceName  string
	SourceColor string
	Summary     string
	Location    string
	Status      string
	AllDay      bool
	StartDate   string
	EndDate     string
	StartAt     *time.Time
	EndAt       *time.Time
}

func NewCalendarMonthData(at time.Time) CalendarMonthData {
	if at.IsZero() {
		at = time.Now()
	}
	location := at.Location()
	month := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, location)
	today := time.Now().In(location)
	monthKey := month.Format("2006-01")

	leadingDays := (int(month.Weekday()) + 6) % 7
	daysInMonth := time.Date(month.Year(), month.Month()+1, 0, 0, 0, 0, 0, location).Day()
	cellCount := leadingDays + daysInMonth
	weekCount := (cellCount + 6) / 7
	weeks := make([]CalendarWeek, 0, weekCount)

	for weekIndex := 0; weekIndex < weekCount; weekIndex++ {
		week := CalendarWeek{Days: make([]CalendarDay, 0, 7)}
		for dayIndex := 0; dayIndex < 7; dayIndex++ {
			cellIndex := weekIndex*7 + dayIndex
			date := month.AddDate(0, 0, cellIndex-leadingDays)
			inMonth := date.Month() == month.Month()
			week.Days = append(week.Days, CalendarDay{
				Date:      date,
				DayNumber: date.Day(),
				ISODate:   date.Format("2006-01-02"),
				InMonth:   inMonth,
				IsToday:   sameCalendarDate(date, today),
				IsWeekend: date.Weekday() == time.Saturday || date.Weekday() == time.Sunday,
			})
		}
		weeks = append(weeks, week)
	}

	return CalendarMonthData{
		Month:            month,
		MonthLabel:       month.Format("January 2006"),
		MonthKey:         monthKey,
		PreviousMonthKey: month.AddDate(0, -1, 0).Format("2006-01"),
		NextMonthKey:     month.AddDate(0, 1, 0).Format("2006-01"),
		TodayMonthKey:    today.Format("2006-01"),
		Weeks:            weeks,
	}
}

func sameCalendarDate(left, right time.Time) bool {
	return left.Year() == right.Year() && left.Month() == right.Month() && left.Day() == right.Day()
}

func calendarMonthURL(monthKey string) string {
	values := url.Values{}
	if strings.TrimSpace(monthKey) != "" {
		values.Set("month", strings.TrimSpace(monthKey))
	}
	if query := values.Encode(); query != "" {
		return "/calendar?" + query
	}
	return "/calendar"
}

func calendarWeekdayLabel(index int) string {
	return []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}[index%7]
}

func calendarDayLabel(day CalendarDay) string {
	if !day.InMonth {
		return day.Date.Format("Monday, January 2, 2006") + ", outside this month"
	}
	return day.Date.Format("Monday, January 2, 2006")
}

func calendarDayClass(day CalendarDay) string {
	classes := "relative min-h-24 border-r border-b border-border/70 bg-background p-2 transition-colors sm:min-h-28"
	if !day.InMonth {
		return classes + " bg-muted/20 text-muted-foreground/45"
	}
	if day.IsWeekend {
		classes += " bg-muted/[0.08]"
	}
	if day.IsToday {
		classes += " bg-primary/[0.045]"
	}
	return classes + " hover:bg-accent/35"
}

func calendarDayNumberClass(day CalendarDay) string {
	classes := "inline-flex size-7 items-center justify-center rounded-full text-xs font-semibold tabular-nums"
	if day.IsToday {
		return classes + " bg-primary text-primary-foreground shadow-sm"
	}
	if !day.InMonth {
		return classes + " text-muted-foreground/45"
	}
	return classes + " text-foreground"
}

func calendarEventsForDay(day CalendarDay, events []CalendarEvent) []CalendarEvent {
	dayStart := time.Date(day.Date.Year(), day.Date.Month(), day.Date.Day(), 0, 0, 0, 0, day.Date.Location())
	dayEnd := dayStart.AddDate(0, 0, 1)
	var result []CalendarEvent
	for _, event := range events {
		if event.AllDay {
			startDate := strings.TrimSpace(event.StartDate)
			endDate := strings.TrimSpace(event.EndDate)
			if startDate != "" && endDate != "" && startDate < dayEnd.Format("2006-01-02") && endDate > dayStart.Format("2006-01-02") {
				result = append(result, event)
			}
			continue
		}
		if event.StartAt != nil && event.EndAt != nil && event.StartAt.Before(dayEnd.UTC()) && event.EndAt.After(dayStart.UTC()) {
			result = append(result, event)
		}
	}
	return result
}

func calendarAgendaEvents(month CalendarMonthData) []CalendarEvent {
	now := time.Now().In(month.Month.Location())
	var result []CalendarEvent
	for _, event := range month.Events {
		if event.AllDay {
			if strings.TrimSpace(event.EndDate) != "" && strings.TrimSpace(event.EndDate) <= now.Format("2006-01-02") {
				continue
			}
		} else if event.EndAt != nil && event.EndAt.Before(now.UTC()) {
			continue
		}
		result = append(result, event)
	}
	if len(result) > 8 {
		return result[:8]
	}
	return result
}

func calendarEventSummary(event CalendarEvent) string {
	if summary := strings.TrimSpace(event.Summary); summary != "" {
		return summary
	}
	return "Untitled event"
}

func calendarEventColorStyle(event CalendarEvent) string {
	color := strings.TrimSpace(event.SourceColor)
	if color == "" {
		return "background-color: var(--primary);"
	}
	return "background-color: " + accountColorValue(color) + ";"
}

func calendarEventTimeLabel(event CalendarEvent, location *time.Location) string {
	if event.AllDay {
		return "All day"
	}
	if event.StartAt == nil {
		return ""
	}
	return event.StartAt.In(location).Format("15:04")
}

func calendarEventAgendaDateLabel(event CalendarEvent, location *time.Location) string {
	if event.AllDay {
		if parsed, err := time.ParseInLocation("2006-01-02", event.StartDate, location); err == nil {
			return parsed.Format("Mon, Jan 2")
		}
		return event.StartDate
	}
	if event.StartAt == nil {
		return ""
	}
	return event.StartAt.In(location).Format("Mon, Jan 2")
}

func calendarEventAgendaTimeLabel(event CalendarEvent, location *time.Location) string {
	if event.AllDay {
		return "All day"
	}
	if event.StartAt == nil {
		return ""
	}
	if event.EndAt == nil {
		return event.StartAt.In(location).Format("15:04")
	}
	return event.StartAt.In(location).Format("15:04") + "–" + event.EndAt.In(location).Format("15:04")
}

func calendarEventMeta(event CalendarEvent) string {
	source := strings.TrimSpace(event.SourceName)
	location := strings.TrimSpace(event.Location)
	if source == "" {
		return location
	}
	if location == "" {
		return source
	}
	return source + " · " + location
}

func calendarAgendaStatusClass(month CalendarMonthData) string {
	if month.SyncError {
		return "flex items-center gap-2 text-xs font-semibold text-destructive"
	}
	return "flex items-center gap-2 text-xs font-semibold text-foreground"
}

func calendarAgendaStatusDotClass(month CalendarMonthData) string {
	if month.SyncError {
		return "size-2 rounded-full bg-destructive"
	}
	return "size-2 rounded-full bg-emerald-500"
}

func calendarAgendaStatusLabel(month CalendarMonthData) string {
	if month.SyncError {
		return "Calendar sync needs attention"
	}
	if len(month.Events) > 0 {
		return "Calendar synchronized"
	}
	return "Ready for calendar connections"
}

func calendarAgendaStatusDetail(month CalendarMonthData) string {
	if month.SyncError {
		return month.SyncMessage
	}
	return "Events are read-only for now. Event creation will follow after sync is stable."
}

func calendarAccountDisplayName(account models.Account) string {
	if name := strings.TrimSpace(account.Name); name != "" {
		return name
	}
	if email := strings.TrimSpace(account.Email); email != "" {
		return email
	}
	return "Connected account"
}

func calendarConfiguredAccounts(accounts []models.Account) []models.Account {
	configured := make([]models.Account, 0, len(accounts))
	for _, account := range accounts {
		if account.IsDeleting || !account.CalendarSyncEnabled {
			continue
		}
		configured = append(configured, account)
	}
	return configured
}

func calendarAccountProviderLabel(account models.Account) string {
	switch strings.ToLower(strings.TrimSpace(account.Provider)) {
	case "gmail":
		return "Google"
	case "outlook":
		return "Microsoft"
	default:
		provider := strings.TrimSpace(account.Provider)
		if provider == "" {
			return "Mailbox"
		}
		return strings.ToUpper(provider[:1]) + provider[1:]
	}
}
