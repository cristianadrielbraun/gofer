package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenReadOnlyRequiresExistingCurrentDatabaseAndRejectsWrites(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "directory with spaces", "gofer.db")
	db, err := New(databasePath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, email, email_normalized, name, status, auth_version, is_admin)
		VALUES ('owner', 'owner@example.com', 'owner@example.com', 'Owner', 'active', 1, 1)`); err != nil {
		t.Fatalf("insert read-only fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close writable database: %v", err)
	}

	readOnly, err := OpenReadOnly(databasePath)
	if err != nil {
		t.Fatalf("OpenReadOnly() error = %v", err)
	}
	defer readOnly.Close()
	var userID string
	if err := readOnly.Read().QueryRow(`SELECT id FROM users WHERE id = 'owner'`).Scan(&userID); err != nil || userID != "owner" {
		t.Fatalf("read-only query = %q, %v", userID, err)
	}
	if _, err := readOnly.Write().Exec(`UPDATE users SET name = 'Changed' WHERE id = 'owner'`); err == nil {
		t.Fatal("read-only compatibility connection accepted a write")
	}
}

func TestOpenReadOnlyDoesNotCreateMissingPathOrMigrateStaleSchema(t *testing.T) {
	missingDirectory := filepath.Join(t.TempDir(), "missing")
	missingPath := filepath.Join(missingDirectory, "gofer.db")
	if db, err := OpenReadOnly(missingPath); err == nil || db != nil {
		t.Fatalf("OpenReadOnly(missing) = %#v, %v", db, err)
	}
	if _, err := os.Stat(missingDirectory); !os.IsNotExist(err) {
		t.Fatalf("missing database parent was created: %v", err)
	}

	stalePath := filepath.Join(t.TempDir(), "stale.db")
	db, err := New(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`DELETE FROM schema_version`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO schema_version (version) VALUES (79)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := OpenReadOnly(stalePath)
	if err == nil || readOnly != nil || !strings.Contains(err.Error(), "schema version 79") {
		t.Fatalf("OpenReadOnly(stale) = %#v, %v", readOnly, err)
	}
}

func TestOpenExistingRequiresCurrentSchemaAndPermitsOperatorMutation(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "gofer.db")
	db, err := New(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, email, email_normalized, name, status, auth_version, is_admin)
		VALUES ('operator-target', 'target@example.com', 'target@example.com', 'Target', 'active', 1, 0)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	existing, err := OpenExisting(databasePath)
	if err != nil {
		t.Fatalf("OpenExisting(current) error = %v", err)
	}
	if _, err := existing.Write().Exec(`UPDATE users SET auth_version = 2 WHERE id = 'operator-target'`); err != nil {
		existing.Close()
		t.Fatalf("OpenExisting write error = %v", err)
	}
	if err := existing.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = New(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`DELETE FROM schema_version`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO schema_version (version) VALUES (79)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	stale, err := OpenExisting(databasePath)
	if err == nil || stale != nil || !strings.Contains(err.Error(), "schema version 79") {
		t.Fatalf("OpenExisting(stale) = %#v, %v", stale, err)
	}
}
