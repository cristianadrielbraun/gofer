package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrateV89AddsPasswordResetRequestStateWithoutChangingUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (89);
		CREATE TABLE users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL,
			username_normalized TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			user_type TEXT NOT NULL,
			is_admin INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO users (id, username, username_normalized, status, user_type, is_admin)
		VALUES ('webmail-user', 'Mail.User', 'mail.user', 'disabled', 'webmail', 0);
	`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := New(path)
	if err != nil {
		t.Fatalf("New() migration error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var version int
	var username, status, userType string
	var requestedAt sql.NullTime
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`
		SELECT username, status, user_type, password_reset_requested_at
		FROM users WHERE id = 'webmail-user'`,
	).Scan(&username, &status, &userType, &requestedAt); err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion || username != "Mail.User" || status != "disabled" || userType != "webmail" || requestedAt.Valid {
		t.Fatalf("migrated state = version:%d username:%q status:%q type:%q requested:%v", version, username, status, userType, requestedAt)
	}
}
