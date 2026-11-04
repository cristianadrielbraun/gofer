package views

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// Subscriptions are listed under their own heading rather than under Local
// folders, and are changed or removed instead of renamed on a provider.
func TestCalendarSidebarListsSubscriptions(t *testing.T) {
	accounts := []models.Account{
		{ID: "mail", Provider: "imap", Name: "Robin", Email: "robin@example.test", CalendarSyncEnabled: true, CalendarSources: []models.AccountCalendarSource{
			{ID: "home", Name: "Home", Provider: "caldav", AccessRole: "owner"},
		}},
		{ID: "local", Provider: "local", Name: "Local folders", CalendarSyncEnabled: true, CalendarSources: []models.AccountCalendarSource{
			{ID: "feed", Name: "Garden club", Provider: "ics", AccessRole: "reader", SubscriptionURL: "https://feeds.example.test/garden.ics", RefreshMinutes: 360, Color: "#22c55e"},
		}},
	}
	var output bytes.Buffer
	if err := CalendarSidebarContent(accounts, false).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	page := output.String()
	for _, want := range []string{
		`data-calendar-subscriptions`,
		`data-calendar-subscription-edit="feed"`,
		`data-calendar-subscription-url="https://feeds.example.test/garden.ics"`,
		`data-calendar-subscription-minutes="360"`,
		`data-calendar-subscription-remove="feed"`,
		`id="calendar-subscription-dialog"`,
		`data-calendar-subscription-add`,
		`data-calendar-source-stop="home"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("sidebar lacks %s", want)
		}
	}
	for _, unwanted := range []string{`data-calendar-source-stop="feed"`, `data-calendar-source-rename="feed"`, `data-calendar-source-delete="feed"`, `/api/calendar/sources/feed/export`, "Local folders"} {
		if strings.Contains(page, unwanted) {
			t.Errorf("sidebar has %s", unwanted)
		}
	}
	if subscriptions, _ := calendarSubscriptions(accounts[:1]); len(subscriptions) != 0 {
		t.Errorf("subscriptions without Local folders = %v", subscriptions)
	}
}
