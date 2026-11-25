package storage

import (
	"path/filepath"
	"testing"
)

func TestMigrateV149ToV150AddsGroups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	// The shape of an installation from before groups, with people in it.
	for _, statement := range []string{
		`DROP TABLE directory_group_members`,
		`DROP TABLE directory_groups`,
		`DROP TABLE admin_revisions`,
		`INSERT INTO users (id, username, username_normalized, created_at, updated_at) VALUES ('u-ines', 'ines.moreau', 'ines.moreau', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`DELETE FROM schema_version`,
		`INSERT INTO schema_version (version) VALUES (149)`,
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
	var version, tables int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != CurrentSchemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name IN ('directory_groups', 'directory_group_members', 'admin_revisions')`).Scan(&tables); err != nil || tables != 3 {
		t.Fatalf("group tables = %d, %v", tables, err)
	}
	for _, statement := range []string{
		`INSERT INTO directory_groups (id, name, name_normalized, created_at, updated_at) VALUES ('g-field', 'Field team', 'field team', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO directory_group_members (group_id, user_id, added_at) VALUES ('g-field', 'u-ines', CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	for _, refused := range []string{
		`INSERT INTO directory_groups (id, name, name_normalized, created_at, updated_at) VALUES ('g-other', 'FIELD TEAM', 'field team', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO directory_groups (id, name, name_normalized, source, created_at, updated_at) VALUES ('g-ad', 'Imported', 'imported', 'carrier pigeon', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO directory_group_members (group_id, user_id, added_at) VALUES ('g-field', 'u-nobody', CURRENT_TIMESTAMP)`,
		`INSERT INTO directory_group_members (group_id, user_id, source, added_at) VALUES ('g-field', 'u-ines', 'manual', CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Write().Exec(refused); err == nil {
			t.Fatalf("accepted %s", refused)
		}
	}
	if _, err := db.Write().Exec(`DELETE FROM directory_groups WHERE id = 'g-field'`); err != nil {
		t.Fatal(err)
	}
	var members int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM directory_group_members`).Scan(&members); err != nil || members != 0 {
		t.Fatalf("members left after their group = %d, %v", members, err)
	}
}

// Running the migration on a database that already has groups keeps them.
func TestMigrateV149ToV150KeepsGroupsAlreadyThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO directory_groups (id, name, name_normalized, created_at, updated_at) VALUES ('g-kept', 'Night shift', 'night shift', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO admin_revisions (name, revision) VALUES ('membership', 7)`,
		`DELETE FROM schema_version`,
		`INSERT INTO schema_version (version) VALUES (149)`,
	} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	if err := migrateV149ToV150(db.Write()); err != nil {
		t.Fatal(err)
	}
	if err := migrateV149ToV150(db.Write()); err != nil {
		t.Fatal("second run", err)
	}
	var version, groups, revision int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 150 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM directory_groups WHERE id = 'g-kept'`).Scan(&groups); err != nil || groups != 1 {
		t.Fatalf("kept groups = %d, %v", groups, err)
	}
	if err := db.Read().QueryRow(`SELECT revision FROM admin_revisions WHERE name = 'membership'`).Scan(&revision); err != nil || revision != 7 {
		t.Fatalf("kept revision = %d, %v", revision, err)
	}
}
