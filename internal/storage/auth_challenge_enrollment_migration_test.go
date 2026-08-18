package storage

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateV83AddsFederatedEnrollmentChallengePurpose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (83);
		CREATE TABLE users (id TEXT PRIMARY KEY);
		CREATE TABLE sessions (id TEXT PRIMARY KEY, user_id TEXT REFERENCES users(id));
		INSERT INTO users (id) VALUES ('invitee');
	`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(fmt.Sprintf(authChallengesV83Table, "auth_challenges")); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		INSERT INTO auth_challenges (
			id, challenge_hash, nonce_hash, purpose, origin, expires_at
		) VALUES ('login-challenge', ?, ?, 'federated_link', 'https://gofer.example', '2026-08-16 10:00:00')`,
		strings.Repeat("a", 64), strings.Repeat("b", 64),
	); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := New(path)
	if err != nil {
		t.Fatalf("migrate v83 authentication challenges: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var version, preserved int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 85 {
		t.Fatalf("schema version = %d, %v; want 85", version, err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges WHERE id = 'login-challenge'`).Scan(&preserved); err != nil || preserved != 1 {
		t.Fatalf("preserved challenge count = %d, %v", preserved, err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO auth_challenges (
			id, user_id, challenge_hash, nonce_hash, purpose, origin, expires_at
		) VALUES ('enrollment-challenge', 'invitee', ?, ?, 'federated_enrollment', 'https://gofer.example', '2026-08-16 10:00:00')`,
		strings.Repeat("c", 64), strings.Repeat("d", 64),
	); err != nil {
		t.Fatalf("insert federated enrollment challenge: %v", err)
	}
	assertExecFails(t, db.Write(), `
		INSERT INTO auth_challenges (
			id, challenge_hash, purpose, origin, expires_at
		) VALUES ('invalid-challenge', '`+strings.Repeat("e", 64)+`', 'invalid', 'https://gofer.example', '2026-08-16 10:00:00')`)
	assertNoForeignKeyViolations(t, db.Read())
}

func TestMigrateV83FederatedEnrollmentChallengeRollsBackOnConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (83);
		CREATE TABLE users (id TEXT PRIMARY KEY);
		CREATE TABLE sessions (id TEXT PRIMARY KEY, user_id TEXT REFERENCES users(id));
	`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(fmt.Sprintf(authChallengesV83Table, "auth_challenges")); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE auth_challenges_v83_old (id TEXT PRIMARY KEY)`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	if db, err := New(path); err == nil || db != nil {
		if db != nil {
			_ = db.Close()
		}
		t.Fatalf("New() = %#v, %v; want migration conflict", db, err)
	}
	raw, err = openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 83 {
		t.Fatalf("rolled-back schema version = %d, %v; want 83", version, err)
	}
}
