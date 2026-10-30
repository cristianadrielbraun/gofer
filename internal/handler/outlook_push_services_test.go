package handler

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// In the shared store, a calendar notification makes the account's selected
// Outlook calendars due now, and leaves the others to their schedule.
func TestOutlookCalendarNotificationSchedulesTheAccountsCalendars(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized, name) VALUES ('default', 'default', 'default', 'Default');
		INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('out-1', 'default', 'outlook', 'out-1@example.test');`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "default", "out-1", "outlook", []storage.CalendarSource{
		{RemoteID: "cal-1", Name: "Calendar", IsPrimary: true, IsSelected: true},
		{RemoteID: "cal-2", Name: "Holidays"},
	}); err != nil {
		t.Fatal(err)
	}
	sources, err := db.ListCalendarSourcesForAccount(t.Context(), "default", "out-1")
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	for _, source := range sources {
		if err := db.ScheduleCalendarSync(t.Context(), "default", source.ID, later); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{db: db}

	before := time.Now()
	h.OutlookServiceChanged(t.Context(), mail.OutlookPushCalendar, []string{"out-1"})
	scheduled, err := db.ListInstanceCalendarSyncSources(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range scheduled {
		if source.NextAttemptAt == nil {
			t.Fatalf("%s has no next attempt", source.Name)
		}
		next := *source.NextAttemptAt
		due := !next.After(time.Now())
		if due != source.IsSelected || (due && next.Before(before.Add(-time.Second))) {
			t.Fatalf("%s (selected %t) next attempt %s", source.Name, source.IsSelected, next)
		}
	}
}
