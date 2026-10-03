package views

import (
	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"time"
)

func calendarCreatePickerValue(layout, value string) time.Time {
	at, _ := time.Parse(layout, value)
	return at
}

type CalendarCreateSource struct {
	ID          string
	Name        string
	AccountName string
	Writable    bool
	Authorized  bool
}

type CalendarCreateData struct {
	RequestID   string
	EventID     string
	Version     string
	EditSeries  bool
	Recurrence  *calendar.RecurrenceDraft
	Summary     string
	Description string
	Location    string
	AllDay      bool
	SourceID    string
	Sources     []CalendarCreateSource
	Date        string
	StartTime   string
	// EndDate is inclusive for all-day form values, including edit prefill.
	EndDate  string
	EndTime  string
	TimeZone string
}
