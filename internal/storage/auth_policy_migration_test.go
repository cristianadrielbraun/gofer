package storage

import (
	"path/filepath"
	"testing"
)

func TestMigrateV81AddsSafeDefaultInstanceMFAPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (81);
		CREATE TABLE users (
			id TEXT PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active',
			auth_version INTEGER NOT NULL DEFAULT 1,
			is_admin INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO users (id, email, name, is_admin)
		VALUES ('owner', 'owner@example.com', 'Owner', 1);
		CREATE TABLE auth_system_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			initialized INTEGER NOT NULL DEFAULT 0 CHECK (initialized IN (0, 1)),
			owner_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
			initialized_at DATETIME,
			setup_token_hash TEXT,
			setup_expires_at DATETIME,
			setup_attempts INTEGER NOT NULL DEFAULT 0 CHECK (setup_attempts >= 0),
			setup_rotated_at DATETIME,
			cutover_version INTEGER NOT NULL DEFAULT 0 CHECK (cutover_version >= 0)
		);
		INSERT INTO auth_system_state (
			id, initialized, owner_user_id, initialized_at, setup_attempts,
			setup_rotated_at, cutover_version
		) VALUES (1, 1, 'owner', '2026-08-09 20:00:00', 3, '2026-08-09 19:00:00', 7);
	`); err != nil {
		_ = raw.Close()
		t.Fatalf("seed v81 authentication state: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := New(path)
	if err != nil {
		t.Fatalf("migrate v81 authentication state: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var version int
	var policy string
	var initialized, attempts, cutover int
	var owner string
	var initializedAt, rotatedAt any
	var updatedAt, updatedBy any
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 83 {
		t.Fatalf("schema version = %d, %v; want 83", version, err)
	}
	if err := db.Read().QueryRow(`
		SELECT initialized, owner_user_id, initialized_at, setup_attempts,
		       setup_rotated_at, cutover_version, mfa_policy,
		       security_policy_updated_at, security_policy_updated_by
		FROM auth_system_state WHERE id = 1`,
	).Scan(
		&initialized, &owner, &initializedAt, &attempts, &rotatedAt, &cutover,
		&policy, &updatedAt, &updatedBy,
	); err != nil {
		t.Fatal(err)
	}
	if initialized != 1 || owner != "owner" || initializedAt == nil || attempts != 3 ||
		rotatedAt == nil || cutover != 7 || policy != "administrators" || updatedAt != nil || updatedBy != nil {
		t.Fatalf("migrated state = initialized:%d owner:%q initialized_at:%v attempts:%d rotated_at:%v cutover:%d policy:%q updated_at:%v updated_by:%v",
			initialized, owner, initializedAt, attempts, rotatedAt, cutover, policy, updatedAt, updatedBy)
	}
	assertNoForeignKeyViolations(t, db.Read())
	assertExecFails(t, db.Write(), `UPDATE auth_system_state SET mfa_policy = 'invalid' WHERE id = 1`)
}

func TestMigrateV81RepairsPartialAuthenticationPolicyColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (81);
		CREATE TABLE users (id TEXT PRIMARY KEY);
		CREATE TABLE auth_system_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			mfa_policy TEXT NOT NULL DEFAULT 'administrators' CHECK (mfa_policy IN ('administrators', 'all_users'))
		);
	`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := New(path)
	if err != nil {
		t.Fatalf("repair partial v81 authentication policy schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, column := range []string{"mfa_policy", "security_policy_updated_at", "security_policy_updated_by"} {
		if exists, err := columnExists(db.Read(), "auth_system_state", column); err != nil || !exists {
			t.Fatalf("auth_system_state.%s exists=%t err=%v", column, exists, err)
		}
	}
}
