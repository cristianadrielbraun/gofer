package storage

import (
	"path/filepath"
	"testing"
)

func TestMigrateV147ToV148KeepsExistingRequirementsOnTheApp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	// An installation from before a requirement could accept a passkey, with
	// one person an administrator already requires two-factor sign-in for.
	if _, err := db.Write().Exec(`
		ALTER TABLE users DROP COLUMN mfa_passkey_counts;
		INSERT INTO users (id, username, username_normalized, mfa_required) VALUES ('u1', 'Tomas', 'tomas', 1);
		DELETE FROM schema_version;
		INSERT INTO schema_version (version) VALUES (147);`); err != nil {
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
	var version, counts int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 148 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	if err := db.Read().QueryRow(`SELECT mfa_passkey_counts FROM users WHERE id = 'u1'`).Scan(&counts); err != nil || counts != 0 {
		t.Fatalf("existing requirement accepts a passkey = %d, %v", counts, err)
	}
	if _, err := db.Write().Exec(`UPDATE users SET mfa_passkey_counts = 2 WHERE id = 'u1'`); err == nil {
		t.Fatal("passkey second-step choice accepted a value other than 0 or 1")
	}
}
