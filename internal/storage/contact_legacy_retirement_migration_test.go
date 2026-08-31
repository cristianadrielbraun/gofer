package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMigrateV91ToV92RetiresLegacyContactsAndPreservesCanonicalData(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "gofer.db")
	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("New() initial error = %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users (id, username, username_normalized, name)
		VALUES ('default', 'default', 'default', 'Default');
		INSERT INTO contact_profiles (id, user_id, display_name, primary_email, origin, sync_enabled)
		VALUES ('profile-1', 'default', 'Canonical Contact', 'canonical@example.com', 'manual', 1);
		INSERT INTO contact_cards (
			id, user_id, profile_id, kind, provider, account_id, address_book_id, remote_id, etag
		) VALUES (
			'card-1', 'default', 'profile-1', 'provider', 'carddav', 'account-1', 'book-1',
			'https://dav.example/contacts/1.vcf', '"etag-1"'
		);
		INSERT INTO contact_sync_memberships (id, user_id, profile_id, account_id, address_book_id)
		VALUES ('membership-1', 'default', 'profile-1', 'account-1', 'book-1');
		CREATE TABLE contacts (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			display_name TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE contact_emails (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			email TEXT NOT NULL DEFAULT '',
			normalized_email TEXT NOT NULL DEFAULT '',
			label TEXT NOT NULL DEFAULT '',
			is_primary INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE contact_sources (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			provider TEXT NOT NULL DEFAULT '',
			account_id TEXT NOT NULL DEFAULT '',
			remote_id TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE contact_save_targets (
			contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			target TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (contact_id, target)
		);
		INSERT INTO contacts (id, user_id, display_name)
		VALUES ('legacy-1', 'default', 'Legacy Duplicate');
		INSERT INTO contact_identities (user_id, profile_id, kind, normalized_value)
		VALUES ('default', 'profile-1', 'email', 'canonical@example.com');
		INSERT INTO contact_emails (id, user_id, contact_id, email, normalized_email, is_primary)
		VALUES ('legacy-email-1', 'default', 'legacy-1', 'canonical@example.com', 'canonical@example.com', 1);
		INSERT INTO contact_sources (id, user_id, contact_id, provider, account_id, remote_id)
		VALUES ('legacy-source-1', 'default', 'legacy-1', 'carddav', 'account-1', 'https://dav.example/contacts/1.vcf');
		INSERT INTO contact_save_targets (contact_id, user_id, target)
		VALUES ('legacy-1', 'default', 'account:account-1');
		DROP TABLE contact_sync_operations;
		CREATE TABLE contact_sync_operations (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			email TEXT NOT NULL DEFAULT '',
			payload_json TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			attempt_count INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			locked_at DATETIME,
			next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX idx_contact_sync_operations_due
		ON contact_sync_operations(status, next_attempt_at, locked_at);
		CREATE INDEX idx_contact_sync_operations_contact
		ON contact_sync_operations(user_id, contact_id, created_at DESC);
		INSERT INTO contact_sync_operations (
			id, user_id, contact_id, email, payload_json, status, attempt_count, last_error
		) VALUES (
			'operation-1', 'default', 'legacy-1', 'canonical@example.com', '{"kind":"upsert"}',
			'done', 2, 'transient failure'
		);
		DELETE FROM schema_version;
		INSERT INTO schema_version (version) VALUES (91)`); err != nil {
		_ = db.Close()
		t.Fatalf("prepare v91 database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() initial error = %v", err)
	}

	db, err = New(dbPath)
	if err != nil {
		t.Fatalf("New() after v92 migration error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var version int
	if err := db.Read().QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != 92 {
		t.Fatalf("schema version = %d, want 92", version)
	}
	for _, table := range []string{"contacts", "contact_emails", "contact_sources", "contact_save_targets"} {
		var count int
		if err := db.Read().QueryRowContext(ctx, `
			SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&count); err != nil {
			t.Fatalf("check retired table %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("retired table %s still exists", table)
		}
	}

	var profileCount, cardCount, membershipCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_profiles WHERE id = 'profile-1'`).Scan(&profileCount); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_cards WHERE id = 'card-1'`).Scan(&cardCount); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_sync_memberships WHERE id = 'membership-1'`).Scan(&membershipCount); err != nil {
		t.Fatal(err)
	}
	if profileCount != 1 || cardCount != 1 || membershipCount != 1 {
		t.Fatalf("canonical data counts = profile:%d card:%d membership:%d, want all 1", profileCount, cardCount, membershipCount)
	}

	var contactID, status, email, payload, lastError string
	var attempts int
	if err := db.Read().QueryRowContext(ctx, `
		SELECT contact_id, status, email, payload_json, attempt_count, last_error
		FROM contact_sync_operations WHERE id = 'operation-1'`,
	).Scan(&contactID, &status, &email, &payload, &attempts, &lastError); err != nil {
		t.Fatalf("load preserved sync operation: %v", err)
	}
	if contactID != "profile-1" || status != "done" || email != "canonical@example.com" || payload != `{"kind":"upsert"}` || attempts != 2 || lastError != "transient failure" {
		t.Fatalf("preserved sync operation = contact:%q status:%q email:%q payload:%q attempts:%d error:%q", contactID, status, email, payload, attempts, lastError)
	}
	var legacyForeignKeys int
	if err := db.Read().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pragma_foreign_key_list('contact_sync_operations') WHERE "table" = 'contacts'`,
	).Scan(&legacyForeignKeys); err != nil {
		t.Fatalf("inspect sync operation foreign keys: %v", err)
	}
	if legacyForeignKeys != 0 {
		t.Fatalf("contact_sync_operations legacy foreign keys = %d, want 0", legacyForeignKeys)
	}
	var foreignKeyViolations int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyViolations); err != nil {
		t.Fatalf("run foreign key check: %v", err)
	}
	if foreignKeyViolations != 0 {
		t.Fatalf("foreign key violations after migration = %d, want 0", foreignKeyViolations)
	}
}
