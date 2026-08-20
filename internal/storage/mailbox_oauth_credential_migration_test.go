package storage

import (
	"path/filepath"
	"strings"
	"testing"
)

func seedV85MailboxOAuthSchema(t *testing.T, path, accounts, credentials string) {
	t.Helper()
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (85);
		CREATE TABLE users (id TEXT PRIMARY KEY);
		INSERT INTO users (id) VALUES ('owner'), ('other');
		CREATE TABLE accounts (
			id TEXT PRIMARY KEY,
			user_id TEXT REFERENCES users(id) ON DELETE CASCADE,
			provider TEXT NOT NULL DEFAULT 'imap',
			provider_account_id TEXT NOT NULL DEFAULT '',
			email_address TEXT NOT NULL
		);
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
		CREATE UNIQUE INDEX idx_oauth_provider_account ON oauth_accounts(provider, provider_account_id);
	` + accounts + credentials); err != nil {
		_ = db.Close()
		t.Fatalf("seed v85 mailbox OAuth schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateV85MapsExactAndSingleUnboundMailboxCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	seedV85MailboxOAuthSchema(t, path, `
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address) VALUES
			('gmail-one', 'owner', 'gmail', 'google-one', 'one@example.com'),
			('gmail-two', 'owner', 'gmail', 'google-two', 'two@example.com'),
			('outlook-unbound', 'other', 'outlook', '', 'other@example.com');
	`, `
		INSERT INTO oauth_accounts (
			id, user_id, provider, provider_account_id, access_token, refresh_token, scopes
		) VALUES
			('google-one-token', 'owner', 'google', 'google-one', 'access-one', 'refresh-one', 'mail-one'),
			('google-two-token', 'owner', 'google', 'google-two', 'access-two', 'refresh-two', 'mail-two'),
			('microsoft-token', 'other', 'microsoft', 'microsoft-one', 'access-three', 'refresh-three', 'mail-three');
	`)

	db, err := New(path)
	if err != nil {
		t.Fatalf("migrate exact mailbox OAuth ownership: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != CurrentSchemaVersion {
		t.Fatalf("schema version = %d, %v; want %d", version, err, CurrentSchemaVersion)
	}
	for credentialID, accountID := range map[string]string{
		"google-one-token": "gmail-one",
		"google-two-token": "gmail-two",
		"microsoft-token":  "outlook-unbound",
	} {
		var linkedAccount, accessToken, refreshToken string
		var keyVersion any
		if err := db.Read().QueryRow(`
			SELECT account_id, access_token, refresh_token, key_version
			FROM oauth_accounts WHERE id = ?`, credentialID,
		).Scan(&linkedAccount, &accessToken, &refreshToken, &keyVersion); err != nil {
			t.Fatal(err)
		}
		if linkedAccount != accountID || accessToken == "" || refreshToken == "" || keyVersion != nil {
			t.Fatalf("migrated %s = account:%q access:%q refresh:%q key:%v", credentialID, linkedAccount, accessToken, refreshToken, keyVersion)
		}
	}
	var providerAccountID string
	if err := db.Read().QueryRow(`SELECT provider_account_id FROM accounts WHERE id = 'outlook-unbound'`).Scan(&providerAccountID); err != nil || providerAccountID != "microsoft-one" {
		t.Fatalf("backfilled Outlook identity = %q, %v", providerAccountID, err)
	}
	if exists, err := columnExists(db.Read(), "oauth_accounts", "user_id"); err != nil || exists {
		t.Fatalf("legacy oauth_accounts.user_id exists = %t, %v", exists, err)
	}
	if _, err := db.Write().Exec(`DELETE FROM accounts WHERE id = 'gmail-one'`); err != nil {
		t.Fatalf("delete exact mailbox account: %v", err)
	}
	var deletedCredential int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM oauth_accounts WHERE id = 'google-one-token'`).Scan(&deletedCredential); err != nil || deletedCredential != 0 {
		t.Fatalf("credential after mailbox deletion = %d, %v", deletedCredential, err)
	}
	assertNoForeignKeyViolations(t, db.Read())
}

func TestFreshAndMigratedMailboxOAuthSchemasMatch(t *testing.T) {
	fresh, err := New(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("New(fresh) error = %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close() })

	migratedPath := filepath.Join(t.TempDir(), "migrated.db")
	seedV85MailboxOAuthSchema(t, migratedPath, `
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address)
		VALUES ('gmail-one', 'owner', 'gmail', 'google-one', 'one@example.com');
	`, `
		INSERT INTO oauth_accounts (
			id, user_id, provider, provider_account_id, access_token, refresh_token
		) VALUES ('google-one-token', 'owner', 'google', 'google-one', 'access-one', 'refresh-one');
	`)
	migrated, err := New(migratedPath)
	if err != nil {
		t.Fatalf("New(migrated) error = %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })

	normalize := func(signature []string) string {
		joined := strings.Join(signature, "\n")
		return strings.ReplaceAll(joined, `CREATE TABLE "oauth_accounts"`, `CREATE TABLE oauth_accounts`)
	}
	freshSignature := normalize(authenticationTableSignature(t, fresh.Read(), "oauth_accounts"))
	migratedSignature := normalize(authenticationTableSignature(t, migrated.Read(), "oauth_accounts"))
	if freshSignature != migratedSignature {
		t.Fatalf("oauth_accounts schema mismatch\nfresh:\n%s\nmigrated:\n%s", freshSignature, migratedSignature)
	}
}

func TestMigrateV85RejectsAmbiguousMailboxCredentialMappingAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	seedV85MailboxOAuthSchema(t, path, `
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address) VALUES
			('gmail-one', 'owner', 'gmail', '', 'one@example.com'),
			('gmail-two', 'owner', 'gmail', '', 'two@example.com');
	`, `
		INSERT INTO oauth_accounts (
			id, user_id, provider, provider_account_id, access_token, refresh_token
		) VALUES ('ambiguous-token', 'owner', 'google', 'google-one', 'access-secret', 'refresh-secret');
	`)

	if db, err := New(path); err == nil || !strings.Contains(err.Error(), "ownership is ambiguous") {
		if db != nil {
			_ = db.Close()
		}
		t.Fatalf("ambiguous migration error = %v", err)
	}
	raw, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version int
	var accessToken, refreshToken string
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT access_token, refresh_token FROM oauth_accounts WHERE id = 'ambiguous-token'`).Scan(&accessToken, &refreshToken); err != nil {
		t.Fatal(err)
	}
	if version != 85 || accessToken != "access-secret" || refreshToken != "refresh-secret" {
		t.Fatalf("rolled-back migration = version:%d access:%q refresh:%q", version, accessToken, refreshToken)
	}
	if exists, err := columnExists(raw, "oauth_accounts", "account_id"); err != nil || exists {
		t.Fatalf("rolled-back account_id exists = %t, %v", exists, err)
	}
}
