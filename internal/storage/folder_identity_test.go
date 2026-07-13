package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestFolderIDForIdentityIsStableAndCollisionProof(t *testing.T) {
	if got := FolderIDForIdentity("acc", "imap", "Projects/2026"); got == "" {
		t.Fatal("FolderIDForIdentity() returned an empty ID")
	}
	if got, want := FolderIDForIdentity("acc", "imap", "Projects/2026"), FolderIDForIdentity("acc", "imap", "Projects/2026"); got != want {
		t.Fatalf("same identity IDs differ: %q != %q", got, want)
	}
	variants := []string{"Projects/2026", "Projects.2026", "Projects 2026", "projects/2026", "Projekte/2026", "Projects/２０２６"}
	seen := make(map[string]string, len(variants))
	for _, remoteName := range variants {
		id := FolderIDForIdentity("acc", "imap", remoteName)
		if previous, exists := seen[id]; exists {
			t.Fatalf("remote names %q and %q collided at %q", previous, remoteName, id)
		}
		seen[id] = remoteName
	}
	if FolderIDForIdentity("acc", "imap", "Projects/2026") == FolderIDForIdentity("other", "imap", "Projects/2026") {
		t.Fatal("same remote identity in two accounts produced the same ID")
	}
	if FolderIDForIdentity("acc", "imap", "Projects/2026") == FolderIDForIdentity("acc", "gmail", "Projects/2026") {
		t.Fatal("same identity for different providers produced the same ID")
	}
	for _, empty := range [][3]string{{"", "imap", "INBOX"}, {"acc", "", "INBOX"}, {"acc", "imap", ""}} {
		if got := FolderIDForIdentity(empty[0], empty[1], empty[2]); got != "" {
			t.Fatalf("empty identity input %#v produced %q", empty, got)
		}
	}
}

