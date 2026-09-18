package storage

import (
	"path/filepath"
	"testing"
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
