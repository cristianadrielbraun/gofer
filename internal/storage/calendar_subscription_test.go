package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// Calendars from before schema 136 keep their rows and events, and a
// subscription can be stored in Local folders afterwards.
func TestCalendarSubscriptionMigration(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Write().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild calendar_sources as schema 135 had it.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF; PRAGMA legacy_alter_table = ON;
 INSERT INTO users(id,username,username_normalized) VALUES('owner','owner','owner');
 INSERT INTO accounts(id,user_id,provider,email_address) VALUES('account','owner','imap','robin@example.test');
 CREATE TABLE calendar_sources_old (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    provider TEXT NOT NULL CHECK (provider IN ('gmail', 'outlook', 'caldav', 'jmap')),
    remote_id TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    timezone TEXT NOT NULL DEFAULT '',
    color TEXT NOT NULL DEFAULT '',
    access_role TEXT NOT NULL DEFAULT '',
    is_primary INTEGER NOT NULL DEFAULT 0 CHECK (is_primary IN (0, 1)),
    is_selected INTEGER NOT NULL DEFAULT 1 CHECK (is_selected IN (0, 1)),
    is_hidden INTEGER NOT NULL DEFAULT 0 CHECK (is_hidden IN (0, 1)),
    is_deleted INTEGER NOT NULL DEFAULT 0 CHECK (is_deleted IN (0, 1)),
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_in_gofer INTEGER NOT NULL DEFAULT 0,
    UNIQUE (account_id, remote_id));
 DROP TABLE calendar_sources;
 ALTER TABLE calendar_sources_old RENAME TO calendar_sources;
 INSERT INTO calendar_sources(id,user_id,account_id,provider,remote_id,name,is_hidden,created_in_gofer) VALUES('cal','owner','account','caldav','https://dav.example.test/cal/','Home',1,1);
 INSERT INTO calendar_sync_state(source_id) VALUES('cal');
 INSERT INTO calendar_events(id,user_id,source_id,remote_id,summary,all_day,start_date,end_date) VALUES('event','owner','cal','https://dav.example.test/cal/1.ics','Dentist',1,'2026-11-12','2026-11-13');
 DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(135);
 PRAGMA legacy_alter_table = OFF; PRAGMA foreign_keys = ON;`); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	for range 2 {
		if err := migrateV135ToV136(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	var name string
	var hidden, created, refresh int
	if err := db.Read().QueryRow(`SELECT s.name, s.is_hidden, s.created_in_gofer, s.refresh_minutes FROM calendar_events e JOIN calendar_sources s ON s.id = e.source_id WHERE e.id = 'event'`).Scan(&name, &hidden, &created, &refresh); err != nil || name != "Home" || hidden != 1 || created != 1 || refresh != 0 {
		t.Fatalf("migrated calendar = %q hidden %d created %d refresh %d, %v", name, hidden, created, refresh, err)
	}
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 136 {
		t.Fatalf("schema version = %d, %v", version, err)
	}

	local, err := db.EnsureLocalFoldersAccount(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	source, err := db.AddCalendarSubscription(ctx, "owner", local, CalendarSubscription{URL: "https://feeds.example.test/holidays.ics", Name: "Holidays", Color: "#22c55e", RefreshMinutes: 360})
	if err != nil {
		t.Fatal(err)
	}
	sources, err := db.ListSelectedCalendarSources(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	var found *CalendarSource
	for i := range sources {
		if sources[i].ID == source.ID {
			found = &sources[i]
		}
	}
	if found == nil || found.Provider != CalendarSourceProviderICS || found.RemoteID != "https://feeds.example.test/holidays.ics" || found.RefreshMinutes != 360 || found.AccessRole != "reader" || found.AccountID != local {
		t.Fatalf("subscription = %+v", found)
	}
}

func TestCalendarSubscriptionChanges(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "subscriptions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('owner','owner','owner');
 INSERT INTO accounts(id,user_id,provider,email_address) VALUES('mail','owner','imap','robin@example.test')`); err != nil {
		t.Fatal(err)
	}
	local, err := db.EnsureLocalFoldersAccount(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	feed := CalendarSubscription{URL: "https://feeds.example.test/team.ics", Name: "Team", Color: "#2563eb", RefreshMinutes: 60}
	// Only Local folders hold subscriptions.
	if _, err := db.AddCalendarSubscription(ctx, "owner", "mail", feed); err == nil {
		t.Fatal("a mail account took a subscription")
	}
	source, err := db.AddCalendarSubscription(ctx, "owner", local, feed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddCalendarSubscription(ctx, "owner", local, feed); !errors.Is(err, ErrCalendarSubscriptionExists) {
		t.Fatalf("second subscription to the same feed: %v", err)
	}
	if _, err := db.Write().Exec(`INSERT INTO calendar_events(id,user_id,source_id,remote_id,summary,all_day,start_date,end_date) VALUES('event','owner',?,'uid-1','Standup',1,'2026-11-12','2026-11-13')`, source.ID); err != nil {
		t.Fatal(err)
	}

	// Renaming keeps the events; a new address drops them for the new feed.
	if err := db.UpdateCalendarSubscription(ctx, "owner", source.ID, CalendarSubscription{URL: feed.URL, Name: "My team", Color: "#ef4444", RefreshMinutes: 1440}); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events WHERE source_id = ?`, source.ID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("events after rename = %d, %v", events, err)
	}
	if err := db.UpdateCalendarSubscription(ctx, "owner", source.ID, CalendarSubscription{URL: "https://feeds.example.test/other.ics", Name: "My team", Color: "#ef4444", RefreshMinutes: 1440}); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events WHERE source_id = ?`, source.ID).Scan(&events); err != nil || events != 0 {
		t.Fatalf("events after a new address = %d, %v", events, err)
	}
	sources, err := db.ListSelectedCalendarSources(ctx, "owner")
	if err != nil || len(sources) != 1 || sources[0].Name != "My team" || sources[0].Color != "#ef4444" || sources[0].RefreshMinutes != 1440 || sources[0].RemoteID != "https://feeds.example.test/other.ics" || sources[0].SyncState != "pending" {
		t.Fatalf("after changes = %+v, %v", sources, err)
	}

	if err := db.RemoveCalendarSubscription(ctx, "owner", source.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveCalendarSubscription(ctx, "owner", source.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("removing twice: %v", err)
	}
	if err := db.UpdateCalendarSubscription(ctx, "owner", source.ID, feed); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("changing a removed subscription: %v", err)
	}
}
