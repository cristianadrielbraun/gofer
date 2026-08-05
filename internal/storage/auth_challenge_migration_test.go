package storage

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func seedV79ChallengeSchema(t *testing.T, path string) {
	t.Helper()
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB() error = %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (79);
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
		INSERT INTO users (id, email, name) VALUES ('owner', 'owner@example.com', 'Owner');
	`); err != nil {
		t.Fatalf("seed v79 base schema: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(sessionsV79Table, "sessions")); err != nil {
		t.Fatalf("seed v79 session table: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(authChallengesV79Table, "auth_challenges")); err != nil {
		t.Fatalf("seed v79 challenge table: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO auth_challenges (
			id, user_id, challenge_hash, purpose, attempts, max_attempts, expires_at
		) VALUES (
			'legacy-challenge', 'owner', 'legacy-state-value', 'federated_login', 0, 3,
			datetime('now', '+10 minutes')
		)`); err != nil {
		t.Fatalf("seed v79 challenge: %v", err)
	}
}

func TestMigrateV79AddsOriginBindingAndInvalidatesLegacyChallenges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	seedV79ChallengeSchema(t, path)

	db, err := New(path)
	if err != nil {
		t.Fatalf("New() migration error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 80 {
		t.Fatalf("schema version = %d, %v; want 80", version, err)
	}
	var challengeHash, origin, userID string
	var nonceHash *string
	var consumed bool
	if err := db.Read().QueryRow(`
		SELECT challenge_hash, nonce_hash, origin, COALESCE(user_id, ''), consumed_at IS NOT NULL
		FROM auth_challenges WHERE id = 'legacy-challenge'`).Scan(&challengeHash, &nonceHash, &origin, &userID, &consumed); err != nil {
		t.Fatalf("query migrated challenge: %v", err)
	}
	if challengeHash != canonicalChallengeHash("legacy-state-value") || len(challengeHash) != 64 {
		t.Fatalf("migrated challenge hash = %q", challengeHash)
	}
	if nonceHash != nil || origin != "" || userID != "owner" || !consumed {
		t.Fatalf("migrated challenge = nonce:%v origin:%q user:%q consumed:%t", nonceHash, origin, userID, consumed)
	}
	assertNoForeignKeyViolations(t, db.Read())
	assertExecFails(t, db.Write(), `
		INSERT INTO auth_challenges (id, challenge_hash, purpose, origin, expires_at)
		VALUES ('invalid-hash', 'not-a-hash', 'login', 'https://gofer.example', datetime('now', '+1 minute'))`)
	assertExecFails(t, db.Write(), `
		INSERT INTO auth_challenges (id, challenge_hash, purpose, origin, expires_at)
		VALUES ('missing-origin', '`+strings.Repeat("a", 64)+`', 'login', '', datetime('now', '+1 minute'))`)
}

func TestMigrateV79ChallengeFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	seedV79ChallengeSchema(t, path)
	raw, err := openDB(path)
	if err != nil {
		t.Fatalf("open v79 database: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE auth_challenges_v79_old (id TEXT PRIMARY KEY)`); err != nil {
		_ = raw.Close()
		t.Fatalf("create conflicting migration table: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close v79 database: %v", err)
	}

	if db, err := New(path); err == nil {
		_ = db.Close()
		t.Fatal("New() accepted a conflicting challenge migration table")
	} else if !strings.Contains(err.Error(), "temporary authentication challenge migration table") {
		t.Fatalf("New() error = %v, want temporary-table detail", err)
	}
	raw, err = openDB(path)
	if err != nil {
		t.Fatalf("reopen rolled-back database: %v", err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 79 {
		t.Fatalf("rolled-back schema version = %d, %v; want 79", version, err)
	}
	if exists, err := columnExists(raw, "auth_challenges", "origin"); err != nil || exists {
		t.Fatalf("auth_challenges.origin after rollback = %t, %v; want absent", exists, err)
	}
}
