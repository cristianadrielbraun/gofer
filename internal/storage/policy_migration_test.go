package storage

import (
	"path/filepath"
	"testing"
)

func TestMigrateV150ToV151AddsPolicies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP TABLE policy_values`,
		`DROP TABLE policy_assignments`,
		`DROP TABLE policies`,
		`DELETE FROM schema_version`,
		`INSERT INTO schema_version (version) VALUES (150)`,
	} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var version, rows int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != CurrentSchemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	// Nothing is set yet: every setting keeps its value.
	if err := db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM policies) + (SELECT COUNT(*) FROM policy_values)`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("policy rows = %d, %v", rows, err)
	}
	for _, statement := range []string{
		`INSERT INTO policies (id, name, name_normalized, is_default, created_at, updated_at) VALUES ('default', 'Default policy', 'default policy', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO policies (id, name, name_normalized, priority, created_at, updated_at) VALUES ('p-1', 'Contractors', 'contractors', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO policy_values (policy_id, setting_key, value_json, updated_at) VALUES ('p-1', 'mail.export', 'false', CURRENT_TIMESTAMP)`,
		`INSERT INTO policy_assignments (policy_id, target_kind, target_id, created_at) VALUES ('p-1', 'group', 'g-1', CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	for _, refused := range []string{
		// A second Default policy, a Default policy in the order, a
		// policy outside it, and two policies at the same place.
		`INSERT INTO policies (id, name, name_normalized, is_default, created_at, updated_at) VALUES ('d-2', 'Other default', 'other default', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`UPDATE policies SET priority = 2 WHERE id = 'default'`,
		`INSERT INTO policies (id, name, name_normalized, created_at, updated_at) VALUES ('p-2', 'Finance', 'finance', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO policies (id, name, name_normalized, priority, created_at, updated_at) VALUES ('p-3', 'Finance', 'finance', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO policies (id, name, name_normalized, priority, created_at, updated_at) VALUES ('p-4', 'CONTRACTORS', 'contractors', 4, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO policy_assignments (policy_id, target_kind, target_id, created_at) VALUES ('p-1', 'domain', 'example.org', CURRENT_TIMESTAMP)`,
		`INSERT INTO policy_values (policy_id, setting_key, value_json, updated_at) VALUES ('nope', 'mail.export', 'true', CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Write().Exec(refused); err == nil {
			t.Fatalf("accepted %s", refused)
		}
	}
	if _, err := db.Write().Exec(`DELETE FROM policies WHERE id = 'p-1'`); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM policy_values) + (SELECT COUNT(*) FROM policy_assignments)`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("values and assignments left after their policy = %d, %v", left, err)
	}
	// Running it again keeps what's there.
	if _, err := db.Write().Exec(`DELETE FROM schema_version`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO schema_version (version) VALUES (150)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`UPDATE policies SET description = 'Kept' WHERE id = 'default'`); err != nil {
		t.Fatal(err)
	}
	if err := migrateV150ToV151(db.Write()); err != nil {
		t.Fatal(err)
	}
	var description string
	if err := db.Read().QueryRow(`SELECT description FROM policies WHERE id = 'default'`).Scan(&description); err != nil || description != "Kept" {
		t.Fatalf("Default policy after a second run = %q, %v", description, err)
	}
}
