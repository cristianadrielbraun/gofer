package views

import (
	"bytes"
	"strings"
	"testing"
)

func TestCalendarCreateUsesTemplUIControls(t *testing.T) {
	data := CalendarCreateData{SourceID: "work", Sources: []CalendarCreateSource{
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
		`data-calendar-create-source-select`, `id="calendar-create-source"`, `class="select-container`,
		`name="source_id" data-tui-selectbox-hidden-input value="work"`, `role="listbox"`, `role="group"`,
		`data-tui-selectbox-value="work" data-tui-selectbox-selected="true" data-tui-selectbox-disabled="false"`,
		`data-tui-selectbox-value="holidays" data-tui-selectbox-selected="false" data-tui-selectbox-disabled="true"`,
		`data-calendar-source-authorized="false"`, `Read-only`, `Outlook`, `Google`,
		`data-tui-textarea`, `data-tui-label-disabled-style`, `data-calendar-create-submit`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("New event is missing templUI control contract %q", want)
		}
	}
	if strings.Contains(html, "<select") || strings.Contains(html, "<option") {
		t.Error("New event must not render a native calendar selector")
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
