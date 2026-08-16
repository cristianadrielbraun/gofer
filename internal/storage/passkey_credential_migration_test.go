package storage

import (
	"path/filepath"
	"testing"
)

func TestMigrateV80AddsEncryptedPasskeyRecordsAndUserHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (80);
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
		CREATE TABLE webauthn_credentials (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			credential_id BLOB NOT NULL UNIQUE CHECK (length(credential_id) > 0),
			public_key BLOB NOT NULL CHECK (length(public_key) > 0),
			sign_count INTEGER NOT NULL DEFAULT 0 CHECK (sign_count >= 0),
			aaguid BLOB,
			transports TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(transports)),
			attachment TEXT NOT NULL DEFAULT '' CHECK (attachment IN ('', 'platform', 'cross-platform')),
			backup_eligible INTEGER NOT NULL DEFAULT 0 CHECK (backup_eligible IN (0, 1)),
			backup_state INTEGER NOT NULL DEFAULT 0 CHECK (backup_state IN (0, 1)),
			name TEXT NOT NULL CHECK (name <> ''),
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			last_used_at DATETIME,
			revoked_at DATETIME,
			CHECK (backup_state = 0 OR backup_eligible = 1)
		);
		INSERT INTO webauthn_credentials (id, user_id, credential_id, public_key, name)
		VALUES ('legacy-passkey', 'owner', x'01', x'02', 'Legacy passkey');
	`); err != nil {
		_ = raw.Close()
		t.Fatalf("seed v80 passkey schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := New(path)
	if err != nil {
		t.Fatalf("migrate v80 passkey schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var version int
	var ciphertext, keyVersion, rpID any
	var flags, cloneWarning int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 84 {
		t.Fatalf("schema version = %d, %v; want 84", version, err)
	}
	if err := db.Read().QueryRow(`
		SELECT credential_ciphertext, key_version, rp_id, flags, clone_warning
		FROM webauthn_credentials WHERE id = 'legacy-passkey'`,
	).Scan(&ciphertext, &keyVersion, &rpID, &flags, &cloneWarning); err != nil {
		t.Fatalf("query migrated passkey: %v", err)
	}
	if ciphertext != nil || keyVersion != nil || rpID != nil || flags != 0 || cloneWarning != 0 {
		t.Fatalf("legacy passkey migration = ciphertext:%v key:%v rp:%v flags:%d clone:%d",
			ciphertext, keyVersion, rpID, flags, cloneWarning)
	}
	if exists, err := tableExists(db.Read(), "webauthn_users"); err != nil || !exists {
		t.Fatalf("webauthn_users exists=%t err=%v", exists, err)
	}
	assertNoForeignKeyViolations(t, db.Read())
	assertExecFails(t, db.Write(), `
		INSERT INTO webauthn_users (user_id, rp_id, user_handle)
		VALUES ('owner', 'gofer.example', x'01')`)
}
