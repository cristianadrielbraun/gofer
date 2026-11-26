package views

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

func TestMailCalendarSidebarLoadsWithTheMonthsDays(t *testing.T) {
	var out bytes.Buffer
	if err := MailCalendarSidebar(map[string]string{"mail_calendar_sidebar_enabled": "true"}).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if strings.Contains(html, i18n.T(context.Background(), "calendar.loading")) {
		t.Fatal("the calendar sidebar should show the month's shape while it loads, not loading text")
	}
	if !strings.Contains(html, `hx-trigger="intersect once"`) || !strings.Contains(html, `aria-busy="true"`) {
		t.Fatal("the loading calendar should load itself once it is on screen, and say it's busy")
	}
	if got := strings.Count(html, "data-mail-calendar-day"); got < 28 {
		t.Fatalf("the loading calendar should draw this month's days, drew %d", got)
	}
	if strings.Contains(html, i18n.T(context.Background(), "calendar.agenda.no_events")) {
		t.Fatal("the loading calendar shouldn't say the day has no events before it knows")
	}
}

func TestMailCalendarSidebarContentDoesNotLoadAgain(t *testing.T) {
	month := NewCalendarData("month", time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC), time.Monday)
	var out bytes.Buffer
	if err := MailCalendarSidebarContent(month).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if strings.Contains(html, "hx-trigger") || !strings.Contains(html, `aria-busy="false"`) {
		t.Fatal("a loaded calendar shouldn't load again")
	}
}

func TestMailCalendarSidebarIsDrawnAtItsSavedWidth(t *testing.T) {
	for _, tc := range []struct{ saved, want string }{
		{"", "300px"},
		{"360", "360px"},
		{"120", "260px"},
		{"900", "450px"},
		{"wide", "300px"},
	} {
		var out bytes.Buffer
		settings := map[string]string{"mail_calendar_sidebar_enabled": "true", "mail_calendar_width": tc.saved}
		if err := MailCalendarSidebar(settings).Render(context.Background(), &out); err != nil {
			t.Fatal(err)
		}
		html := out.String()
		if !strings.Contains(html, "--mail-calendar-width: "+tc.want) {
			t.Errorf("saved width %q should be drawn at %s", tc.saved, tc.want)
		}
		if !strings.Contains(html, `hx-preserve="true"`) {
			t.Fatal("the calendar should keep itself when Mail is drawn again, so it doesn't load twice")
		}
	}
}
