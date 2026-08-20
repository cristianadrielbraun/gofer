package storage

import (
	"path/filepath"
	"strings"
	"testing"
)

const usersV87TestTable = `CREATE TABLE users (
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
	user_type TEXT NOT NULL DEFAULT 'webmail',
	is_admin INTEGER NOT NULL DEFAULT 0,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

func seedV87Users(t *testing.T, path, users string, dependencies bool) {
	t.Helper()
	raw, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	statements := `
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (87);
	` + usersV87TestTable + `;
	` + users
	if dependencies {
		statements += `
			CREATE UNIQUE INDEX idx_users_email_normalized ON users(email_normalized);
			CREATE UNIQUE INDEX idx_users_username_normalized ON users(username_normalized) WHERE username_normalized IS NOT NULL;
			CREATE INDEX idx_users_status ON users(status);
			CREATE TABLE accounts (
				id TEXT PRIMARY KEY,
				user_id TEXT REFERENCES users(id) ON DELETE CASCADE,
				email_address TEXT NOT NULL
			);
			CREATE TABLE auth_identities (
				id TEXT PRIMARY KEY,
				user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				provider TEXT NOT NULL,
				issuer TEXT NOT NULL,
				subject TEXT NOT NULL
			);
			CREATE TABLE password_credentials (
				user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
				password_hash TEXT NOT NULL
			);
			CREATE TABLE sessions (
				id TEXT PRIMARY KEY,
				user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE
			);
			CREATE TABLE management_handoffs (
				id TEXT PRIMARY KEY,
				source_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				target_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE
			);
			CREATE TRIGGER accounts_management_owner_insert
			BEFORE INSERT ON accounts
			WHEN NEW.user_id IS NOT NULL
			 AND EXISTS (SELECT 1 FROM users WHERE id = NEW.user_id AND user_type != 'webmail')
			BEGIN SELECT RAISE(ABORT, 'management user cannot own a mailbox'); END;
			CREATE TRIGGER accounts_management_owner_update
			BEFORE UPDATE OF user_id ON accounts
			WHEN NEW.user_id IS NOT NULL
			 AND EXISTS (SELECT 1 FROM users WHERE id = NEW.user_id AND user_type != 'webmail')
			BEGIN SELECT RAISE(ABORT, 'management user cannot own a mailbox'); END;
			CREATE TRIGGER auth_identities_management_owner_insert
			BEFORE INSERT ON auth_identities
			WHEN EXISTS (SELECT 1 FROM users WHERE id = NEW.user_id AND user_type != 'webmail')
			BEGIN SELECT RAISE(ABORT, 'management user cannot own an application sign-in identity'); END;
			CREATE TRIGGER auth_identities_management_owner_update
			BEFORE UPDATE OF user_id ON auth_identities
			WHEN EXISTS (SELECT 1 FROM users WHERE id = NEW.user_id AND user_type != 'webmail')
			BEGIN SELECT RAISE(ABORT, 'management user cannot own an application sign-in identity'); END;
			INSERT INTO accounts (id, user_id, email_address)
			VALUES ('mailbox', 'mail-user', 'mailbox@example.com');
			INSERT INTO auth_identities (id, user_id, provider, issuer, subject)
			VALUES ('google-identity', 'mail-user', 'google', 'https://accounts.google.com', 'subject');
			INSERT INTO password_credentials (user_id, password_hash)
			VALUES ('mail-user', 'mail-password'), ('admin-user', 'admin-password');
			INSERT INTO sessions (id, user_id) VALUES ('mail-session', 'mail-user');
			INSERT INTO management_handoffs (id, source_user_id, target_user_id)
			VALUES ('handoff', 'mail-user', 'admin-user');
		`
	}
	if _, err := raw.Exec(statements); err != nil {
		t.Fatalf("seed v87 users: %v", err)
	}
}

func TestMigrateV87RemovesAccountEmailsAndPreservesUserDependencies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	seedV87Users(t, path, `
		INSERT INTO users (
			id, email, email_normalized, username, username_normalized, name, user_type, is_admin
		) VALUES
			('mail-user', 'mail-user@example.com', 'mail-user@example.com', 'Mail.User', 'mail.user', 'Mail User', 'webmail', 0),
			('admin-user', 'admin@example.com', 'admin@example.com', 'Admin.Owner', 'admin.owner', 'Admin Owner', 'management', 1);
	`, true)

	db, err := New(path)
	if err != nil {
		t.Fatalf("migrate v87 username-only users: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, removedColumn := range []string{"email", "email_normalized"} {
		if exists, err := columnExists(db.Read(), "users", removedColumn); err != nil || exists {
			t.Fatalf("users.%s exists=%t err=%v, want removed", removedColumn, exists, err)
		}
	}
	var version int
	var username, normalized string
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT username, username_normalized FROM users WHERE id = 'mail-user'`).Scan(&username, &normalized); err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion || username != "Mail.User" || normalized != "mail.user" {
		t.Fatalf("migrated user = version:%d username:%q normalized:%q", version, username, normalized)
	}
	for table, want := range map[string]int{
		"accounts": 1, "auth_identities": 1, "password_credentials": 2,
		"sessions": 1, "management_handoffs": 1,
	} {
		var count int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s rows = %d, %v; want %d", table, count, err, want)
		}
	}
	assertNoForeignKeyViolations(t, db.Read())
	assertExecFails(t, db.Write(), `
		INSERT INTO users (id, username, username_normalized, user_type, is_admin)
		VALUES ('duplicate', 'mail.user', 'mail.user', 'webmail', 0)`)
	assertExecFails(t, db.Write(), `
		INSERT INTO accounts (id, user_id, email_address)
		VALUES ('admin-mailbox', 'admin-user', 'admin-mailbox@example.com')`)
	assertExecFails(t, db.Write(), `
		INSERT INTO auth_identities (id, user_id, provider, issuer, subject)
		VALUES ('admin-identity', 'admin-user', 'google', 'https://accounts.google.com', 'admin-subject')`)
}

func TestMigrateV87RejectsUnsafeUsernamesWithoutChangingSchema(t *testing.T) {
	tests := []struct {
		name      string
		users     string
		wantError string
	}{
		{
			name:      "missing username",
			users:     `INSERT INTO users (id, email) VALUES ('owner', 'owner@example.com');`,
			wantError: "requires a valid username",
		},
		{
			name: "invalid username",
			users: `INSERT INTO users (id, email, username, username_normalized)
				VALUES ('owner', 'owner@example.com', 'owner@example.com', 'owner@example.com');`,
			wantError: "unsupported character",
		},
		{
			name: "normalized collision",
			users: `INSERT INTO users (id, email, username, username_normalized) VALUES
				('one', 'one@example.com', 'Person', 'person-one'),
				('two', 'two@example.com', 'PERSON', 'person-two');`,
			wantError: "conflicting usernames",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gofer.db")
			seedV87Users(t, path, test.users, false)
			if db, err := New(path); err == nil {
				_ = db.Close()
				t.Fatal("New() accepted an unsafe legacy username")
			} else if !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("New() error = %v, want %q", err, test.wantError)
			}
			raw, err := openDB(path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			var version int
			if err := raw.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 87 {
				t.Fatalf("failed migration version = %d, %v; want 87", version, err)
			}
			if exists, err := columnExists(raw, "users", "email"); err != nil || !exists {
				t.Fatalf("failed migration users.email exists=%t err=%v, want preserved", exists, err)
			}
		})
	}
}
