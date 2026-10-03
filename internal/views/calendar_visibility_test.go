package views

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"golang.org/x/net/html"
)

func TestCalendarVisibilityRendersAllCandidatesWithoutHiddenSourceFlash(t *testing.T) {
	start := time.Now().UTC().AddDate(0, 0, 1)
	// Keep the fixture on one day even when this test runs just before midnight.
	start = time.Date(start.Year(), start.Month(), start.Day(), 9, 0, 0, 0, time.UTC)
	month := NewCalendarMonthData(start)
	for index := range 7 {
		end := start.Add(time.Hour)
		month.Events = append(month.Events, CalendarEvent{ID: string(rune('a' + index)), SourceID: "work", SourceHidden: index < 3, Summary: "Meeting", StartAt: &start, EndAt: &end})
	}
	if got := calendarVisibleEventCount(month.Events); got != 4 {
		t.Fatalf("visible count = %d, want 4", got)
	}
	if got := len(calendarAgendaEvents(month)); got != 4 {
		t.Fatalf("agenda count = %d, want 4", got)
	}
	for _, data := range []CalendarMonthData{month, NewCalendarWeekData(start)} {
		data.Events = month.Events
		var output bytes.Buffer
		if err := CalendarPage(data, nil).Render(t.Context(), &output); err != nil {
			t.Fatal(err)
		}
		doc, err := html.Parse(&output)
		if err != nil {
			t.Fatal(err)
		}
		monthCandidates, hiddenCandidates := 0, 0
		var inspect func(*html.Node)
		inspect = func(node *html.Node) {
			attrs := map[string]string{}
			for _, attr := range node.Attr {
				attrs[attr.Key] = attr.Val
			}
			if _, ok := attrs["data-calendar-month-event"]; ok {
				monthCandidates++
			}
			if _, ok := attrs["data-calendar-event-trigger"]; ok && attrs["data-calendar-source-hidden"] == "true" {
				hiddenCandidates++
				if _, hidden := attrs["hidden"]; !hidden {
					t.Error("hidden-source event visible on server render")
				}
				if attrs["data-calendar-source-id"] != "work" {
					t.Error("event is missing source identity")
				}
			}
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				inspect(child)
			}
		}
		inspect(doc)
		if hiddenCandidates != 6 {
			t.Fatalf("%s hidden candidates = %d, want 6 (grid and agenda)", data.View, hiddenCandidates)
		}
		if data.View == "month" && monthCandidates != 7 {
			t.Fatal("month discarded events beyond its initial three pills")
		}
	}
}

func TestCalendarSidebarGroupsConfiguredSourcesAndMatchesLoadingRows(t *testing.T) {
	accounts := []models.Account{
		{ID: "account", Provider: "gmail", Name: "Work account", CalendarSyncEnabled: true, CalendarSources: []models.AccountCalendarSource{{ID: "primary", Name: "Work", Color: "#4285f4"}, {ID: "holidays", Name: "Holidays", IsHidden: true}}},
		{ID: "unconfigured", Provider: "outlook", Name: "Unconfigured account", CalendarSources: []models.AccountCalendarSource{{ID: "not-selected", Name: "Not configured"}}},
	}
	for _, loading := range []bool{false, true} {
		var output bytes.Buffer
		if err := CalendarSidebarContent(accounts, loading).Render(t.Context(), &output); err != nil {
			t.Fatal(err)
		}
		body := output.String()
		for _, expected := range []string{`aria-label="Calendars for Work account"`, `data-calendar-visibility="primary"`, `data-calendar-visibility="holidays"`, `background-color: #4285f4;`, `data-calendar-source-hidden="true"`} {
			if !strings.Contains(body, expected) {
				t.Errorf("sidebar missing %q", expected)
			}
		}
		if count := strings.Count(body, "data-calendar-visibility="); count != 2 {
			t.Fatalf("toggle count = %d, want 2", count)
		}
		if strings.Contains(body, "Unconfigured account") || strings.Contains(body, "Not configured") {
			t.Error("sidebar exposed an unconfigured account")
		}
	}
}
