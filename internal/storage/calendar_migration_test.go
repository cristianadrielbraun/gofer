package storage

import (
	"path/filepath"
	"testing"
)

func TestMigrateV93ToV94CreatesCalendarSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (
			version INTEGER PRIMARY KEY,
			applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO schema_version (version) VALUES (93);
		CREATE TABLE users (id TEXT PRIMARY KEY);
		CREATE TABLE accounts (
			id TEXT PRIMARY KEY,
			user_id TEXT REFERENCES users(id) ON DELETE CASCADE
		);
	`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("migrate Calendar schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 94 {
		t.Fatalf("schema version = %d, want 94", version)
	}

	for _, table := range []string{"calendar_sources", "calendar_events", "calendar_sync_state"} {
		var count int
		if err := db.Read().QueryRow(`
			SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatalf("inspect %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("table %s count = %d, want 1", table, count)
		}
	}
}

func TestCalendarSchemaPreservesAllDayAndTimedEventInvariants(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized)
		VALUES ('calendar-user', 'calendar-user', 'calendar-user');
		INSERT INTO accounts (id, user_id, email_address)
		VALUES ('calendar-account', 'calendar-user', 'calendar@example.com');
		INSERT INTO calendar_sources (id, user_id, account_id, provider, remote_id, name)
		VALUES ('calendar-source', 'calendar-user', 'calendar-account', 'gmail', 'primary', 'Primary');
		INSERT INTO calendar_sync_state (source_id) VALUES ('calendar-source');
		INSERT INTO calendar_events (
			id, user_id, source_id, remote_id, status, summary, all_day,
			start_date, end_date
		) VALUES (
			'all-day-event', 'calendar-user', 'calendar-source', 'all-day', 'confirmed',
			'Holiday', 1, '2026-09-17', '2026-09-18'
		);
		INSERT INTO calendar_events (
			id, user_id, source_id, remote_id, status, summary, all_day,
			start_at, end_at, start_timezone, end_timezone
		) VALUES (
			'timed-event', 'calendar-user', 'calendar-source', 'timed', 'confirmed',
			'Meeting', 0, '2026-09-17 09:00:00', '2026-09-17 10:00:00',
			'Europe/Prague', 'Europe/Prague'
		);`); err != nil {
		t.Fatalf("insert valid Calendar events: %v", err)
	}

	if _, err := db.Write().Exec(`
		INSERT INTO calendar_events (
			id, user_id, source_id, remote_id, status, summary, all_day,
			start_at, end_at
		) VALUES (
			'invalid-event', 'calendar-user', 'calendar-source', 'invalid', 'confirmed',
			'Invalid', 1, '2026-09-17 09:00:00', '2026-09-17 10:00:00'
		)`); err == nil {
		t.Fatal("Calendar schema accepted mixed all-day and timed fields")
	}

	if _, err := db.Write().Exec(`DELETE FROM users WHERE id = 'calendar-user'`); err != nil {
		t.Fatalf("delete Calendar owner: %v", err)
	}
	for _, check := range []struct {
		name  string
		table string
	}{{"sources", "calendar_sources"}, {"events", "calendar_events"}, {"sync state", "calendar_sync_state"}} {
		var count int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM ` + check.table).Scan(&count); err != nil {
			t.Fatalf("count Calendar %s: %v", check.name, err)
		}
		if count != 0 {
			t.Fatalf("Calendar %s count = %d after owner deletion, want 0", check.name, count)
		}
	}
}

func TestGetAccountsReportsOnlySelectedCalendarSources(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized) VALUES ('calendar-list-user', 'calendar-list-user', 'calendar-list-user');
		INSERT INTO accounts (id, user_id, provider, email_address) VALUES
			('calendar-selected', 'calendar-list-user', 'gmail', 'selected@example.com'),
			('calendar-unselected', 'calendar-list-user', 'outlook', 'unselected@example.com'),
			('calendar-none', 'calendar-list-user', 'gmail', 'none@example.com');
		INSERT INTO calendar_sources (id, user_id, account_id, provider, remote_id, name, is_selected)
		VALUES
			('selected-source', 'calendar-list-user', 'calendar-selected', 'gmail', 'primary', 'Primary', 1),
			('unselected-source', 'calendar-list-user', 'calendar-unselected', 'outlook', 'primary', 'Primary', 0);`); err != nil {
		t.Fatalf("insert calendar account fixtures: %v", err)
	}

	accounts, err := db.GetAccounts(t.Context(), "calendar-list-user")
	if err != nil {
		t.Fatalf("GetAccounts() error = %v", err)
	}
	byID := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		byID[account.ID] = account.CalendarSyncEnabled
	}
	if !byID["calendar-selected"] {
		t.Fatalf("selected calendar account = %#v, want enabled", byID)
	}
	if byID["calendar-unselected"] || byID["calendar-none"] {
		t.Fatalf("unconfigured calendar accounts = %#v, want disabled", byID)
	}
}
