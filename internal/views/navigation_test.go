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

func TestAppLayoutsPreloadNavigationOnce(t *testing.T) {
	settings := map[string]string{"timezone": "Europe/Prague"}
	pages := map[string]templ.Component{
		"mail":     Layout(nil, "inbox", nil, nil, -1, -1, 0, settings, nil, "", 0, models.EmailFilters{}),
		"contacts": ContactsLayout(nil, nil, nil, nil, false, false, models.ContactFilters{}, 0, settings),
		"calendar": CalendarLayout(nil, NewCalendarMonthData(time.Now()), settings),
		"settings": SettingsLayout(nil, models.SyncSettings{}, "accounts", settings, nil),
		"security": PasswordSecurityLayout(settings, PasswordSecurityData{}),
	}
	for name, page := range pages {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if err := page.Render(context.Background(), &output); err != nil {
				t.Fatal(err)
			}
			html := output.String()
			for _, id := range []string{"app-navigation-sidebar", "app-navigation-settings-sidebar", "app-navigation-mail-cards", "app-navigation-mail-table", "app-navigation-contacts-cards", "app-navigation-contacts-table", "app-navigation-calendar-month", "app-navigation-calendar-week", "app-navigation-settings-security", "app-navigation-settings-appearance", "app-navigation-settings-accounts", "app-navigation-settings-compose-display", "app-navigation-settings-about"} {
				if strings.Count(html, `<template id="`+id+`"`) != 1 {
					t.Errorf("expected one inert %s template", id)
				}
			}
			for _, script := range []string{"navigation.js", "passkey-registration.js", "passkey-authentication.js", "notification-avatar.js", "mail-card-layout.js"} {
				if strings.Count(html, `/assets/js/`+script) != 1 {
					t.Errorf("expected %s to load exactly once", script)
				}
			}
			if strings.Count(html, `id="choose-account-type-dialog"`) != 1 {
				t.Error("account setup dialogs must be shared by all screens")
			}
			if strings.Count(html, `id="mail-sync-progress-dialog"`) != 1 {
				t.Error("mail sync dialog must be available regardless of the initial screen")
			}
		})
	}
}

func TestNavigationTemplatesKeepBothListModesOnPhones(t *testing.T) {
	ctx := context.WithValue(context.Background(), cardsListViewKey{}, true)
	var output bytes.Buffer
	if err := AppNavigationTemplates(nil).Render(ctx, &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, mode := range []string{"cards", "table"} {
		start := strings.Index(html, `<template id="app-navigation-mail-`+mode+`">`)
		if start < 0 {
			t.Fatalf("missing %s template", mode)
		}
		end := strings.Index(html[start:], `</template>`)
		if !strings.Contains(html[start:start+end], `data-mail-list-view="`+mode+`"`) {
			t.Errorf("%s template must survive a later resize to desktop", mode)
		}
	}
	start := strings.Index(html, `<template id="app-navigation-settings-security">`)
	if strings.Contains(html[start:], `data-password-security-settings`) {
		t.Error("pending security must not invent permissions or sign-in state")
	}
}
