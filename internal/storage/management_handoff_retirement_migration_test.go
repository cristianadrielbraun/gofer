package storage

import (
	"path/filepath"
	"strings"
	"testing"
)

func seedV88HandoffDatabase(t *testing.T, path, handoffStatus string) {
	t.Helper()
	raw, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	completedAt := "NULL"
	if handoffStatus == "completed" {
		completedAt = "CURRENT_TIMESTAMP"
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (88);
		CREATE TABLE users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL,
			username_normalized TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			user_type TEXT NOT NULL,
			is_admin INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO users (id, username, username_normalized, user_type, is_admin) VALUES
			('webmail-user', 'webmail-user', 'webmail-user', 'webmail', 0),
			('management-owner', 'management-owner', 'management-owner', 'management', 1);
		CREATE TABLE management_handoffs (
			id TEXT PRIMARY KEY,
			source_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			target_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			status TEXT NOT NULL,
			completed_at DATETIME
		);
		INSERT INTO management_handoffs (id, source_user_id, target_user_id, status, completed_at)
		VALUES ('historical-handoff', 'webmail-user', 'management-owner', '` + handoffStatus + `', ` + completedAt + `);
	`); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateV88RetiresCompletedManagementHandoffWithoutChangingUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	seedV88HandoffDatabase(t, path, "completed")

	db, err := New(path)
	if err != nil {
		t.Fatalf("retire completed management handoff: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if exists, err := tableExists(db.Read(), "management_handoffs"); err != nil || exists {
		t.Fatalf("management_handoffs exists=%t, %v; want retired", exists, err)
	}
	var webmailType, managementType string
	var webmailAdmin, managementAdmin, version int
	if err := db.Read().QueryRow(`SELECT user_type, is_admin FROM users WHERE id = 'webmail-user'`).Scan(&webmailType, &webmailAdmin); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT user_type, is_admin FROM users WHERE id = 'management-owner'`).Scan(&managementType, &managementAdmin); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if webmailType != "webmail" || webmailAdmin != 0 || managementType != "management" || managementAdmin != 1 || version != CurrentSchemaVersion {
		t.Fatalf("retired roles = webmail:%s/%d management:%s/%d version:%d", webmailType, webmailAdmin, managementType, managementAdmin, version)
	}
	assertExecFails(t, db.Write(), `UPDATE users SET is_admin = 0 WHERE id = 'management-owner'`)
}

func TestMigrateV88RejectsIncompleteManagementHandoffWithoutDroppingState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	seedV88HandoffDatabase(t, path, "pending")

	if db, err := New(path); err == nil {
		_ = db.Close()
		t.Fatal("New() retired an incomplete management handoff")
	} else if !strings.Contains(err.Error(), "handoff(s) are incomplete") {
		t.Fatalf("New() error = %v", err)
	}

	raw, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version, handoffs int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM management_handoffs`).Scan(&handoffs); err != nil {
		t.Fatal(err)
	}
	if version != 88 || handoffs != 1 {
		t.Fatalf("failed retirement state = version:%d handoffs:%d, want 88/1", version, handoffs)
	}
}
