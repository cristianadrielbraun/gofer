package storage

import (
	"path/filepath"
	"testing"
)

func TestMigrateV146ToV147AddsTheAuditLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	// The shape of an installation from before the audit log, with its
	// security settings already saved.
	if _, err := db.Write().Exec(`
		DROP TABLE audit_events;
		DROP TABLE audit_checkpoints;
		ALTER TABLE auth_system_state DROP COLUMN admin_audit_retention_days;
		INSERT INTO auth_system_state (id, initialized) VALUES (1, 1);
		DELETE FROM schema_version;
		INSERT INTO schema_version (version) VALUES (146);`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var version, retention, tables int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 147 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	if err := db.Read().QueryRow(`SELECT admin_audit_retention_days FROM auth_system_state WHERE id = 1`).Scan(&retention); err != nil || retention != 730 {
		t.Fatalf("audit retention of an existing installation = %d, %v", retention, err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name IN ('audit_events', 'audit_checkpoints')`).Scan(&tables); err != nil || tables != 2 {
		t.Fatalf("audit tables = %d, %v", tables, err)
	}
	for _, days := range []int{89, 3651} {
		if _, err := db.Write().Exec(`UPDATE auth_system_state SET admin_audit_retention_days = ? WHERE id = 1`, days); err == nil {
			t.Fatalf("audit retention accepted %d days", days)
		}
	}
	if _, err := db.Write().Exec(`INSERT INTO audit_events (id, occurred_at, actor_kind, action, outcome, chain_mac) VALUES ('e', CURRENT_TIMESTAMP, 'admin', 'user.disabled', 'maybe', 'x')`); err == nil {
		t.Fatal("audit events accepted an unknown outcome")
	}
}
