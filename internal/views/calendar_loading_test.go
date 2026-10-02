package views

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestCalendarSkeletonUsesCanonicalMonthAndWeekLayout(t *testing.T) {
	for _, at := range []time.Time{
		time.Date(2027, time.February, 1, 12, 0, 0, 0, time.UTC),   // four rows
		time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC), // five rows
		time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),      // six rows
	} {
		for _, data := range []CalendarMonthData{NewCalendarMonthData(at), NewCalendarWeekData(at)} {
			var loaded, pending bytes.Buffer
			if err := CalendarPage(data, nil).Render(t.Context(), &loaded); err != nil {
				t.Fatal(err)
			}
			data.Loading, data.AutoSync = true, true
			if err := CalendarPage(data, nil).Render(t.Context(), &pending); err != nil {
				t.Fatal(err)
			}
			html := pending.String()
			for _, hook := range []string{`id="calendar-main"`, "data-calendar-surface", "data-calendar-layout-footer", `id="calendar-agenda-heading"`, `id="calendar-agenda-list"`, "data-calendar-loading-agenda-event", "data-calendar-loading", `aria-busy="true"`, "Loading calendar…"} {
				if !strings.Contains(html, hook) {
					t.Errorf("%s skeleton missing %s", data.View, hook)
				}
			}
			for _, unsafe := range []string{"data-calendar-auto-sync", "data-calendar-event-trigger", "No upcoming events", "Ready for calendar connections", "Calendar events are up to date"} {
				if strings.Contains(html, unsafe) {
					t.Errorf("%s skeleton has premature result or active request: %s", data.View, unsafe)
				}
			}
			if strings.Count(html, `data-calendar-day=`) != strings.Count(loaded.String(), `data-calendar-day=`) {
				t.Error("Loading changes the number of days")
			}
			start, end := strings.Index(html, "<header"), strings.Index(html, "</header>")+len("</header>")
			if !strings.Contains(loaded.String(), strings.ReplaceAll(html[start:end], " inert", "")) {
				t.Error("Skeleton header does not match the real controls")
			}
			if data.View == "week" {
				if strings.Count(html, "data-calendar-week-hour=") != 7*24 || strings.Count(html, "data-calendar-week-hour-label=") != 24 {
					t.Error("Week skeleton must retain the complete shared time axis")
				}
			} else if !strings.Contains(html, "data-calendar-loading-events") {
				t.Error("Month skeleton is missing event pill placeholders")
			}
		}
	}
}

func TestCalendarSkeletonSidebarShowsOnlyConfiguredAccounts(t *testing.T) {
	accounts := []models.Account{
		{ID: "configured", Provider: "gmail", Name: "Configured calendar", CalendarSyncEnabled: true},
		{ID: "unconfigured", Provider: "gmail", Name: "Unconfigured calendar"},
	}
	var pending bytes.Buffer
	if err := CalendarSidebarContent(accounts, true).Render(t.Context(), &pending); err != nil {
		t.Fatal(err)
	}
	html := pending.String()
	for _, expected := range []string{"New event", "Soon", "My calendars", "All calendars", "Connected accounts", "Configured calendar", "Calendar sync", `aria-busy="true"`, "inert"} {
		if !strings.Contains(html, expected) {
			t.Errorf("Skeleton sidebar missing %q", expected)
		}
	}
	if strings.Contains(html, "Unconfigured calendar") {
		t.Error("Skeleton leaks unconfigured accounts")
	}
}

func TestCalendarSkeletonTemplatesRespectSavedLayoutAndTimezone(t *testing.T) {
	settings := map[string]string{"timezone": "Pacific/Kiritimati", "mail_list_width": "62%"}
	data := calendarLoadingData("week", settings)
	if !data.Loading || data.AutoSync || data.Month.Location().String() != settings["timezone"] {
		t.Fatalf("Unexpected skeleton data: %+v", data)
	}
	var output bytes.Buffer
	if err := CalendarLoadingTemplates(nil, settings).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id="calendar-loading-month"`, `id="calendar-loading-week"`, `width:clamp(300px,62%,calc(100% - 300px));`, "data-calendar-week-grid", "data-calendar-month-grid"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("Loading templates missing %s", want)
		}
	}
}
