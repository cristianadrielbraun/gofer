package storage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestReplaceCalendarSourcesReconcilesAndPreservesSelection(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized)
		VALUES ('calendar-user', 'calendar-user', 'calendar-user');
		INSERT INTO accounts (id, user_id, provider, email_address)
		VALUES ('calendar-account', 'calendar-user', 'gmail', 'calendar@example.com');`); err != nil {
		t.Fatalf("insert Calendar account: %v", err)
	}

	if err := db.ReplaceCalendarSources(t.Context(), "calendar-user", "calendar-account", "gmail", []CalendarSource{
		{RemoteID: "primary", Name: "Primary", TimeZone: "Europe/Prague", IsPrimary: true, IsSelected: true},
		{RemoteID: "team", Name: "Team", IsPrimary: false, IsSelected: false},
	}); err != nil {
		t.Fatalf("initial ReplaceCalendarSources() error = %v", err)
	}

	sources, err := db.ListCalendarSourcesForAccount(t.Context(), "calendar-user", "calendar-account")
	if err != nil {
		t.Fatalf("ListCalendarSourcesForAccount() error = %v", err)
	}
	if len(sources) != 2 || sources[0].RemoteID != "primary" || !sources[0].IsSelected || sources[1].RemoteID != "team" || sources[1].IsSelected {
		t.Fatalf("initial sources = %#v", sources)
	}

	if err := db.ReplaceCalendarSources(t.Context(), "calendar-user", "calendar-account", "gmail", []CalendarSource{
		{RemoteID: "team", Name: "Team renamed", IsPrimary: false, IsSelected: true},
	}); err != nil {
		t.Fatalf("reconcile ReplaceCalendarSources() error = %v", err)
	}

	sources, err = db.ListCalendarSourcesForAccount(t.Context(), "calendar-user", "calendar-account")
	if err != nil {
		t.Fatalf("ListCalendarSourcesForAccount() after reconcile error = %v", err)
	}
	if len(sources) != 1 || sources[0].RemoteID != "team" || sources[0].Name != "Team renamed" || sources[0].IsSelected {
		t.Fatalf("reconciled sources = %#v, want existing selection preserved", sources)
	}

	var deleted, selected, syncStates int
	if err := db.Read().QueryRow(`SELECT is_deleted, is_selected FROM calendar_sources WHERE remote_id = 'primary'`).Scan(&deleted, &selected); err != nil {
		t.Fatalf("read missing primary source: %v", err)
	}
	if deleted != 1 || selected != 0 {
		t.Fatalf("missing primary source state = deleted:%d selected:%d", deleted, selected)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_sync_state`).Scan(&syncStates); err != nil {
		t.Fatalf("count Calendar sync states: %v", err)
	}
	if syncStates != 2 {
		t.Fatalf("Calendar sync state count = %d, want 2", syncStates)
	}
}

func TestReplaceCalendarEventsReconcilesWindowAndListsSelected(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized)
		VALUES ('calendar-event-user', 'calendar-event-user', 'calendar-event-user');
		INSERT INTO accounts (id, user_id, provider, email_address)
		VALUES ('calendar-event-account', 'calendar-event-user', 'gmail', 'calendar@example.com');`); err != nil {
		t.Fatalf("insert Calendar account: %v", err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "calendar-event-user", "calendar-event-account", "gmail", []CalendarSource{
		{RemoteID: "primary", Name: "Primary", Color: "#4285f4", IsPrimary: true, IsSelected: true},
	}); err != nil {
		t.Fatalf("ReplaceCalendarSources() error = %v", err)
	}
	sources, err := db.ListSelectedCalendarSources(t.Context(), "calendar-event-user")
	if err != nil {
		t.Fatalf("ListSelectedCalendarSources() error = %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("selected sources = %#v, want one source", sources)
	}

	windowStart := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	windowEnd := windowStart.AddDate(0, 1, 0)
	timedStart := time.Date(2026, time.September, 3, 9, 0, 0, 0, time.UTC)
	timedEnd := timedStart.Add(time.Hour)
	if err := db.ReplaceCalendarEvents(t.Context(), "calendar-event-user", sources[0].ID, []CalendarEvent{
		{
			RemoteID:      "timed",
			Summary:       "Planning",
			Status:        "confirmed",
			StartAt:       &timedStart,
			EndAt:         &timedEnd,
			AttendeesJSON: "not-json",
		},
		{
			RemoteID:   "all-day",
			Summary:    "Holiday",
			Status:     "confirmed",
			AllDay:     true,
			StartDate:  "2026-09-04",
			EndDate:    "2026-09-05",
			SourceName: "ignored",
		},
	}, windowStart, windowEnd); err != nil {
		t.Fatalf("initial ReplaceCalendarEvents() error = %v", err)
	}

	events, err := db.ListCalendarEvents(t.Context(), "calendar-event-user", windowStart, windowEnd)
	if err != nil {
		t.Fatalf("ListCalendarEvents() error = %v", err)
	}
	if len(events) != 2 || events[0].Summary != "Planning" || events[1].Summary != "Holiday" {
		t.Fatalf("cached events = %#v, want timed and all-day events", events)
	}
	if events[0].SourceName != "Primary" || events[0].SourceColor != "#4285f4" {
		t.Fatalf("cached source metadata = %#v", events[0])
	}
	if events[0].AttendeesJSON != "[]" {
		t.Fatalf("invalid attendees JSON was not normalized: %q", events[0].AttendeesJSON)
	}

	if err := db.ReplaceCalendarEvents(t.Context(), "calendar-event-user", sources[0].ID, []CalendarEvent{
		{
			RemoteID: "timed",
			Summary:  "Planning updated",
			Status:   "confirmed",
			StartAt:  &timedStart,
			EndAt:    &timedEnd,
		},
	}, windowStart, windowEnd); err != nil {
		t.Fatalf("reconcile ReplaceCalendarEvents() error = %v", err)
	}
	events, err = db.ListCalendarEvents(t.Context(), "calendar-event-user", windowStart, windowEnd)
	if err != nil {
		t.Fatalf("ListCalendarEvents() after reconcile error = %v", err)
	}
	if len(events) != 1 || events[0].Summary != "Planning updated" {
		t.Fatalf("reconciled events = %#v, want only updated timed event", events)
	}

	var state string
	if err := db.Read().QueryRow(`SELECT state FROM calendar_sync_state WHERE source_id = ?`, sources[0].ID).Scan(&state); err != nil {
		t.Fatalf("read calendar sync state: %v", err)
	}
	if state != "ok" {
		t.Fatalf("calendar sync state = %q, want ok", state)
	}
}
