package views

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestCalendarTransferDialogs(t *testing.T) {
	accounts := []models.Account{
		{ID: "a1", Name: "Robin", Email: "robin@example.com", CalendarSyncEnabled: true, CalendarSources: []models.AccountCalendarSource{
			{ID: "s1", Name: "Garden", Provider: "caldav", AccessRole: "owner"},
			{ID: "s2", Name: "Holidays", Provider: "caldav", AccessRole: "reader"},
		}},
		{ID: "a2", Name: "Sam", Email: "sam@example.com", CalendarSyncEnabled: true, CalendarSources: []models.AccountCalendarSource{
			{ID: "s3", Name: "Work", Provider: "gmail", AccessRole: "owner"},
		}},
	}
	var out bytes.Buffer
	if err := CalendarSidebarContent(accounts, false).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	page := out.String()
	for _, want := range []string{`aria-label="Calendar actions"`, `data-calendar-import-open=""`, `data-calendar-export-open=""`, `id="calendar-import-dialog"`, `id="calendar-export-dialog"`, `data-calendar-import-open="s1"`} {
		if !strings.Contains(page, want) {
			t.Errorf("sidebar lacks %s", want)
		}
	}
	importDialog := page[strings.Index(page, `id="calendar-import-dialog"`):strings.Index(page, `id="calendar-export-dialog"`)]
	if !strings.Contains(importDialog, `value="source:s1"`) || strings.Contains(importDialog, "source:s2") || strings.Contains(importDialog, "source:s3") {
		t.Error("import offers the wrong calendars: only writable CalDAV ones belong")
	}
	if !strings.Contains(importDialog, `value="account:a1"`) || strings.Contains(importDialog, "account:a2") {
		t.Error("import offers new calendars on the wrong accounts: only CalDAV ones belong")
	}
	exportDialog := page[strings.Index(page, `id="calendar-export-dialog"`):]
	if !strings.Contains(exportDialog, `value="s1"`) || !strings.Contains(exportDialog, `value="s2"`) || strings.Contains(exportDialog, `value="s3"`) {
		t.Error("export offers the wrong calendars: every CalDAV one belongs, read-only too")
	}
}
