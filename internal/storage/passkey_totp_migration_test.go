package storage

import (
	"path/filepath"
	"testing"
)

func TestMigrateV146ToV147KeepsTheExtraCodeForExistingUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	// The shape of an installation from before the passkey preference, with a
	// user already signed up.
	if _, err := db.Write().Exec(`
		ALTER TABLE users DROP COLUMN passkey_totp_required;
		INSERT INTO users (id, username, username_normalized) VALUES ('u1', 'Marta', 'marta');
		DELETE FROM schema_version;
		INSERT INTO schema_version (version) VALUES (146);`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var version, required int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 147 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	if err := db.Read().QueryRow(`SELECT passkey_totp_required FROM users WHERE id = 'u1'`).Scan(&required); err != nil || required != 1 {
		t.Fatalf("existing user's passkey extra code = %d, %v", required, err)
	}
	if _, err := db.Write().Exec(`UPDATE users SET passkey_totp_required = 2 WHERE id = 'u1'`); err == nil {
		t.Fatal("passkey extra code accepted a value other than 0 or 1")
	}
}
