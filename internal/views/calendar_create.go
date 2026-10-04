package views

import (
	"encoding/json"
	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"time"
)

func calendarTeamsEventParams(id string) string {
	raw, _ := json.Marshal(map[string]string{"event_id": id})
	return string(raw)
}

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
	Provider    string
}

type CalendarCreateData struct {
	RequestID       string
	EventID         string
	Version         string
	EditSeries      bool
	EditOccurrence  bool
	Recurrence      *calendar.RecurrenceDraft
	Summary         string
	Description     string
	DescriptionHTML string
	Location        string
	AllDay          bool
	SourceID        string
	TeamsState      string
	Sources         []CalendarCreateSource
	Date            string
	StartTime       string
	// EndDate is inclusive for all-day form values, including edit prefill.
	EndDate  string
	EndTime  string
	TimeZone string
	Guests   string
}

type CalendarTeamsData struct {
	State, SourceID string
}

// calendarDialogClass sizes a calendar dialog as a bounded column: header and
// footer stay put while the body scrolls inside the viewport. Let content grow
// naturally up to the screen height instead of imposing a fixed desktop cap.
func calendarDialogClass(width string) string {
	return "w-[calc(100vw_-_2rem)] max-w-[calc(100vw_-_2rem)] " + width +
		" max-h-[calc(100dvh-2rem)] [&_[data-tui-dialog-panel]]:flex [&_[data-tui-dialog-panel]]:max-h-[calc(100dvh-2rem)]" +
		" [&_[data-tui-dialog-panel]]:min-h-0 [&_[data-tui-dialog-panel]]:flex-col [&_[data-tui-dialog-panel]]:gap-0 [&_[data-tui-dialog-panel]]:p-0"
}
