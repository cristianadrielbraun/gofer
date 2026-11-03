package views

import (
	"context"
	"encoding/json"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"strconv"
	"time"
)

func calendarTeamsEventParams(data CalendarCreateData) string {
	scope := ""
	if data.EditSeries {
		scope = "series"
	} else if data.EditOccurrence {
		scope = "occurrence"
	}
	raw, _ := json.Marshal(map[string]string{"event_id": data.EventID, "edit_scope": scope})
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
	MeetingJoinURL  string
	// TeamsReplaceDelete is the delete request that removes this Teams meeting
	// once a copy without Teams is created. Empty when it can't be replaced.
	TeamsReplaceDelete string
	// Reminder is the event's reminder in minutes, "none", or empty for a new
	// event, which keeps the calendar's default.
	Reminder string
	// ShowAs is how the event marks its time; empty means busy.
	ShowAs    string
	Private   bool
	Sources   []CalendarCreateSource
	Date      string
	StartTime string
	// EndDate is inclusive for all-day form values, including edit prefill.
	EndDate  string
	EndTime  string
	TimeZone string
	Guests   string
}

type CalendarTeamsData struct {
	State, SourceID, Provider, JoinURL, EventID string
	// Replaceable: an existing Teams meeting that can be replaced by a copy
	// without Teams, since Microsoft can't turn Teams off. Series: that copy
	// is the whole series.
	Replaceable, Series bool
}

// calendarDialogClass sizes a calendar dialog as a bounded column: header and
// footer stay put while the body scrolls inside the viewport. Let content grow
// naturally up to the screen height instead of imposing a fixed desktop cap.
func calendarDialogClass(width string) string {
	return "calendar-dialog w-[calc(100vw_-_2rem)] max-w-[calc(100vw_-_2rem)] " + width +
		" max-h-[calc(100dvh-2rem)] [&_[data-tui-dialog-panel]]:flex [&_[data-tui-dialog-panel]]:max-h-[calc(100dvh-2rem)]" +
		" [&_[data-tui-dialog-panel]]:min-h-0 [&_[data-tui-dialog-panel]]:flex-col [&_[data-tui-dialog-panel]]:gap-0 [&_[data-tui-dialog-panel]]:p-0"
}

// calendarReminderChoices lists the editor's reminder options, keeping an
// event's own reminder when it isn't one of them.
func calendarReminderChoices(ctx context.Context, data CalendarCreateData) (string, [][2]string) {
	choices := [][2]string{{"none", i18n.T(ctx, "calendar.reminder.none")}}
	for _, minutes := range []int{0, 5, 10, 15, 30, 60, 1440} {
		choices = append(choices, [2]string{strconv.Itoa(minutes), CalendarReminderLabel(ctx, minutes)})
	}
	current := data.Reminder
	if current == "" {
		return "default", append([][2]string{{"default", i18n.T(ctx, "calendar.reminder.default")}}, choices...)
	}
	for _, choice := range choices {
		if choice[0] == current {
			return current, choices
		}
	}
	if minutes, err := strconv.Atoi(current); err == nil {
		return current, append(choices, [2]string{current, CalendarReminderLabel(ctx, minutes)})
	}
	return "none", choices
}

// CalendarReminderLabel describes a reminder, in minutes before the start.
func CalendarReminderLabel(ctx context.Context, minutes int) string {
	if minutes == 0 {
		return i18n.T(ctx, "calendar.reminder.at_start")
	}
	return i18n.T(ctx, "calendar.reminder.before", "span", CalendarReminderSpan(ctx, minutes))
}

// CalendarReminderSpan says how long a reminder is, such as "15 minutes".
func CalendarReminderSpan(ctx context.Context, minutes int) string {
	switch {
	case minutes > 0 && minutes%1440 == 0:
		return i18n.N(ctx, "calendar.duration.days", minutes/1440)
	case minutes > 0 && minutes%60 == 0:
		return i18n.N(ctx, "calendar.duration.hours", minutes/60)
	}
	return i18n.N(ctx, "calendar.duration.minutes", minutes)
}

// calendarShowAsChoices lists busy and free, keeping an Outlook event's other
// state when it has one.
func calendarShowAsChoices(ctx context.Context, data CalendarCreateData) (string, [][2]string) {
	choices := [][2]string{{"busy", CalendarShowAsLabel(ctx, "busy")}, {"free", CalendarShowAsLabel(ctx, "free")}}
	switch data.ShowAs {
	case "", "busy":
		return "busy", choices
	case "free":
		return "free", choices
	}
	return data.ShowAs, append(choices, [2]string{data.ShowAs, CalendarShowAsLabel(ctx, data.ShowAs)})
}

// CalendarShowAsLabel names how an event marks its time.
func CalendarShowAsLabel(ctx context.Context, showAs string) string {
	switch showAs {
	case "free", "tentative", "oof", "working_elsewhere":
		return i18n.T(ctx, "calendar.show_as."+showAs)
	}
	return i18n.T(ctx, "calendar.show_as.busy")
}

func calendarPrivateValue(data CalendarCreateData) string {
	if data.Private {
		return "true"
	}
	return "false"
}
