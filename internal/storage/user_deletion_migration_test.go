package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrateV90AddsResumableUserDeletionStateWithoutChangingUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (90);
		CREATE TABLE users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL,
			username_normalized TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			user_type TEXT NOT NULL,
			is_admin INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO users (id, username, username_normalized, status, user_type, is_admin) VALUES
			('active-user', 'Active.User', 'active.user', 'active', 'webmail', 0),
			('disabled-user', 'Disabled.User', 'disabled.user', 'disabled', 'webmail', 0);
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

	var version, pending int
	var startedAt sql.NullTime
	var startedBy sql.NullString
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`
		SELECT deletion_pending, deletion_started_at, deletion_started_by
		FROM users WHERE id = 'disabled-user'`,
	).Scan(&pending, &startedAt, &startedBy); err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion || pending != 0 || startedAt.Valid || startedBy.Valid {
		t.Fatalf("migrated deletion state = version:%d pending:%d started:%v by:%v", version, pending, startedAt, startedBy)
	}
	if _, err := db.Write().Exec(`UPDATE users SET deletion_pending = 1 WHERE id = 'active-user'`); err == nil {
		t.Fatal("active user accepted pending deletion state")
	}
	if _, err := db.Write().Exec(`
		UPDATE users
		SET deletion_pending = 1, deletion_started_at = CURRENT_TIMESTAMP, deletion_started_by = 'active-user'
		WHERE id = 'disabled-user'`); err != nil {
		t.Fatalf("mark disabled webmail user deletion pending: %v", err)
	}
	if _, err := db.Write().Exec(`UPDATE users SET status = 'active' WHERE id = 'disabled-user'`); err == nil {
		t.Fatal("pending deletion user was re-enabled")
	}
}
