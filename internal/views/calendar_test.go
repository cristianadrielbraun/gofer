package views

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestCalendarMonthDataBuildsMondayFirstGrid(t *testing.T) {
	location, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatalf("load test timezone: %v", err)
	}

	month := NewCalendarMonthData(time.Date(2026, time.September, 17, 12, 0, 0, 0, location))
	if month.MonthLabel != "September 2026" {
		t.Fatalf("month label = %q, want September 2026", month.MonthLabel)
	}
	if len(month.Weeks) != 5 {
		t.Fatalf("week count = %d, want 5", len(month.Weeks))
	}
	if got := month.Weeks[0].Days[0].ISODate; got != "2026-08-31" {
		t.Fatalf("first grid date = %q, want 2026-08-31", got)
	}
	if got := month.Weeks[0].Days[1].ISODate; got != "2026-09-01" {
		t.Fatalf("second grid date = %q, want 2026-09-01", got)
	}

	cellCount := 0
	inMonthCount := 0
	for _, week := range month.Weeks {
		if len(week.Days) != 7 {
			t.Fatalf("week day count = %d, want 7", len(week.Days))
		}
		for _, day := range week.Days {
			cellCount++
			if day.InMonth {
				inMonthCount++
			}
		}
	}
	if cellCount != 35 || inMonthCount != 30 {
		t.Fatalf("grid counts = %d cells and %d in-month days, want 35 and 30", cellCount, inMonthCount)
	}
}

func TestCalendarMainRendersNavigationAndAgendaState(t *testing.T) {
	month := NewCalendarMonthData(time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC))
	var output bytes.Buffer
	if err := CalendarPage(month, map[string]string{"mail_list_width": "50%"}).Render(t.Context(), &output); err != nil {
		t.Fatalf("CalendarPage.Render() error = %v", err)
	}

	html := output.String()
	for _, expected := range []string{
		`id="mail-list"`,
		`id="calendar-main"`,
		`September 2026`,
		`aria-label="Previous month"`,
		`aria-label="Next month"`,
		`No upcoming events`,
		`Google Calendar`,
		`Microsoft Calendar`,
	} {
		if !strings.Contains(html, expected) {
			t.Fatalf("calendar page missing %q: %s", expected, html)
		}
	}
}

func TestCalendarLayoutLoadsMailAndContactsListsBeforeApp(t *testing.T) {
	at := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	for _, data := range []CalendarMonthData{NewCalendarMonthData(at), NewCalendarWeekData(at)} {
		t.Run(data.View, func(t *testing.T) {
			var output bytes.Buffer
			if err := CalendarLayout(nil, data, nil).Render(t.Context(), &output); err != nil {
				t.Fatal(err)
			}
			html := output.String()
			previous := -1
			for _, script := range []string{"htmx.min.js", "virtual-scroll.js", "app.js"} {
				tag := `<script src="/assets/js/` + script + `"></script>`
				index := strings.Index(html, tag)
				if strings.Count(html, tag) != 1 || index <= previous {
					t.Fatalf("Calendar must load %s exactly once, in dependency order, so HTMX tab switches can initialize lists", script)
				}
				previous = index
			}
		})
	}
}

func TestCalendarTitleBarMatchesMailAndContacts(t *testing.T) {
	at := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	for _, data := range []CalendarMonthData{NewCalendarMonthData(at), NewCalendarWeekData(at)} {
		var output bytes.Buffer
		if err := CalendarMainContent(data).Render(t.Context(), &output); err != nil {
			t.Fatal(err)
		}
		html := output.String()
		start, end := strings.Index(html, "<header"), strings.Index(html, "</header>")
		if start < 0 || end < start {
			t.Fatal("Calendar title bar is missing")
		}
		header := html[start:end]
		for _, want := range []string{data.MonthLabel, "text-lg font-bold tracking-tight", "px-4 py-4", `data-calendar-navigate="-1"`, `data-calendar-navigate="1"`, "data-calendar-view-nav"} {
			if !strings.Contains(header, want) {
				t.Errorf("%s title bar is missing %q", data.View, want)
			}
		}
		if strings.Contains(header, ">Calendar</p>") || strings.Contains(header, "size-10") || strings.Contains(header, "sm:px-5") {
			t.Errorf("%s title bar still has the Calendar badge/label or inconsistent spacing", data.View)
		}
	}
}

func TestCalendarPageRendersTimedAndAllDayEvents(t *testing.T) {
	location := time.FixedZone("CEST", 2*60*60)
	start := time.Date(2026, time.September, 17, 9, 0, 0, 0, location)
	end := start.Add(time.Hour)
	month := NewCalendarMonthData(time.Date(2026, time.September, 17, 12, 0, 0, 0, location))
	month.HasSources = true
	month.LastSyncedAt = &start
	month.Events = []CalendarEvent{
		{Summary: "Planning", SourceName: "Primary", SourceColor: "#4285f4", StartAt: &start, EndAt: &end},
		{Summary: "Holiday", SourceName: "Primary", SourceColor: "#4285f4", AllDay: true, StartDate: "2026-09-18", EndDate: "2026-09-19"},
	}
	var output bytes.Buffer
	if err := CalendarPage(month, map[string]string{"mail_list_width": "50%"}).Render(t.Context(), &output); err != nil {
		t.Fatalf("CalendarPage.Render() error = %v", err)
	}

	html := output.String()
	for _, expected := range []string{`Planning`, `09:00`, `Holiday`, `All day`, `data-calendar-sync-status`, `Last synced 09:00`} {
		if !strings.Contains(html, expected) {
			t.Fatalf("calendar page missing rendered event content %q: %s", expected, html)
		}
	}
	if strings.Contains(html, "data-calendar-sync-label") || strings.Contains(html, "data-calendar-sync-detail") {
		t.Fatal("calendar agenda still renders the duplicate sync-status footer")
	}
	if strings.Contains(html, "border-l-2") || strings.Contains(html, "border-left-color") {
		t.Fatalf("calendar events still render the colored left border: %s", html)
	}
}

func TestCalendarSidebarActivatesCalendarAppAndShowsProviders(t *testing.T) {
	accounts := []models.Account{
		{ID: "google-1", Provider: "gmail", Name: "Personal", Email: "me@example.com", Color: "#4285f4", CalendarSyncEnabled: true},
		{ID: "outlook-1", Provider: "outlook", Name: "Work", Email: "work@example.com", Color: "#2563eb", CalendarSyncEnabled: true},
		{ID: "imap-1", Provider: "imap", Name: "Unconfigured", Email: "imap@example.com", Color: "#64748b"},
	}
	var output bytes.Buffer
	if err := SidebarHeader(accounts, "calendar").Render(t.Context(), &output); err != nil {
		t.Fatalf("SidebarHeader.Render() error = %v", err)
	}
	if err := CalendarSidebarBody(accounts).Render(t.Context(), &output); err != nil {
		t.Fatalf("CalendarSidebarBody.Render() error = %v", err)
	}

	html := output.String()
	for _, expected := range []string{
		`data-sidebar-app-button="calendar"`,
		`href="/calendar"`,
		`data-sidebar-app-button="calendar" aria-current aria-label="Calendar"`,
		`data-sidebar-app-body="calendar"`,
		`Google account`,
		`Microsoft account`,
	} {
		if !strings.Contains(html, expected) {
			t.Fatalf("calendar sidebar missing %q: %s", expected, html)
		}
	}
	if strings.Contains(html, "Unconfigured") {
		t.Fatalf("calendar sidebar rendered an unconfigured account: %s", html)
	}
}