func TestMigrateV65FolderIdentityRekeysReferencesAndAliases(t *testing.T) {
	path := seedV65FolderIdentityDB(t, false)
	db, err := New(path)
	if err != nil {
		t.Fatalf("New() migration error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	rootID := FolderIDForIdentity("imap-acc", "imap", "Projects/2026")
	childID := FolderIDForIdentity("imap-acc", "imap", "Projects.2026")
	gmailID := FolderIDForIdentity("gmail-acc", "gmail", "INBOX")

	var version int
	if err := db.Read().QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if version != 70 {
		t.Fatalf("schema version = %d, want 70", version)
	}
	for _, oldID := range []string{"legacy-root", "legacy-child", "gmail-inbox"} {
		var count int
		if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM folders WHERE id = ?`, oldID).Scan(&count); err != nil {
			t.Fatalf("query old folder %q: %v", oldID, err)
		}
		if count != 0 {
			t.Fatalf("old folder %q still exists", oldID)
		}
	}

	var parent string
	if err := db.Read().QueryRowContext(ctx, `SELECT COALESCE(parent_id, '') FROM folders WHERE id = ?`, childID).Scan(&parent); err != nil {
		t.Fatalf("query migrated parent: %v", err)
	}
	if parent != rootID {
		t.Fatalf("child parent = %q, want %q", parent, rootID)
	}
	var providerRemoteID string
	if err := db.Read().QueryRowContext(ctx, `SELECT provider_remote_id FROM folders WHERE id = ?`, gmailID).Scan(&providerRemoteID); err != nil {
		t.Fatalf("query Gmail folder: %v", err)
	}
	if providerRemoteID != "INBOX" {
		t.Fatalf("Gmail provider ID = %q, want INBOX", providerRemoteID)
	}

	checks := []struct {
		query string
		args  []any
		name  string
	}{
		{`SELECT COUNT(*) FROM message_folder_state WHERE folder_id = ?`, []any{childID}, "message membership"},
		{`SELECT COUNT(*) FROM folder_thread_state WHERE folder_id = ?`, []any{childID}, "thread state"},
		{`SELECT COUNT(*) FROM sync_state WHERE folder_id = ?`, []any{childID}, "sync state"},
		{`SELECT COUNT(*) FROM label_mutation_queue WHERE folder_id = ?`, []any{childID}, "label mutation"},
		{`SELECT COUNT(*) FROM message_mutations WHERE folder_id = ? AND destination_folder_id = ?`, []any{childID, rootID}, "message mutation"},
		{`SELECT COUNT(*) FROM imap_draft_states WHERE folder_id = ?`, []any{childID}, "draft state"},
	}
	for _, check := range checks {
		var count int
		if err := db.Read().QueryRowContext(ctx, check.query, check.args...).Scan(&count); err != nil {
			t.Fatalf("query %s: %v", check.name, err)
		}
		if count != 1 {
			t.Fatalf("%s rows = %d, want 1", check.name, count)
		}
	}

	var idleRaw, collapsedRaw string
	if err := db.Read().QueryRowContext(ctx, `SELECT value FROM app_settings WHERE user_id = 'owner' AND key = 'idle_folders'`).Scan(&idleRaw); err != nil {
		t.Fatalf("query idle settings: %v", err)
	}
	var idle map[string][]string
	if err := json.Unmarshal([]byte(idleRaw), &idle); err != nil {
		t.Fatalf("decode idle settings: %v", err)
	}
	if len(idle["imap-acc"]) != 1 || idle["imap-acc"][0] != childID {
		t.Fatalf("idle settings = %#v, want canonical child ID", idle)
	}
	if err := db.Read().QueryRowContext(ctx, `SELECT value FROM app_settings WHERE user_id = 'owner' AND key = 'sidebar_folder_collapsed'`).Scan(&collapsedRaw); err != nil {
		t.Fatalf("query sidebar settings: %v", err)
	}
	var collapsed map[string]bool
	if err := json.Unmarshal([]byte(collapsedRaw), &collapsed); err != nil {
		t.Fatalf("decode sidebar settings: %v", err)
	}
	if !collapsed["imap-acc:"+childID] {
		t.Fatalf("sidebar settings = %#v, want canonical child key", collapsed)
	}

	var aliasID string
	if err := db.Read().QueryRowContext(ctx, `SELECT new_id FROM folder_id_aliases WHERE old_id = 'legacy-child'`).Scan(&aliasID); err != nil {
		t.Fatalf("query folder alias: %v", err)
	}
	if aliasID != childID {
		t.Fatalf("legacy-child alias = %q, want %q", aliasID, childID)
	}
	resolved, err := db.ResolveFolderIDForUser(ctx, "owner", "legacy-child")
	if err != nil || resolved != childID {
		t.Fatalf("ResolveFolderIDForUser(owner, alias) = %q, %v; want %q", resolved, err, childID)
	}
	if _, err := db.ResolveFolderIDForUser(ctx, "intruder", "legacy-child"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ResolveFolderIDForUser(intruder, alias) error = %v, want sql.ErrNoRows", err)
	}
	resolved, err = db.ResolveFolderIDForUser(ctx, "owner", childID)
	if err != nil || resolved != childID {
		t.Fatalf("ResolveFolderIDForUser(owner, canonical) = %q, %v; want %q", resolved, err, childID)
	}

	rows, err := db.Read().QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check returned a violation after folder migration")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_check rows: %v", err)
	}

	var providerIndexUnique int
	indexRows, err := db.Read().QueryContext(ctx, `PRAGMA index_list(folders)`)
	if err != nil {
		t.Fatalf("folder index list: %v", err)
	}
	for indexRows.Next() {
		var seq int
		var name string
		var unique, partial int
		var origin string
		if err := indexRows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			indexRows.Close()
			t.Fatalf("scan folder index: %v", err)
		}
		if name == "idx_folders_account_provider_remote" {
			providerIndexUnique = unique
		}
	}
	if err := indexRows.Close(); err != nil {
		t.Fatalf("close folder index rows: %v", err)
	}
	if providerIndexUnique != 1 {
		t.Fatalf("provider identity index unique = %d, want 1", providerIndexUnique)
	}
}

func TestMigrateV65FolderIdentityRejectsDuplicateProviderIdentity(t *testing.T) {
	path := seedV65FolderIdentityDB(t, true)
	if _, err := New(path); err == nil || !strings.Contains(err.Error(), "same provider identity") {
		t.Fatalf("New() duplicate identity error = %v, want diagnostic", err)
	}

	raw, err := openDB(path)
	if err != nil {
		t.Fatalf("reopen duplicate database: %v", err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("query failed migration version: %v", err)
	}
	if version != 65 {
		t.Fatalf("failed migration version = %d, want rollback to 65", version)
	}
}

func seedV65FolderIdentityDB(t *testing.T, duplicate bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gofer.db")
	initial, err := New(path)
	if err != nil {
		t.Fatalf("create current database: %v", err)
	}
	if err := initial.Close(); err != nil {
		t.Fatalf("close current database: %v", err)
	}
	raw, err := openDB(path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`
		DROP INDEX IF EXISTS idx_folders_account_provider_remote;
		DROP INDEX IF EXISTS idx_folders_account_remote;
		DROP TABLE IF EXISTS folder_id_aliases;
		CREATE INDEX idx_folders_account_provider_remote ON folders(account_id, provider_remote_id);
		UPDATE schema_version SET version = 65;
		INSERT INTO users (id, email, name) VALUES ('owner', 'owner@example.com', 'Owner');
		INSERT INTO accounts (id, user_id, provider, email_address) VALUES
			('imap-acc', 'owner', 'imap', 'imap@example.com'),
			('gmail-acc', 'owner', 'gmail', 'gmail@example.com');
		INSERT INTO folders (id, account_id, parent_id, remote_id, provider_remote_id, name, role)
		VALUES
			('legacy-root', 'imap-acc', NULL, 'Projects/2026', '', 'Projects', 'custom'),
			('legacy-child', 'imap-acc', 'legacy-root', 'Projects.2026', '', 'Projects.2026', 'custom'),
			('gmail-inbox', 'gmail-acc', NULL, 'INBOX', 'INBOX', 'Inbox', 'inbox');
		INSERT INTO messages (id, account_id, internet_message_id, subject, from_email)
		VALUES (9001, 'imap-acc', '<migration@example.com>', 'Migration', 'sender@example.com');
		INSERT INTO message_folder_state (message_id, folder_id, remote_uid)
		VALUES (9001, 'legacy-child', 42);
		INSERT INTO folder_thread_state (folder_id, thread_key, head_message_id, account_id)
		VALUES ('legacy-child', 'thread:9001', 9001, 'imap-acc');
		INSERT INTO sync_state (account_id, folder_id, cursor)
		VALUES ('imap-acc', 'legacy-child', 'cursor');
		INSERT INTO label_mutation_queue (account_id, message_id, folder_id, provider_type, operation, label_name)
		VALUES ('imap-acc', 9001, 'legacy-child', 'imap', 'add', 'Projects');
		INSERT INTO message_mutations (id, account_id, message_id, folder_id, provider_type, kind, target_value, destination_folder_id)
		VALUES ('migration-move', 'imap-acc', 9001, 'legacy-child', 'imap', 'move', 1, 'legacy-root');
		INSERT INTO imap_draft_states (account_id, draft_key, local_message_id, folder_id, folder_remote_name)
		VALUES ('imap-acc', 'draft-1', 9001, 'legacy-child', 'Projects.2026');
		INSERT INTO app_settings (user_id, key, value) VALUES
			('owner', 'idle_folders', '{"imap-acc":["legacy-child"]}'),
			('owner', 'sidebar_folder_collapsed', '{"imap-acc:legacy-child":true}');
	`); err != nil {
		t.Fatalf("seed v65 folder database: %v", err)
	}
	if duplicate {
		if _, err := raw.Exec(`INSERT INTO folders (id, account_id, remote_id, provider_remote_id, name, role) VALUES ('legacy-duplicate', 'imap-acc', 'Projects/2026', '', 'Duplicate', 'custom')`); err != nil {
			t.Fatalf("seed duplicate identity: %v", err)
		}
	}
	return path
}
