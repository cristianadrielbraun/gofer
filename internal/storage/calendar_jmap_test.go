package storage

import (
	"context"
	"path/filepath"
	"testing"
)

// Calendars from before schema 127 keep their rows, their events and their
// display choice, and a JMAP calendar can be stored afterwards.
func TestJMAPCalendarSourcesMigration(t *testing.T) {
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
	// Rebuild calendar_sources as schema 126 had it.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF; PRAGMA legacy_alter_table = ON;
 INSERT INTO users(id,username,username_normalized) VALUES('owner','owner','owner');
 INSERT INTO accounts(id,user_id,provider,email_address) VALUES('account','owner','jmap','robin@example.test');
 CREATE TABLE calendar_sources_old (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    provider TEXT NOT NULL CHECK (provider IN ('gmail', 'outlook', 'caldav')),
    remote_id TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    timezone TEXT NOT NULL DEFAULT '',
    color TEXT NOT NULL DEFAULT '',
    access_role TEXT NOT NULL DEFAULT '',
    is_primary INTEGER NOT NULL DEFAULT 0 CHECK (is_primary IN (0, 1)),
    is_selected INTEGER NOT NULL DEFAULT 1 CHECK (is_selected IN (0, 1)),
    is_deleted INTEGER NOT NULL DEFAULT 0 CHECK (is_deleted IN (0, 1)),
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    is_hidden INTEGER NOT NULL DEFAULT 0 CHECK (is_hidden IN (0, 1)),
    UNIQUE (account_id, remote_id));
 DROP TABLE calendar_sources;
 ALTER TABLE calendar_sources_old RENAME TO calendar_sources;
 INSERT INTO calendar_sources(id,user_id,account_id,provider,remote_id,name,is_hidden) VALUES('cal','owner','account','caldav','https://dav.example.test/cal/','Home',1);
 INSERT INTO calendar_sync_state(source_id) VALUES('cal');
 INSERT INTO calendar_events(id,user_id,source_id,remote_id,summary,all_day,start_date,end_date) VALUES('event','owner','cal','https://dav.example.test/cal/1.ics','Dentist',1,'2026-11-12','2026-11-13');
 DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(126);
 PRAGMA legacy_alter_table = OFF; PRAGMA foreign_keys = ON;`); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	for range 2 {
		if err := migrateV126ToV127(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	var name string
	var hidden int
	if err := db.Read().QueryRow(`SELECT s.name, s.is_hidden FROM calendar_events e JOIN calendar_sources s ON s.id = e.source_id WHERE e.id = 'event'`).Scan(&name, &hidden); err != nil || name != "Home" || hidden != 1 {
		t.Fatalf("migrated calendar = %q hidden %d, %v", name, hidden, err)
	}
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 127 {
		t.Fatalf("schema version = %d, %v", version, err)
	}

	// The account switches to JMAP calendars: the CalDAV one is let go.
	if err := db.ReplaceCalendarSources(ctx, "owner", "account", CalendarSourceProviderJMAP, []CalendarSource{{RemoteID: "c1", Name: "Personal", IsPrimary: true, IsSelected: true}}); err != nil {
		t.Fatal(err)
	}
	sources, err := db.ListCalendarSourcesForAccount(ctx, "owner", "account")
	if err != nil || len(sources) != 1 || sources[0].Provider != "jmap" || sources[0].RemoteID != "c1" {
		t.Fatalf("sources after switching = %+v, %v", sources, err)
	}
	// An IMAP account can't have JMAP calendars.
	if _, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,provider,email_address) VALUES('imap','owner','imap','sam@example.test')`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(ctx, "owner", "imap", CalendarSourceProviderJMAP, []CalendarSource{{RemoteID: "c1"}}); err == nil {
		t.Fatal("an IMAP account got JMAP calendars")
	}
}
