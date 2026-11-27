package views

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

func renderCalendarColorTest(t *testing.T, component templ.Component) string {
	t.Helper()
	var out bytes.Buffer
	if err := component.Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// A synced calendar's menu offers Color…, with the note its picker shows:
// a Google account needs full calendar access for Google to take the color,
// other providers keep it only in Gofer. A subscription's color is changed
// with the rest of it instead.
func TestCalendarColorMenuItem(t *testing.T) {
	gmail := models.Account{ID: "acc", Provider: "gmail", Email: "alex@example.com", Name: "Alex"}
	full := gmail
	full.CalendarFullAccess = true
	for _, test := range []struct {
		account models.Account
		source  models.AccountCalendarSource
		want    string
	}{
		{gmail, models.AccountCalendarSource{Provider: "gmail"}, "google_access"},
		{full, models.AccountCalendarSource{Provider: "gmail"}, ""},
		{models.Account{}, models.AccountCalendarSource{Provider: "outlook"}, ""},
		{models.Account{CalendarCreateNeedsReconnect: true}, models.AccountCalendarSource{Provider: "outlook"}, "reconnect"},
		{models.Account{}, models.AccountCalendarSource{Provider: "caldav"}, "provider"},
		{models.Account{}, models.AccountCalendarSource{Provider: "jmap"}, "provider"},
	} {
		if got := calendarColorNote(test.account, test.source); got != test.want {
			t.Fatalf("%s with full access %v = %q, want %q", test.source.Provider, test.account.CalendarFullAccess, got, test.want)
		}
	}

	row := renderCalendarColorTest(t, calendarSourceRow(gmail, models.AccountCalendarSource{ID: "club", Name: "Book club", Provider: "gmail", AccessRole: "reader", Color: "#16a765", ColorInGoferOnly: true}, false))
	for _, want := range []string{
		`data-calendar-color-dot="club"`, `data-calendar-source-color="club"`, `data-calendar-source-name="Book club"`,
		`data-calendar-color="#16a765"`, `data-calendar-color-in-gofer="true"`, `data-calendar-color-note="google_access"`,
		`data-calendar-account-email="alex@example.com"`, "Color…",
	} {
		if !strings.Contains(row, want) {
			t.Fatalf("calendar row missing %q: %s", want, row)
		}
	}
	subscription := renderCalendarColorTest(t, calendarSourceRow(models.Account{}, models.AccountCalendarSource{ID: "feed", Name: "Holidays", Provider: "ics"}, false))
	if strings.Contains(subscription, "data-calendar-source-color") {
		t.Fatalf("a subscription offers Color…: %s", subscription)
	}
}

// The calendar color picker and the account marker picker share their
// colors, custom color field and status line.
func TestCalendarColorPicker(t *testing.T) {
	picker := renderCalendarColorTest(t, CalendarColorPicker())
	for _, want := range []string{
		`id="calendar-color-popover"`, "data-calendar-color-picker", "data-calendar-color-preview", "Calendar color",
		`data-color-picker-option="#2563eb"`, `id="calendar-color-custom"`, "data-color-picker-custom", "data-color-picker-status",
		`data-calendar-color-note="google_access"`, `data-calendar-color-note="provider"`, "data-calendar-color-allow-access",
		`data-calendar-color-note="reconnect"`, `data-calendar-color-note="refused"`, "reconnect the account from its actions menu",
		"Allow full calendar access", "Only Gofer will show the color you pick.", "data-calendar-color-reset",
		"Use the calendar&#39;s own color",
	} {
		if !strings.Contains(picker, want) {
			t.Fatalf("calendar color picker missing %q: %s", want, picker)
		}
	}
	account := renderCalendarColorTest(t, AccountColorPopover(models.Account{ID: "acc", Name: "Alex", Color: "#2563eb"}))
	for _, want := range []string{
		`data-account-color-picker`, `data-account-id="acc"`, `data-color-picker-option="#2563eb" data-account-color-active="true"`,
		`id="account-color-custom-acc"`, "data-color-picker-custom", "data-color-picker-status", `data-account-marker="acc"`, "Marker color",
		`data-color-picker-code>#2563eb</span>`, "account-color-swatch size-7 rounded-md", "color-picker-custom",
	} {
		if !strings.Contains(account, want) {
			t.Fatalf("account marker picker missing %q: %s", want, account)
		}
	}
}

// An event's color marks are found by its calendar, so a color picked for
// the calendar repaints them where they are.
func TestCalendarEventsMarkTheirColor(t *testing.T) {
	event := CalendarEvent{ID: "lunch", SourceID: "club", Summary: "Lunch on Friday?", SourceColor: "#16a765", AllDay: true}
	for name, html := range map[string]string{
		"agenda card": renderCalendarColorTest(t, CalendarAgendaEventCard(event, time.UTC, false)),
		"day dots":    renderCalendarColorTest(t, CalendarDayDots([]CalendarEvent{event})),
	} {
		start := strings.Index(html, `data-calendar-source-id="club"`)
		if start < 0 || !strings.Contains(html, "data-calendar-event-color") {
			t.Fatalf("%s doesn't mark its color by calendar: %s", name, html)
		}
	}
}
