package storage

import (
	"strings"
	"testing"

	"path/filepath"
)

func TestMigrateV76AddsUserLifecycleFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB() error = %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (76);
		CREATE TABLE users (
			id TEXT PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			avatar_url TEXT NOT NULL DEFAULT '',
			is_admin INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO users (id, email, name, is_admin) VALUES ('owner', ' Owner@Example.COM ', 'Owner', 1);
	`); err != nil {
		raw.Close()
		t.Fatalf("seed v76 database: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close v76 database: %v", err)
	}

	db, err := New(path)
	if err != nil {
		t.Fatalf("New() migration error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var version, authVersion, mfaRequired int
	var emailNormalized, status string
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("query version: %v", err)
	}
	if err := db.Read().QueryRow(`
		SELECT email_normalized, status, auth_version, mfa_required
		FROM users WHERE id = 'owner'`).Scan(&emailNormalized, &status, &authVersion, &mfaRequired); err != nil {
		t.Fatalf("query migrated user: %v", err)
	}
	if version != 79 || emailNormalized != "owner@example.com" || status != "active" || authVersion != 1 || mfaRequired != 0 {
		t.Fatalf("migrated user = version:%d email:%q status:%q auth:%d mfa:%d", version, emailNormalized, status, authVersion, mfaRequired)
	}
	if _, err := db.Write().Exec(`INSERT INTO users (id, email, email_normalized) VALUES ('duplicate', 'duplicate@example.com', 'owner@example.com')`); err == nil {
		t.Fatal("normalized email uniqueness accepted a duplicate")
	}
}

func TestMigrateV76RollsBackNormalizedEmailCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB() error = %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (76);
		CREATE TABLE users (
			id TEXT PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			avatar_url TEXT NOT NULL DEFAULT '',
			is_admin INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO users (id, email) VALUES ('one', 'Person@Example.com'), ('two', 'person@example.com');
	`); err != nil {
		raw.Close()
		t.Fatalf("seed collision database: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close collision database: %v", err)
	}

	if db, err := New(path); err == nil {
		_ = db.Close()
		t.Fatal("New() accepted a normalized email collision")
	} else if !strings.Contains(err.Error(), "normalized email collision") {
		t.Fatalf("New() error = %v, want collision detail", err)
	}

	raw, err = openDB(path)
	if err != nil {
		t.Fatalf("reopen collision database: %v", err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("query rolled back version: %v", err)
	}
	hasLifecycleColumn := false
	rows, err := raw.Query(`PRAGMA table_info(users)`)
	if err != nil {
		t.Fatalf("table info: %v", err)
	}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			t.Fatalf("scan table info: %v", err)
		}
		hasLifecycleColumn = hasLifecycleColumn || name == "status"
	}
	rows.Close()
	if version != 76 || hasLifecycleColumn {
		t.Fatalf("failed migration left version=%d status_column=%t, want 76/false", version, hasLifecycleColumn)
	}
}
