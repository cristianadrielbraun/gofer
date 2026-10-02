package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestGetCalendarEventPreservesDetailsAndVisibility(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized) VALUES ('event-user', 'event-user', 'event-user'), ('other-user', 'other-user', 'other-user');
		INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('event-account', 'event-user', 'gmail', 'event@example.com');`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "event-user", "event-account", "gmail", []CalendarSource{
		{ID: "event-source", RemoteID: "primary", Name: "Work", Color: "#4285f4", IsSelected: true},
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.October, 3, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	if err := db.ReplaceCalendarEvents(t.Context(), "event-user", "event-source", []CalendarEvent{
		{ID: "event-id", RemoteID: "remote-id", Summary: "Planning", Description: "Full notes", Location: "Room 2", OrganizerName: "Organizer", OrganizerEmail: "organizer@example.com", AttendeesJSON: `[{"email":"guest@example.com"}]`, StartAt: &start, EndAt: &end, StartTimeZone: "UTC", EndTimeZone: "UTC", ProviderCreatedAt: &start, ProviderUpdatedAt: &end},
		{ID: "all-day-id", RemoteID: "all-day", AllDay: true, StartDate: "2026-10-03", EndDate: "2026-10-04"},
	}, start.Add(-time.Hour), end.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	event, err := db.GetCalendarEvent(t.Context(), "event-user", "event-id")
	if err != nil || event.Description != "Full notes" || event.Location != "Room 2" || event.OrganizerEmail != "organizer@example.com" || event.SourceName != "Work" || event.SourceColor != "#4285f4" || event.AttendeesJSON != `[{"email":"guest@example.com"}]` {
		t.Fatalf("event details = %#v, error = %v", event, err)
	}
	if event.StartAt == nil || !event.StartAt.Equal(start) || event.EndAt == nil || !event.EndAt.Equal(end) || event.ProviderCreatedAt == nil || !event.ProviderCreatedAt.Equal(start) || event.ProviderUpdatedAt == nil || !event.ProviderUpdatedAt.Equal(end) {
		t.Fatal("event timestamps changed while scanning")
	}
	allDay, err := db.GetCalendarEvent(t.Context(), "event-user", "all-day-id")
	if err != nil || !allDay.AllDay || allDay.StartDate != "2026-10-03" || allDay.EndDate != "2026-10-04" || allDay.StartAt != nil {
		t.Fatalf("all-day details = %#v, error = %v", allDay, err)
	}
	for _, test := range []struct{ user, id string }{{"other-user", "event-id"}, {"event-user", "missing"}, {"", "event-id"}, {"event-user", ""}} {
		if _, err := db.GetCalendarEvent(t.Context(), test.user, test.id); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("user %q event %q lookup = %v, want not found", test.user, test.id, err)
		}
	}
	for _, test := range []struct{ name, hide, restore string }{
		{"deselected", "UPDATE calendar_sources SET is_selected = 0 WHERE id = 'event-source'", "UPDATE calendar_sources SET is_selected = 1 WHERE id = 'event-source'"},
		{"source deleted", "UPDATE calendar_sources SET is_deleted = 1 WHERE id = 'event-source'", "UPDATE calendar_sources SET is_deleted = 0 WHERE id = 'event-source'"},
		{"event deleted", "UPDATE calendar_events SET is_deleted = 1 WHERE id = 'event-id'", "UPDATE calendar_events SET is_deleted = 0 WHERE id = 'event-id'"},
		{"account deleting", "UPDATE accounts SET is_deleting = 1 WHERE id = 'event-account'", "UPDATE accounts SET is_deleting = 0 WHERE id = 'event-account'"},
		{"account transferred", "UPDATE accounts SET user_id = 'other-user' WHERE id = 'event-account'", "UPDATE accounts SET user_id = 'event-user' WHERE id = 'event-account'"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := db.Write().Exec(test.hide); err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = db.Write().Exec(test.restore) }()
			if _, err := db.GetCalendarEvent(t.Context(), "event-user", "event-id"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("hidden event lookup = %v, want not found", err)
			}
		})
	}
}
