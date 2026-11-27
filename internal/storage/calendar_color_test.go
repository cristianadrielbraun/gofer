package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrateV154ToV155AddsChosenCalendarColor(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "colors.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// calendar_sources as schema 154 had it, with one calendar.
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('owner','owner','owner');
 INSERT INTO accounts(id,user_id,provider,email_address) VALUES('account','owner','gmail','robin@example.com');
 ALTER TABLE calendar_sources DROP COLUMN chosen_color;
 INSERT INTO calendar_sources(id,user_id,account_id,provider,remote_id,name,color) VALUES('cal','owner','account','gmail','robin@example.com','Robin','#9fe1e7');
 DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(154);`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV154ToV155(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	var color, chosen string
	if err := db.Read().QueryRow(`SELECT color, chosen_color FROM calendar_sources WHERE id='cal'`).Scan(&color, &chosen); err != nil || color != "#9fe1e7" || chosen != "" {
		t.Fatalf("migrated calendar = %q %q, %v", color, chosen, err)
	}
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 155 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
}

// A color the provider didn't take is shown for the calendar and its events
// in place of the provider's, and reading the calendar list again leaves it
// alone. One the provider took becomes the calendar's own.
func TestCalendarColorPickedInGofer(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "colors.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := t.Context()
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('owner','owner','owner'),('other','other','other');
 INSERT INTO accounts(id,user_id,provider,email_address) VALUES('account','owner','imap','robin@example.com');`); err != nil {
		t.Fatal(err)
	}
	list := []CalendarSource{{ID: "cal", RemoteID: "https://dav.example.com/home/", Name: "Home", Color: "#16a765", IsSelected: true}}
	if err := db.ReplaceCalendarSources(ctx, "owner", "account", CalendarSourceProviderCalDAV, list); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 12, 4, 0, 0, 0, 0, time.UTC)
	if err := db.ReplaceCalendarEvents(ctx, "owner", "cal", []CalendarEvent{{ID: "lunch", RemoteID: "lunch.ics", Summary: "Lunch on Friday?", AllDay: true, StartDate: "2026-12-04", EndDate: "2026-12-05"}}, day.AddDate(0, 0, -7), day.AddDate(0, 0, 7)); err != nil {
		t.Fatal(err)
	}
	shown := func() (string, string) {
		t.Helper()
		sources, err := db.ListSelectedCalendarSources(ctx, "owner")
		if err != nil || len(sources) != 1 {
			t.Fatalf("sources = %+v, %v", sources, err)
		}
		events, err := db.ListCalendarEvents(ctx, "owner", day.AddDate(0, 0, -1), day.AddDate(0, 0, 2))
		if err != nil || len(events) != 1 {
			t.Fatalf("events = %+v, %v", events, err)
		}
		return sources[0].ShownColor(), events[0].SourceColor
	}

	if err := db.SetCalendarSourceColor(ctx, "owner", "cal", CalendarColorChange{Color: "#EF4444"}); err != nil {
		t.Fatal(err)
	}
	if calendar, event := shown(); calendar != "#ef4444" || event != "#ef4444" {
		t.Fatalf("kept in Gofer: calendar %q, event %q", calendar, event)
	}
	if err := db.ReplaceCalendarSources(ctx, "owner", "account", CalendarSourceProviderCalDAV, list); err != nil {
		t.Fatal(err)
	}
	if calendar, event := shown(); calendar != "#ef4444" || event != "#ef4444" {
		t.Fatalf("after reading the list again: calendar %q, event %q", calendar, event)
	}

	if err := db.SetCalendarSourceColor(ctx, "owner", "cal", CalendarColorChange{}); err != nil {
		t.Fatal(err)
	}
	if calendar, event := shown(); calendar != "#16a765" || event != "#16a765" {
		t.Fatalf("back to the provider's: calendar %q, event %q", calendar, event)
	}

	if err := db.SetCalendarSourceColor(ctx, "owner", "cal", CalendarColorChange{Color: "#2563eb", OnProvider: true}); err != nil {
		t.Fatal(err)
	}
	sources, err := db.ListSelectedCalendarSources(ctx, "owner")
	if err != nil || sources[0].Color != "#2563eb" || sources[0].ChosenColor != "" {
		t.Fatalf("taken by the provider = %+v, %v", sources, err)
	}

	if err := db.SetCalendarSourceColor(ctx, "other", "cal", CalendarColorChange{Color: "#ef4444"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("someone else's calendar = %v", err)
	}
	if err := db.SetCalendarSourceColor(ctx, "owner", "cal", CalendarColorChange{Color: "red"}); err == nil {
		t.Fatal("a color that isn't #rrggbb was saved")
	}
}
