package storage

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func seedV78SessionSchema(t *testing.T, path, tokenHash string) {
	t.Helper()
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB() error = %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (78);
		CREATE TABLE users (
			id TEXT PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			email_normalized TEXT,
			username TEXT,
			username_normalized TEXT,
			name TEXT NOT NULL DEFAULT '',
			avatar_url TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active',
			auth_version INTEGER NOT NULL DEFAULT 1,
			mfa_required INTEGER NOT NULL DEFAULT 0,
			last_login_at DATETIME,
			disabled_at DATETIME,
			disabled_by TEXT REFERENCES users(id) ON DELETE SET NULL,
			is_admin INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`); err != nil {
		_ = db.Close()
		t.Fatalf("seed v78 users: %v", err)
	}
	for _, statement := range []string{
		fmt.Sprintf(sessionsV78Table, "sessions"),
		fmt.Sprintf(authChallengesV79Table, "auth_challenges"),
		fmt.Sprintf(authEventsV79Table, "auth_events"),
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("seed v78 authentication table: %v", err)
		}
	}
	if _, err := db.Exec(`
		INSERT INTO users (id, email, email_normalized, name, status, auth_version, is_admin)
		VALUES ('owner', 'owner@example.com', 'owner@example.com', 'Owner', 'active', 4, 1);
		INSERT INTO sessions (
			id, user_id, token, token_hash, auth_version, authentication_method, assurance_level,
			user_agent, expires_at, authenticated_at, last_used_at, idle_expires_at,
			absolute_expires_at, created_at
		) VALUES (
			'legacy-session', 'owner', 'raw-cookie-secret', ?, 4, 'legacy', 'legacy',
			'legacy-agent', '2026-09-03T10:30:00Z', '2026-08-04T10:30:00Z',
			'2026-08-04T10:30:00Z', NULL, NULL, '2026-08-04T10:30:00Z'
		);
		INSERT INTO auth_challenges (
			id, user_id, session_id, challenge_hash, purpose, expires_at
		) VALUES (
			'challenge', 'owner', 'legacy-session', 'challenge-hash', 'step_up', '2026-08-04T10:40:00Z'
		);
		INSERT INTO auth_events (
			id, actor_user_id, subject_user_id, session_id, event_type, success
		) VALUES (
			'event', 'owner', 'owner', 'legacy-session', 'login_succeeded', 1
		);
	`, tokenHash); err != nil {
		_ = db.Close()
		t.Fatalf("seed v78 session rows: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v78 database: %v", err)
	}
}

func TestMigrateV78RemovesRawSessionTokensAndPreservesReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	hash := sha256.Sum256([]byte("raw-cookie-secret"))
	wantHash := hex.EncodeToString(hash[:])
	seedV78SessionSchema(t, path, wantHash)

	db, err := New(path)
	if err != nil {
		t.Fatalf("New() migration error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 80 {
		t.Fatalf("schema version = %d, %v; want 80", version, err)
	}
	for _, column := range []string{"token", "expires_at"} {
		if exists, err := columnExists(db.Read(), "sessions", column); err != nil || exists {
			t.Fatalf("sessions.%s after migration = %t, %v; want absent", column, exists, err)
		}
	}
	var tokenHash, idleExpiresAt, absoluteExpiresAt string
	if err := db.Read().QueryRow(`
		SELECT token_hash, idle_expires_at, absolute_expires_at
		FROM sessions WHERE id = 'legacy-session'`).Scan(&tokenHash, &idleExpiresAt, &absoluteExpiresAt); err != nil {
		t.Fatalf("query migrated session: %v", err)
	}
	if tokenHash != wantHash || idleExpiresAt != absoluteExpiresAt || absoluteExpiresAt == "" {
		t.Fatalf("migrated session = hash:%q idle:%q absolute:%q", tokenHash, idleExpiresAt, absoluteExpiresAt)
	}
	for table, id := range map[string]string{"auth_challenges": "challenge", "auth_events": "event"} {
		var sessionID string
		if err := db.Read().QueryRow(`SELECT session_id FROM `+table+` WHERE id = ?`, id).Scan(&sessionID); err != nil {
			t.Fatalf("query preserved %s row: %v", table, err)
		}
		if sessionID != "legacy-session" {
			t.Fatalf("%s session_id = %q, want legacy-session", table, sessionID)
		}
	}
	assertSessionForeignKeyTargets(t, db.Read(), "auth_challenges")
	assertSessionForeignKeyTargets(t, db.Read(), "auth_events")
	assertNoForeignKeyViolations(t, db.Read())
}

func TestMigrateV78RollsBackInvalidSessionHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	seedV78SessionSchema(t, path, "not-a-canonical-session-hash")

	if db, err := New(path); err == nil {
		_ = db.Close()
		t.Fatal("New() accepted an invalid session hash")
	} else if !strings.Contains(err.Error(), "invalid token hash") {
		t.Fatalf("New() error = %v, want invalid token hash detail", err)
	}

	raw, err := openDB(path)
	if err != nil {
		t.Fatalf("reopen rolled-back database: %v", err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 78 {
		t.Fatalf("rolled-back schema version = %d, %v; want 78", version, err)
	}
	if exists, err := columnExists(raw, "sessions", "token"); err != nil || !exists {
		t.Fatalf("sessions.token after rollback = %t, %v; want present", exists, err)
	}
	var rawToken string
	if err := raw.QueryRow(`SELECT token FROM sessions WHERE id = 'legacy-session'`).Scan(&rawToken); err != nil || rawToken != "raw-cookie-secret" {
		t.Fatalf("raw token after rollback = %q, %v", rawToken, err)
	}
	for _, table := range []string{"sessions_auth_v78_old", "auth_challenges_v78_old", "auth_events_v78_old"} {
		if exists, err := tableExists(raw, table); err != nil || exists {
			t.Fatalf("temporary table %s after rollback = %t, %v; want absent", table, exists, err)
		}
	}
}

func TestMigrateV78RollsBackDuplicateSessionHashes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	hash := strings.Repeat("a", 64)
	seedV78SessionSchema(t, path, hash)
	raw, err := openDB(path)
	if err != nil {
		t.Fatalf("open duplicate database: %v", err)
	}
	if _, err := raw.Exec(`
		INSERT INTO sessions (
			id, user_id, token, token_hash, auth_version, authentication_method, assurance_level,
			user_agent, expires_at, authenticated_at, last_used_at, created_at
		) VALUES (
			'duplicate-session', 'owner', 'second-raw-token', ?, 4, 'legacy', 'legacy',
			'legacy-agent', '2026-09-03T10:30:00Z', '2026-08-04T10:30:00Z',
			'2026-08-04T10:30:00Z', '2026-08-04T10:30:00Z'
		)`, hash); err != nil {
		_ = raw.Close()
		t.Fatalf("insert duplicate hash: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close duplicate database: %v", err)
	}

	if db, err := New(path); err == nil {
		_ = db.Close()
		t.Fatal("New() accepted duplicate session hashes")
	} else if !strings.Contains(err.Error(), "token hash collision") {
		t.Fatalf("New() error = %v, want token hash collision detail", err)
	}
	raw, err = openDB(path)
	if err != nil {
		t.Fatalf("reopen duplicate database: %v", err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 78 {
		t.Fatalf("rolled-back duplicate version = %d, %v; want 78", version, err)
	}
	var count int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("sessions after duplicate rollback = %d, %v; want 2", count, err)
	}
}

func TestMigrateV78RollsBackMismatchedSessionHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	hash := sha256.Sum256([]byte("different-cookie-secret"))
	seedV78SessionSchema(t, path, hex.EncodeToString(hash[:]))

	if db, err := New(path); err == nil {
		_ = db.Close()
		t.Fatal("New() accepted a session hash that did not match its legacy token")
	} else if !strings.Contains(err.Error(), "does not match its legacy bearer token") {
		t.Fatalf("New() error = %v, want token/hash mismatch detail", err)
	}
	raw, err := openDB(path)
	if err != nil {
		t.Fatalf("reopen mismatch database: %v", err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 78 {
		t.Fatalf("rolled-back mismatch version = %d, %v; want 78", version, err)
	}
	if exists, err := columnExists(raw, "sessions", "token"); err != nil || !exists {
		t.Fatalf("sessions.token after mismatch rollback = %t, %v; want present", exists, err)
	}
}

func assertSessionForeignKeyTargets(t *testing.T, db queryer, table string) {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf(`PRAGMA foreign_key_list(%q)`, table))
	if err != nil {
		t.Fatalf("foreign_key_list(%s): %v", table, err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var id, seq int
		var targetTable, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &targetTable, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatalf("scan foreign_key_list(%s): %v", table, err)
		}
		if from == "session_id" {
			found = targetTable == "sessions"
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_list(%s) rows: %v", table, err)
	}
	if !found {
		t.Fatalf("%s.session_id does not reference sessions", table)
	}
}

type queryer interface {
	Query(string, ...any) (*sql.Rows, error)
}
