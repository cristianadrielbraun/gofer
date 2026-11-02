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
		{ID: "a3", Name: "Kim", Email: "kim@example.com", CalendarSyncEnabled: true, CalendarSources: []models.AccountCalendarSource{
			{ID: "s4", Name: "Feed", Provider: "ics", AccessRole: "reader"},
		}},
	}
	var out bytes.Buffer
	if err := CalendarSidebarContent(accounts, false).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	page := out.String()
	for _, want := range []string{`aria-label="Calendar actions"`, `data-calendar-import-open=""`, `data-calendar-export-open=""`, `id="calendar-import-dialog"`, `id="calendar-export-dialog"`, `data-calendar-import-open="s1"`, `href="/api/calendar/sources/s2/export"`} {
		if !strings.Contains(page, want) {
			t.Errorf("sidebar lacks %s", want)
		}
	}
	if strings.Contains(page, `data-calendar-import-open="s2"`) {
		t.Error("a read-only calendar's menu offers Import events")
	}
	importDialog := page[strings.Index(page, `id="calendar-import-dialog"`):strings.Index(page, `id="calendar-export-dialog"`)]
	if !strings.Contains(importDialog, `value="source:s1"`) || strings.Contains(importDialog, "source:s2") || !strings.Contains(importDialog, `value="source:s3"`) || strings.Contains(importDialog, "source:s4") {
		t.Error("import offers the wrong calendars: only writable CalDAV, JMAP, Google and Microsoft ones belong")
	}
	if !strings.Contains(importDialog, `value="account:a1"`) || !strings.Contains(importDialog, `value="account:a2"`) || strings.Contains(importDialog, "account:a3") {
		t.Error("import offers new calendars on the wrong accounts")
	}
	exportDialog := page[strings.Index(page, `id="calendar-export-dialog"`):]
	if !strings.Contains(exportDialog, `value="s1"`) || !strings.Contains(exportDialog, `value="s2"`) || !strings.Contains(exportDialog, `value="s3"`) || strings.Contains(exportDialog, `value="s4"`) {
		t.Error("export offers the wrong calendars: read-only ones belong too, feeds don't")
	}
}
