package mailauth

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newMailboxCredentialTestDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, email, name) VALUES ('owner', 'owner@example.com', 'Owner');
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address) VALUES
			('gmail-one', 'owner', 'gmail', 'google-one', 'one@example.com'),
			('gmail-two', 'owner', 'gmail', 'google-two', 'two@example.com');
	`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSecureOAuthCredentialsEncryptsLegacyRowsAndRejectsWrongKey(t *testing.T) {
	ctx := context.Background()
	db := newMailboxCredentialTestDB(t)
	if _, err := db.Write().Exec(`
		INSERT INTO oauth_accounts (
			id, account_id, provider, provider_account_id, access_token, refresh_token, scopes
		) VALUES (
			'legacy-google', 'gmail-one', 'google', 'google-one',
			'legacy-access-secret', 'legacy-refresh-secret', 'mail contacts'
		)`); err != nil {
		t.Fatal(err)
	}
	manager := New(&Config{}, db, testMailboxCredentialKey)
	if err := manager.SecureOAuthCredentials(ctx); err != nil {
		t.Fatalf("SecureOAuthCredentials() error = %v", err)
	}
	stored := storedOAuthTokenRecord(
		t, manager, ctx, "gmail-one", providers.OAuthGoogle,
		"legacy-access-secret", "legacy-refresh-secret",
	)
	if stored.AccessToken != "legacy-access-secret" || stored.RefreshToken != "legacy-refresh-secret" {
		t.Fatalf("decrypted legacy credential = access:%q refresh:%q", stored.AccessToken, stored.RefreshToken)
	}
	if err := manager.SecureOAuthCredentials(ctx); err != nil {
		t.Fatalf("SecureOAuthCredentials(idempotent) error = %v", err)
	}
	wrongKey := []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	if err := New(&Config{}, db, wrongKey).SecureOAuthCredentials(ctx); err == nil || !strings.Contains(err.Error(), "authenticate mailbox credential") {
		t.Fatalf("SecureOAuthCredentials(wrong key) error = %v", err)
	}
}

func TestSecureOAuthCredentialsRollsBackEveryRowOnFailure(t *testing.T) {
	ctx := context.Background()
	db := newMailboxCredentialTestDB(t)
	if _, err := db.Write().Exec(`
		INSERT INTO oauth_accounts (
			id, account_id, provider, provider_account_id, access_token, refresh_token
		) VALUES
			('credential-one', 'gmail-one', 'google', 'google-one', 'access-one', 'refresh-one'),
			('credential-two', 'gmail-two', 'google', 'google-two', 'access-two', 'refresh-two');
		CREATE TRIGGER reject_second_credential_encryption
		BEFORE UPDATE OF key_version ON oauth_accounts
		WHEN NEW.id = 'credential-two'
		BEGIN
			SELECT RAISE(ABORT, 'forced credential encryption failure');
		END;
	`); err != nil {
		t.Fatal(err)
	}
	manager := New(&Config{}, db, testMailboxCredentialKey)
	if err := manager.SecureOAuthCredentials(ctx); err == nil {
		t.Fatal("SecureOAuthCredentials() error = nil")
	}
	rows, err := db.Read().Query(`
		SELECT access_token, refresh_token, access_token_ciphertext,
		       refresh_token_ciphertext, key_version
		FROM oauth_accounts ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var accessToken, refreshToken string
		var accessCiphertext, refreshCiphertext []byte
		var keyVersion sql.NullInt64
		if err := rows.Scan(&accessToken, &refreshToken, &accessCiphertext, &refreshCiphertext, &keyVersion); err != nil {
			t.Fatal(err)
		}
		if accessToken == "" || refreshToken == "" || accessCiphertext != nil || refreshCiphertext != nil || keyVersion.Valid {
			t.Fatalf("partially encrypted credential = access:%q refresh:%q access-cipher:%x refresh-cipher:%x key:%v",
				accessToken, refreshToken, accessCiphertext, refreshCiphertext, keyVersion)
		}
		count++
	}
	if err := rows.Err(); err != nil || count != 2 {
		t.Fatalf("rollback rows = %d, %v", count, err)
	}
}

