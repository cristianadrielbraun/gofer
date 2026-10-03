package views

import (
	"bytes"
	"html"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestCalendarCreateUsesTemplUIControls(t *testing.T) {
	data := CalendarCreateData{SourceID: "work", Date: "2026-10-02", EndDate: "2026-10-02", StartTime: "09:00", EndTime: "10:00", TimeZone: "Europe/Prague", Sources: []CalendarCreateSource{
		{ID: "work", Name: "Work", AccountName: "Outlook", Writable: true, Authorized: true},
		{ID: "holidays", Name: "Holidays", AccountName: "Google", Writable: false, Authorized: true},
		{ID: "reconnect", Name: "Personal", AccountName: "Google", Writable: true, Authorized: false},
	}}
	var output bytes.Buffer
	if err := CalendarCreateDialog(data).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, want := range []string{
		`action="/api/calendar/events" method="post"`, `data-calendar-event-id=""`, `name="request_id"`, `New event`, `Create event`,
		`data-calendar-create-source-select`, `id="calendar-create-source"`, `class="select-container`,
		`name="source_id" data-tui-selectbox-hidden-input value="work"`, `role="listbox"`, `role="group"`,
		`data-tui-selectbox-value="work" data-tui-selectbox-selected="true" data-tui-selectbox-disabled="false"`,
		`data-tui-selectbox-value="holidays" data-tui-selectbox-selected="false" data-tui-selectbox-disabled="true"`,
		`data-calendar-source-authorized="false"`, `Read-only`, `Outlook`, `Google`,
		`data-tui-textarea`, `data-tui-label-disabled-style`, `data-calendar-create-submit`,
		`name="start_date" value="2026-10-02" data-tui-datepicker-hidden-input`,
		`name="end_date" value="2026-10-02" data-tui-datepicker-hidden-input`,
		`name="start_time" value="09:00" data-tui-timepicker-hidden-input`,
		`name="end_time" value="10:00" data-tui-timepicker-hidden-input`,
		`data-calendar-create-timezone-select`, `data-calendar-create-timezone-option`,
		`data-calendar-repeat`, `name="repeat_frequency" data-tui-selectbox-hidden-input value="none"`,
		`class="grid grid-cols-1 items-end gap-3 data-[repeating=true]:grid-cols-2" data-calendar-repeat-row data-repeating="false"`,
		`data-calendar-repeat-interval-group hidden disabled`,
		`data-calendar-repeat-options hidden disabled`, `data-calendar-repeat-until hidden disabled`, `data-calendar-repeat-count hidden disabled`,
		`id="calendar-create-repeat-interval" type="number" name="repeat_interval"`,
		`id="calendar-create-repeat-count" type="number" name="repeat_count"`,
		`name="repeat_until" value="" data-tui-datepicker-hidden-input`,
		`name="repeat_end" data-tui-selectbox-hidden-input value="never"`,
		`data-calendar-repeat-summary role="status" aria-live="polite"`,
		`Does not repeat`, `Daily`, `Weekly`, `Monthly`, `Yearly`, `On a date`, `After occurrences`,
		`data-calendar-create-date-help hidden role="status" aria-live="polite" aria-atomic="true" class="h-8`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("New event is missing templUI control contract %q", want)
		}
	}
	for _, native := range []string{"<select", "<option", "<datalist", `type="date"`, `type="time"`} {
		if strings.Contains(html, native) {
			t.Errorf("New event must not render native picker markup %s", native)
		}
	}
	if strings.Contains(output.String(), `name="version"`) || strings.Contains(output.String(), "Save changes") {
		t.Error("creation must retain its request ID and create labels")
	}
}

