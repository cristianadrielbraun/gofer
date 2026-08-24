package storage

import (
	"path/filepath"
	"testing"
)

func TestMigrateV86SeparatesManagementAndWebmailUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (86);
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
		CREATE TABLE accounts (
			id TEXT PRIMARY KEY,
			user_id TEXT REFERENCES users(id) ON DELETE CASCADE,
			email_address TEXT NOT NULL
		);
		CREATE TABLE auth_identities (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE
		);
		INSERT INTO users (id, email, email_normalized, username, username_normalized, is_admin) VALUES
			('management-owner', 'owner@example.com', 'owner@example.com', 'management-owner', 'management-owner', 1),
			('webmail-user', 'mail@example.com', 'mail@example.com', 'webmail-user', 'webmail-user', 0);
	`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := New(path)
	if err != nil {
		t.Fatalf("migrate v86 user separation: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for userID, wantType := range map[string]string{
		"management-owner": "management",
		"webmail-user":     "webmail",
	} {
		var gotType string
		if err := db.Read().QueryRow(`SELECT user_type FROM users WHERE id = ?`, userID).Scan(&gotType); err != nil {
			t.Fatalf("read %s user type: %v", userID, err)
		}
		if gotType != wantType {
			t.Fatalf("%s user type = %q, want %q", userID, gotType, wantType)
		}
	}
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != CurrentSchemaVersion {
		t.Fatalf("schema version = %d, %v; want %d", version, err, CurrentSchemaVersion)
	}
	assertNoForeignKeyViolations(t, db.Read())
}

func TestSeparatedUserSchemaRejectsNewMixedAccountsAndManagementMailboxes(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized, user_type, is_admin) VALUES
			('management-owner', 'management-owner', 'management-owner', 'management', 1),
			('webmail-user', 'webmail-user', 'webmail-user', 'webmail', 0),
			('identity-user', 'identity-user', 'identity-user', 'webmail', 0);
		INSERT INTO accounts (id, user_id, email_address)
		VALUES ('webmail-mailbox', 'webmail-user', 'mailbox@example.com');
		INSERT INTO auth_identities (id, user_id, provider, issuer, subject)
		VALUES ('webmail-identity', 'identity-user', 'google', 'https://accounts.google.com', 'identity-subject');
	`); err != nil {
		t.Fatal(err)
	}

	assertExecFails(t, db.Write(), `
		INSERT INTO users (id, username, username_normalized, user_type, is_admin)
		VALUES ('mixed', 'mixed', 'mixed', 'webmail', 1)`)
	assertExecFails(t, db.Write(), `UPDATE users SET is_admin = 1 WHERE id = 'webmail-user'`)
	assertExecFails(t, db.Write(), `UPDATE users SET is_admin = 0 WHERE id = 'management-owner'`)
	assertExecFails(t, db.Write(), `
		INSERT INTO users (id, username, username_normalized, user_type, is_admin)
		VALUES ('non-admin-management', 'non-admin-management', 'non-admin-management', 'management', 0)`)
	assertExecFails(t, db.Write(), `UPDATE users SET user_type = 'management' WHERE id = 'webmail-user'`)
	assertExecFails(t, db.Write(), `
		INSERT INTO accounts (id, user_id, email_address)
		VALUES ('management-mailbox', 'management-owner', 'owner-mailbox@example.com')`)
	assertExecFails(t, db.Write(), `UPDATE accounts SET user_id = 'management-owner' WHERE id = 'webmail-mailbox'`)
	assertExecFails(t, db.Write(), `
		INSERT INTO auth_identities (id, user_id, provider, issuer, subject)
		VALUES ('management-identity', 'management-owner', 'google', 'https://accounts.google.com', 'management-subject')`)
	assertExecFails(t, db.Write(), `UPDATE auth_identities SET user_id = 'management-owner' WHERE id = 'webmail-identity'`)
	assertExecFails(t, db.Write(), `UPDATE users SET user_type = 'management' WHERE id = 'identity-user'`)
}