func TestOAuthCredentialsAreExactPerMailboxAndContextBound(t *testing.T) {
	ctx := context.Background()
	db := newMailboxCredentialTestDB(t)
	manager := New(&Config{}, db, testMailboxCredentialKey)
	expiresAt := time.Now().Add(time.Hour)
	if err := manager.UpsertOAuthAccount(
		ctx, "gmail-one", providers.OAuthGoogle, "google-one",
		"access-one", "refresh-one", "Bearer", &expiresAt, "mail",
	); err != nil {
		t.Fatal(err)
	}
	if err := manager.UpsertOAuthAccount(
		ctx, "gmail-two", providers.OAuthGoogle, "google-two",
		"access-two", "refresh-two", "Bearer", &expiresAt, "mail",
	); err != nil {
		t.Fatal(err)
	}
	if token, err := manager.GetOAuthTokenForAccount(ctx, "gmail-one"); err != nil || token != "access-one" {
		t.Fatalf("gmail-one token = %q, %v", token, err)
	}
	if token, err := manager.GetOAuthTokenForAccount(ctx, "gmail-two"); err != nil || token != "access-two" {
		t.Fatalf("gmail-two token = %q, %v", token, err)
	}
	var firstCiphertext, secondCiphertext []byte
	if err := db.Read().QueryRow(`SELECT access_token_ciphertext FROM oauth_accounts WHERE account_id = 'gmail-one'`).Scan(&firstCiphertext); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT access_token_ciphertext FROM oauth_accounts WHERE account_id = 'gmail-two'`).Scan(&secondCiphertext); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`
		UPDATE oauth_accounts
		SET access_token_ciphertext = CASE account_id
			WHEN 'gmail-one' THEN ?
			WHEN 'gmail-two' THEN ?
		END`, secondCiphertext, firstCiphertext); err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []string{"gmail-one", "gmail-two"} {
		if token, err := manager.GetOAuthTokenForAccount(ctx, accountID); err == nil || token != "" || !strings.Contains(err.Error(), "authenticate mailbox credential") {
			t.Fatalf("context-bound token for %s = %q, %v", accountID, token, err)
		}
	}

	if _, err := db.Write().Exec(`UPDATE accounts SET provider_account_id = 'google-one' WHERE id = 'gmail-two'`); err != nil {
		t.Fatal(err)
	}
	if err := manager.UpsertOAuthAccount(
		ctx, "gmail-two", providers.OAuthGoogle, "google-one",
		"replacement", "replacement-refresh", "Bearer", &expiresAt, "mail",
	); err == nil || !strings.Contains(err.Error(), "already assigned") {
		t.Fatalf("provider identity reassignment error = %v", err)
	}
}

func TestOAuthCredentialOperationsRequireEncryptionKey(t *testing.T) {
	db := newMailboxCredentialTestDB(t)
	manager := New(&Config{}, db, nil)
	if err := manager.SecureOAuthCredentials(context.Background()); err == nil || !strings.Contains(err.Error(), "key is unavailable") {
		t.Fatalf("SecureOAuthCredentials(no key) error = %v", err)
	}
	if err := manager.UpsertOAuthAccount(
		context.Background(), "gmail-one", providers.OAuthGoogle, "google-one",
		"access", "refresh", "Bearer", nil, "mail",
	); err == nil || !strings.Contains(err.Error(), "key is unavailable") {
		t.Fatalf("UpsertOAuthAccount(no key) error = %v", err)
	}
	var credentials int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM oauth_accounts`).Scan(&credentials); err != nil || credentials != 0 {
		t.Fatalf("credentials after missing-key upsert = %d, %v", credentials, err)
	}
}