func TestCalendarEditReusesFormWithServerPrefill(t *testing.T) {
	for _, test := range []struct {
		name, endDate string
		allDay        bool
	}{
		{"timed", "2026-10-24", false},
		{"single all-day", "2026-10-24", true},
		{"all-day inclusive end across DST", "2026-10-26", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := CalendarCreateData{
				RequestID: "8d29ced2-98b3-4f55-8d83-bddc7470c486",
				EventID:   "event/with space?#", Version: `version-"<&>`, SourceID: "work", AllDay: test.allDay,
				Summary: "Planning <script>alert(1)</script>", Location: "Room <2>", Description: "Line one\n</textarea><script>unsafe</script>",
				Date: "2026-10-24", EndDate: test.endDate, StartTime: "09:15", EndTime: "10:30", TimeZone: "Europe/Prague",
				Sources: []CalendarCreateSource{{ID: "work", Name: "Work", AccountName: "Outlook", Writable: true, Authorized: true}},
			}
			var output bytes.Buffer
			if err := CalendarCreateDialog(data).Render(t.Context(), &output); err != nil {
				t.Fatal(err)
			}
			markup := output.String()
			for _, want := range []string{
				`id="calendar-create-dialog"`, `data-calendar-create-form`, `data-calendar-event-id="event/with space?#"`,
				`action="/api/calendar/events/event%2Fwith%20space%3F%23"`, `Edit event`, `Close edit event`, `Save changes`,
				`name="version" value="` + html.EscapeString(data.Version) + `"`,
				`name="request_id" value="` + data.RequestID + `"`,
				`data-calendar-create-error role="alert" aria-atomic="true" tabindex="-1"`,
				`value="` + html.EscapeString(data.Summary) + `"`, `value="` + html.EscapeString(data.Location) + `"`, html.EscapeString(data.Description),
				`name="start_date" value="2026-10-24" data-tui-datepicker-hidden-input`,
				`name="end_date" value="` + test.endDate + `" data-tui-datepicker-hidden-input`,
				`name="start_time" value="09:15" data-tui-timepicker-hidden-input`, `name="end_time" value="10:30" data-tui-timepicker-hidden-input`,
				`name="timezone" data-tui-selectbox-hidden-input value="Europe/Prague"`, `data-tui-textarea`,
				`name="repeat_frequency" data-tui-selectbox-hidden-input value="none"`, `data-calendar-repeat-options hidden disabled`,
				`name="repeat_until" value="" data-tui-datepicker-hidden-input`, `name="repeat_count"`,
			} {
				if !strings.Contains(markup, want) {
					t.Errorf("edit form missing %q", want)
				}
			}
			for _, unwanted := range []string{"Create event", "<script>", "<select", `type="date"`, `type="time"`} {
				if strings.Contains(markup, unwanted) {
					t.Errorf("edit form unexpectedly contains %q", unwanted)
				}
			}
			sourceTrigger := regexp.MustCompile(`<button[^>]*id="calendar-create-source"[^>]*>`).FindString(markup)
			if !regexp.MustCompile(`\sdisabled(?:\s|=|>)`).MatchString(sourceTrigger) || !strings.Contains(sourceTrigger, "select-trigger") {
				t.Error("edit must keep the original templUI calendar selector disabled")
			}
			sourceInputs := regexp.MustCompile(`<input[^>]*name="source_id"[^>]*>`).FindAllString(markup, -1)
			if len(sourceInputs) != 1 || strings.Contains(sourceInputs[0], " disabled") || !strings.Contains(sourceInputs[0], `value="work"`) {
				t.Fatalf("edit must submit exactly one enabled original source_id: %v", sourceInputs)
			}
			if strings.Count(markup, `name="version"`) != 1 || strings.Count(markup, `name="request_id"`) != 1 {
				t.Error("edit must submit exactly one version and request_id")
			}
			allDay := regexp.MustCompile(`<input[^>]*name="all_day"[^>]*>`).FindString(markup)
			if regexp.MustCompile(`\schecked(?:\s|=|>)`).MatchString(allDay) != test.allDay {
				t.Error("all-day checkbox must match server prefill")
			}
			if got := strings.Count(markup, "data-calendar-create-time hidden"); (got == 2) != test.allDay {
				t.Errorf("server-prefilled all-day time controls hidden count = %d", got)
			}
			help := regexp.MustCompile(`<p[^>]*data-calendar-create-date-help[^>]*>`).FindString(markup)
			if strings.Contains(help, " hidden") == test.allDay {
				t.Error("inclusive end-date help must be visible for server-prefilled all-day events")
			}
		})
	}
}

func TestCalendarCreatePickerAssetsLoadOnDirectEntry(t *testing.T) {
	var page, settings bytes.Buffer
	month := NewCalendarMonthData(time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC))
	if err := CalendarLayout(nil, month, nil).Render(t.Context(), &page); err != nil {
		t.Fatal(err)
	}
	if err := SettingsComponentScripts().Render(t.Context(), &settings); err != nil {
		t.Fatal(err)
	}
	for _, html := range []string{page.String(), settings.String()} {
		for _, asset := range []string{"datepicker.min.js", "timepicker.min.js", "calendar.min.js", "selectbox.min.js"} {
			if !strings.Contains(html, asset) {
				t.Errorf("Missing picker runtime asset %s", asset)
			}
		}
	}
}

func TestCalendarCreateEmptySelectorIsDisabled(t *testing.T) {
	var output bytes.Buffer
	if err := CalendarCreateDialog(CalendarCreateData{}).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	start := strings.Index(html, `<button id="calendar-create-source"`)
	if start < 0 {
		t.Fatal("Missing templUI calendar trigger")
	}
	end := strings.Index(html[start:], ">")
	if end < 0 || !strings.Contains(html[start:start+end], " disabled") {
		t.Error("No configured calendars must leave the trigger disabled")
	}
	if !strings.Contains(html, "No configured calendars") {
		t.Error("The empty selector needs an explicit placeholder")
	}
}
