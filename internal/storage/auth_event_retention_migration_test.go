package storage

import (
	"path/filepath"
	"testing"
)

func TestMigrateV92ToV93AddsBoundedAuthenticationEventRetention(t *testing.T) {
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
		INSERT INTO schema_version (version) VALUES (92);
		CREATE TABLE users (id TEXT PRIMARY KEY);
		CREATE TABLE auth_system_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			initialized INTEGER NOT NULL DEFAULT 0 CHECK (initialized IN (0, 1))
		);
		INSERT INTO auth_system_state (id, initialized) VALUES (1, 1);
		CREATE TABLE auth_events (
			id TEXT PRIMARY KEY,
			occurred_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var version, days int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`
		SELECT auth_event_retention_days FROM auth_system_state WHERE id = 1`,
	).Scan(&days); err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion || days != 180 {
		t.Fatalf("migrated retention = version:%d days:%d, want version:%d", version, days, CurrentSchemaVersion)
	}
	for _, invalid := range []int{0, 366} {
		if _, err := db.Write().Exec(`
			UPDATE auth_system_state SET auth_event_retention_days = ? WHERE id = 1`, invalid,
		); err == nil {
			t.Fatalf("database accepted invalid retention %d", invalid)
		}
	}
	var indexes int
	if err := db.Read().QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_auth_events_occurred'`,
	).Scan(&indexes); err != nil || indexes != 1 {
		t.Fatalf("retention index count = %d, %v", indexes, err)
	}
}
