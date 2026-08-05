package storage

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

var authenticationSchemaTables = []string{
	"auth_challenges",
	"auth_events",
	"auth_identities",
	"auth_system_state",
	"auth_throttle",
	"password_credentials",
	"recovery_codes",
	"sessions",
	"totp_credentials",
	"user_enrollment_tokens",
	"webauthn_credentials",
}

func seedV77AuthenticationSchema(t *testing.T, path, token string) {
	t.Helper()
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB() error = %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (77);
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
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			token TEXT NOT NULL UNIQUE,
			user_agent TEXT NOT NULL DEFAULT '',
			expires_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX idx_sessions_user ON sessions(user_id);
		CREATE INDEX idx_sessions_token ON sessions(token);
		CREATE INDEX idx_sessions_expires ON sessions(expires_at);
		CREATE TABLE oauth_accounts (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			provider TEXT NOT NULL,
			provider_account_id TEXT NOT NULL,
			access_token TEXT NOT NULL DEFAULT '',
			refresh_token TEXT NOT NULL DEFAULT '',
			token_type TEXT NOT NULL DEFAULT 'Bearer',
			expires_at DATETIME,
			scopes TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO users (id, email, email_normalized, name, status, auth_version, is_admin)
		VALUES ('owner', 'owner@example.com', 'owner@example.com', 'Owner', 'active', 3, 1);
		INSERT INTO sessions (id, user_id, token, user_agent, expires_at, created_at)
		VALUES ('legacy-session', 'owner', ?, 'legacy-agent', '2026-09-03T10:30:00Z', '2026-08-04T10:30:00Z');
		INSERT INTO oauth_accounts (
			id, user_id, provider, provider_account_id, access_token, refresh_token, scopes
		) VALUES ('mailbox-token', 'owner', 'google', 'provider-subject', 'access-secret', 'refresh-secret', 'mail-scope');
	`, token); err != nil {
		_ = db.Close()
		t.Fatalf("seed v77 authentication schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v77 database: %v", err)
	}
}

func TestMigrateV77AddsAuthenticationSchemaAndPreservesSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	const rawToken = "legacy-session-secret"
	seedV77AuthenticationSchema(t, path, rawToken)

	db, err := New(path)
	if err != nil {
		t.Fatalf("New() migration error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 80 {
		t.Fatalf("schema version = %d, %v; want 80", version, err)
	}
	hash := sha256.Sum256([]byte(rawToken))
	wantHash := hex.EncodeToString(hash[:])
	var tokenHash, method, assurance string
	var authVersion int64
	var authenticatedAt, lastUsedAt, idleExpiresAt, absoluteExpiresAt, createdAt string
	if err := db.Read().QueryRow(`
		SELECT token_hash, auth_version, authentication_method, assurance_level,
		       authenticated_at, last_used_at, idle_expires_at, absolute_expires_at, created_at
		FROM sessions WHERE id = 'legacy-session'`).Scan(
		&tokenHash, &authVersion, &method, &assurance,
		&authenticatedAt, &lastUsedAt, &idleExpiresAt, &absoluteExpiresAt, &createdAt,
	); err != nil {
		t.Fatalf("query migrated session: %v", err)
	}
	if tokenHash != wantHash || authVersion != 3 || method != "legacy" || assurance != "legacy" {
		t.Fatalf("migrated session identity = hash:%q auth:%d method:%q assurance:%q", tokenHash, authVersion, method, assurance)
	}
	if authenticatedAt != createdAt || lastUsedAt != createdAt || idleExpiresAt != absoluteExpiresAt {
		t.Fatalf("migrated session times = authenticated:%q last-used:%q idle:%q absolute:%q created:%q", authenticatedAt, lastUsedAt, idleExpiresAt, absoluteExpiresAt, createdAt)
	}
	if exists, err := columnExists(db.Read(), "sessions", "token"); err != nil || exists {
		t.Fatalf("sessions.token after v79 migration = %t, %v; want absent", exists, err)
	}

	var accessToken, refreshToken, scopes string
	if err := db.Read().QueryRow(`
		SELECT access_token, refresh_token, scopes FROM oauth_accounts WHERE id = 'mailbox-token'`).Scan(
		&accessToken, &refreshToken, &scopes,
	); err != nil {
		t.Fatalf("query preserved mailbox credential: %v", err)
	}
	if accessToken != "access-secret" || refreshToken != "refresh-secret" || scopes != "mail-scope" {
		t.Fatalf("mailbox credential changed = access:%q refresh:%q scopes:%q", accessToken, refreshToken, scopes)
	}
	assertAuthenticationTablesExist(t, db.Read())
	assertNoForeignKeyViolations(t, db.Read())
}

func TestMigrateV77AcceptsLegacyV12AuthenticationTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB() error = %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (77);
		CREATE TABLE users (
			id TEXT PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			avatar_url TEXT NOT NULL DEFAULT '',
			is_admin INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			token TEXT NOT NULL UNIQUE,
			user_agent TEXT NOT NULL DEFAULT '',
			expires_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO users (id, email, name, is_admin) VALUES ('legacy-owner', 'owner@example.com', 'Owner', 1);
		INSERT INTO sessions (id, user_id, token, expires_at)
		VALUES ('legacy-session', 'legacy-owner', 'legacy-token', '2026-09-03T10:30:00Z');
	`); err != nil {
		_ = raw.Close()
		t.Fatalf("seed legacy authentication tables: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	db, err := New(path)
	if err != nil {
		t.Fatalf("New() legacy authentication migration error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var authVersion int64
	if err := db.Read().QueryRow(`SELECT auth_version FROM sessions WHERE id = 'legacy-session'`).Scan(&authVersion); err != nil {
		t.Fatalf("query migrated legacy session: %v", err)
	}
	if authVersion != 1 {
		t.Fatalf("legacy session auth version = %d, want 1", authVersion)
	}
	assertAuthenticationTablesExist(t, db.Read())
	assertNoForeignKeyViolations(t, db.Read())
}

func TestMigrateV77RollsBackInvalidLegacySession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	seedV77AuthenticationSchema(t, path, "")

	if db, err := New(path); err == nil {
		_ = db.Close()
		t.Fatal("New() accepted an empty legacy session token")
	} else if !strings.Contains(err.Error(), "empty bearer token") {
		t.Fatalf("New() error = %v, want empty bearer token detail", err)
	}

	raw, err := openDB(path)
	if err != nil {
		t.Fatalf("reopen rolled-back database: %v", err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 77 {
		t.Fatalf("rolled-back schema version = %d, %v; want 77", version, err)
	}
	if exists, err := tableExists(raw, "auth_identities"); err != nil || exists {
		t.Fatalf("auth_identities after rollback = %t, %v; want absent", exists, err)
	}
	if exists, err := columnExists(raw, "sessions", "token_hash"); err != nil || exists {
		t.Fatalf("sessions.token_hash after rollback = %t, %v; want absent", exists, err)
	}
	var token string
	if err := raw.QueryRow(`SELECT token FROM sessions WHERE id = 'legacy-session'`).Scan(&token); err != nil || token != "" {
		t.Fatalf("legacy session after rollback = %q, %v; want original empty token", token, err)
	}
}

func TestAuthenticationSchemaConstraints(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, email, email_normalized, name, status, is_admin)
		VALUES ('owner', 'owner@example.com', 'owner@example.com', 'Owner', 'active', 1)`); err != nil {
		t.Fatalf("insert owner: %v", err)
	}

	if _, err := db.Write().Exec(`
		INSERT INTO auth_identities (id, user_id, provider, issuer, subject)
		VALUES ('identity-one', 'owner', 'google', 'https://accounts.google.com', 'subject')`); err != nil {
		t.Fatalf("insert identity: %v", err)
	}
	assertExecFails(t, db.Write(), `
		INSERT INTO auth_identities (id, user_id, provider, issuer, subject)
		VALUES ('identity-two', 'owner', 'google', 'https://accounts.google.com', 'subject')`)
	assertExecFails(t, db.Write(), `
		INSERT INTO password_credentials (user_id, password_hash)
		VALUES ('missing-user', 'hash')`)
	assertExecFails(t, db.Write(), `
		INSERT INTO webauthn_credentials (
			id, user_id, credential_id, public_key, backup_eligible, backup_state, name
		) VALUES ('passkey', 'owner', x'01', x'02', 0, 1, 'Passkey')`)
	if _, err := db.Write().Exec(`
		INSERT INTO totp_credentials (id, user_id, encrypted_seed, key_version)
		VALUES ('totp-one', 'owner', x'01', 1)`); err != nil {
		t.Fatalf("insert TOTP credential: %v", err)
	}
	assertExecFails(t, db.Write(), `
		INSERT INTO totp_credentials (id, user_id, encrypted_seed, key_version)
		VALUES ('totp-two', 'owner', x'02', 1)`)
	assertExecFails(t, db.Write(), `
		INSERT INTO auth_challenges (id, challenge_hash, purpose, expires_at)
		VALUES ('challenge', 'hash', 'mailbox_oauth', CURRENT_TIMESTAMP)`)
	assertExecFails(t, db.Write(), `
		INSERT INTO user_enrollment_tokens (id, user_id, token_hash, purpose, expires_at)
		VALUES ('token', 'owner', 'hash', 'invitation_email', CURRENT_TIMESTAMP)`)
	assertExecFails(t, db.Write(), `INSERT INTO auth_system_state (id) VALUES (2)`)
	if _, err := db.Write().Exec(`
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method, assurance_level,
			authenticated_at, last_used_at, idle_expires_at, absolute_expires_at, created_at
		) VALUES (
			'session-one', 'owner', ?, 1, 'legacy', 'legacy',
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, datetime('now', '+1 day'), datetime('now', '+1 day'), CURRENT_TIMESTAMP
		)`, strings.Repeat("a", 64)); err != nil {
		t.Fatalf("insert session hash: %v", err)
	}
	assertExecFails(t, db.Write(), `
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method, assurance_level,
			authenticated_at, last_used_at, idle_expires_at, absolute_expires_at, created_at
		) VALUES (
			'session-two', 'owner', '`+strings.Repeat("a", 64)+`', 1, 'legacy', 'legacy',
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, datetime('now', '+1 day'), datetime('now', '+1 day'), CURRENT_TIMESTAMP
		)`)
	assertNoForeignKeyViolations(t, db.Read())
}

func TestFreshAndMigratedAuthenticationSchemasMatch(t *testing.T) {
	fresh, err := New(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("New(fresh) error = %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close() })

	migratedPath := filepath.Join(t.TempDir(), "migrated.db")
	seedV77AuthenticationSchema(t, migratedPath, "legacy-token")
	migrated, err := New(migratedPath)
	if err != nil {
		t.Fatalf("New(migrated) error = %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })

	for _, table := range authenticationSchemaTables {
		freshSignature := authenticationTableSignature(t, fresh.Read(), table)
		migratedSignature := authenticationTableSignature(t, migrated.Read(), table)
		if !reflect.DeepEqual(freshSignature, migratedSignature) {
			t.Fatalf("%s schema mismatch\nfresh:    %v\nmigrated: %v", table, freshSignature, migratedSignature)
		}
	}
}

func assertAuthenticationTablesExist(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range authenticationSchemaTables {
		if exists, err := tableExists(db, table); err != nil || !exists {
			t.Fatalf("table %s exists = %t, %v", table, exists, err)
		}
	}
}

func assertNoForeignKeyViolations(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check returned a violation")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_check rows: %v", err)
	}
}

func assertExecFails(t *testing.T, db *sql.DB, statement string) {
	t.Helper()
	if _, err := db.Exec(statement); err == nil {
		t.Fatalf("statement unexpectedly succeeded: %s", strings.Join(strings.Fields(statement), " "))
	}
}

func tableExists(db *sql.DB, table string) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count)
	return count == 1, err
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%q)`, table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func authenticationTableSignature(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	var signature []string
	var createSQL string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&createSQL); err != nil {
		t.Fatalf("table SQL for %s: %v", table, err)
	}
	normalizedSQL := strings.Join(strings.Fields(createSQL), " ")
	normalizedSQL = strings.Replace(normalizedSQL, "CREATE TABLE IF NOT EXISTS", "CREATE TABLE", 1)
	signature = append(signature, "sql:"+normalizedSQL)

	columnRows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%q)`, table))
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	for columnRows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := columnRows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			columnRows.Close()
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		signature = append(signature, fmt.Sprintf("column:%d:%s:%s:%d:%v:%d", cid, name, kind, notNull, defaultValue, primaryKey))
	}
	if err := columnRows.Close(); err != nil {
		t.Fatalf("close table_info(%s): %v", table, err)
	}

	foreignRows, err := db.Query(fmt.Sprintf(`PRAGMA foreign_key_list(%q)`, table))
	if err != nil {
		t.Fatalf("foreign_key_list(%s): %v", table, err)
	}
	for foreignRows.Next() {
		var id, seq int
		var targetTable, from, to, onUpdate, onDelete, match string
		if err := foreignRows.Scan(&id, &seq, &targetTable, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			foreignRows.Close()
			t.Fatalf("scan foreign_key_list(%s): %v", table, err)
		}
		signature = append(signature, fmt.Sprintf("foreign:%d:%d:%s:%s:%s:%s:%s:%s", id, seq, targetTable, from, to, onUpdate, onDelete, match))
	}
	if err := foreignRows.Close(); err != nil {
		t.Fatalf("close foreign_key_list(%s): %v", table, err)
	}

	indexRows, err := db.Query(fmt.Sprintf(`PRAGMA index_list(%q)`, table))
	if err != nil {
		t.Fatalf("index_list(%s): %v", table, err)
	}
	for indexRows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := indexRows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			indexRows.Close()
			t.Fatalf("scan index_list(%s): %v", table, err)
		}
		var columns []string
		columnIndexRows, err := db.Query(fmt.Sprintf(`PRAGMA index_info(%q)`, name))
		if err != nil {
			indexRows.Close()
			t.Fatalf("index_info(%s): %v", name, err)
		}
		for columnIndexRows.Next() {
			var columnSeq, cid int
			var columnName string
			if err := columnIndexRows.Scan(&columnSeq, &cid, &columnName); err != nil {
				columnIndexRows.Close()
				indexRows.Close()
				t.Fatalf("scan index_info(%s): %v", name, err)
			}
			columns = append(columns, fmt.Sprintf("%d:%d:%s", columnSeq, cid, columnName))
		}
		if err := columnIndexRows.Close(); err != nil {
			indexRows.Close()
			t.Fatalf("close index_info(%s): %v", name, err)
		}
		signature = append(signature, fmt.Sprintf("index:%s:%d:%s:%d:%s", name, unique, origin, partial, strings.Join(columns, ",")))
	}
	if err := indexRows.Close(); err != nil {
		t.Fatalf("close index_list(%s): %v", table, err)
	}
	sort.Strings(signature)
	return signature
}
