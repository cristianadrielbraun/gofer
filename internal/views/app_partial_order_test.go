package views

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// A section switch that lands while the panels slide shows its sidebar at
// once, parsing only the response up to the list (placeSwitchSidebarEarly in
// app.js). Both sidebar panes must come whole before the list starts.
func TestSectionPartialsPutTheSidebarBeforeTheList(t *testing.T) {
	accounts := []models.Account{{ID: "acc-1", Email: "ana@example.test", Provider: "imap"}}
	month := NewCalendarMonthData(time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC))
	list := regexp.MustCompile(`\sid="mail-list"`)
	for name, partial := range map[string]templ.Component{
		"mail":     MailAppPartial(accounts, "inbox", nil, nil, 0, 0, nil, nil, "", 0, 0, models.EmailFilters{}),
		"contacts": ContactsAppPartial(accounts, nil, nil, nil, false, false, models.ContactFilters{}, 0, nil),
		"calendar": CalendarAppPartial(accounts, month, nil),
	} {
		var out bytes.Buffer
		if err := partial.Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		html := out.String()
		at := list.FindStringIndex(html)
		if at == nil {
			t.Fatalf("%s partial has no list", name)
		}
		start := html[:at[0]]
		for _, pane := range []string{`id="sidebar-app-body"`, `id="sidebar-sync-controls"`} {
			if !strings.Contains(start, pane) {
				t.Errorf("%s partial puts %s after the list", name, pane)
			}
		}
	}
}

// Mail and Contacts show a real sidebar while a switch slides; without one
// the sidebar empties until the section arrives.
func TestPagesCarryMailAndContactsSidebarCopies(t *testing.T) {
	accounts := []models.Account{{ID: "acc-1", Email: "ana@example.test", Provider: "imap", EmailSyncEnabled: true}}
	var out bytes.Buffer
	if err := AppSwitchSidebarTemplates(accounts, nil).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, mode := range []string{"mail", "contacts"} {
		start := strings.Index(html, `<template id="sidebar-copy-`+mode+`">`)
		if start < 0 {
			t.Fatalf("no %s sidebar copy", mode)
		}
		if !strings.Contains(html[start:], `data-sidebar-app-body="`+mode+`"`) {
			t.Errorf("%s copy is not the %s sidebar", mode, mode)
		}
	}
	out.Reset()
	if err := AppSwitchSidebarTemplates(nil, nil).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "sidebar-copy-") {
		t.Error("a page without accounts carries sidebar copies")
	}
}
