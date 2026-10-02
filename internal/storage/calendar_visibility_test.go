package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestCalendarVisibilityDoesNotChangeSyncOrCache(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized) VALUES ('visibility-user', 'visibility-user', 'visibility-user'), ('other', 'other', 'other');
		INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('visibility-account', 'visibility-user', 'gmail', 'visibility@example.com');`); err != nil {
		t.Fatal(err)
	}
	sources := []CalendarSource{{ID: "work", RemoteID: "primary", Name: "Work", Color: "#4285f4", IsSelected: true}, {ID: "personal", RemoteID: "personal", Name: "Personal", IsSelected: false}}
	if err := db.ReplaceCalendarSources(ctx, "visibility-user", "visibility-account", "gmail", sources); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	if err := db.ReplaceCalendarEvents(ctx, "visibility-user", "work", []CalendarEvent{{ID: "meeting", RemoteID: "meeting", Summary: "Planning", StartAt: &start, EndAt: &end}}, start.Add(-time.Hour), end.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCalendarSourceVisibility(ctx, "visibility-user", "work", false); err != nil {
		t.Fatal(err)
	}
	selected, err := db.ListSelectedCalendarSources(ctx, "visibility-user")
	if err != nil || len(selected) != 1 || !selected[0].IsHidden || !selected[0].IsSelected {
		t.Fatalf("sync sources = %#v, error = %v", selected, err)
	}
	if err := db.StartCalendarSync(ctx, "visibility-user", "work"); err != nil {
		t.Fatalf("hidden calendar cannot sync: %v", err)
	}
	events, err := db.ListCalendarEvents(ctx, "visibility-user", start.Add(-time.Hour), end.Add(time.Hour))
	if err != nil || len(events) != 1 || events[0].ID != "meeting" || !events[0].SourceHidden {
		t.Fatalf("hidden cache = %#v, error = %v", events, err)
	}
	accounts, err := db.GetAccounts(ctx, "visibility-user")
	if err != nil || len(accounts) != 1 || !accounts[0].CalendarSyncEnabled || len(accounts[0].CalendarSources) != 1 || !accounts[0].CalendarSources[0].IsHidden || accounts[0].CalendarSources[0].Color != "#4285f4" {
		t.Fatalf("sidebar metadata = %#v, error = %v", accounts, err)
	}
	// Discovery and setup selection must preserve display choices.
	if err := db.ReplaceCalendarSources(ctx, "visibility-user", "visibility-account", "gmail", sources); err != nil {
		t.Fatal(err)
	}
	for _, selection := range [][]string{nil, {"work"}} {
		if err := db.SetCalendarSourceSelection(ctx, "visibility-user", "visibility-account", selection); err != nil {
			t.Fatal(err)
		}
	}
	selected, err = db.ListSelectedCalendarSources(ctx, "visibility-user")
	if err != nil || len(selected) != 1 || !selected[0].IsHidden {
		t.Fatal("discovery or sync selection reset display preference")
	}
	for _, test := range []struct{ user, source string }{{"other", "work"}, {"visibility-user", "missing"}, {"visibility-user", "personal"}} {
		if err := db.SetCalendarSourceVisibility(ctx, test.user, test.source, true); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("unauthorized visibility change: %v", err)
		}
	}
	if _, err := db.Write().Exec(`UPDATE accounts SET is_deleting = 1 WHERE id = 'visibility-account'`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCalendarSourceVisibility(ctx, "visibility-user", "work", true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("deleting account can change visibility")
	}
	if _, err := db.Write().Exec(`UPDATE accounts SET is_deleting = 0 WHERE id = 'visibility-account'`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCalendarSourceVisibility(ctx, "visibility-user", "work", true); err != nil {
		t.Fatal(err)
	}
	event, err := db.GetCalendarEvent(ctx, "visibility-user", "meeting")
	if err != nil || event.SourceHidden || event.Summary != "Planning" {
		t.Fatal("showing calendar changed its cache")
	}
}

func TestMigrateV96ToV97PreservesCalendarState(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY);
		INSERT INTO schema_version VALUES (96);
		CREATE TABLE calendar_sources (id TEXT PRIMARY KEY, is_selected INTEGER NOT NULL);
		INSERT INTO calendar_sources VALUES ('selected', 1), ('unselected', 0);
		CREATE TABLE calendar_events (id TEXT PRIMARY KEY, source_id TEXT REFERENCES calendar_sources(id));
		INSERT INTO calendar_events VALUES ('cached', 'selected');
		CREATE TABLE calendar_sync_state (source_id TEXT PRIMARY KEY REFERENCES calendar_sources(id), state TEXT);
		INSERT INTO calendar_sync_state VALUES ('selected', 'synced');`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV96ToV97(db); err != nil {
			t.Fatal(err)
		}
	}
	var selected, hidden, version, cached int
	var state string
	if err := db.QueryRow(`SELECT is_selected, is_hidden FROM calendar_sources WHERE id = 'selected'`).Scan(&selected, &hidden); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM calendar_events WHERE id = 'cached' AND source_id = 'selected'`).Scan(&cached); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT state FROM calendar_sync_state WHERE source_id = 'selected'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if selected != 1 || hidden != 0 || version != 97 || cached != 1 || state != "synced" {
		t.Fatalf("migration changed calendar state: %d %d %d %d %s", selected, hidden, version, cached, state)
	}
	if _, err := db.Exec(`UPDATE calendar_sources SET is_hidden = 2`); err == nil {
		t.Fatal("invalid visibility flag accepted")
	}
}
