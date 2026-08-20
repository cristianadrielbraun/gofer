package mailauth

import (
	"bytes"
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
		INSERT INTO users (id, username, username_normalized, name) VALUES ('owner', 'owner', 'owner', 'Owner');
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

func TestSecureOAuthCredentialsUpgradesBothVersionOneContextFormats(t *testing.T) {
	ctx := context.Background()
	db := newMailboxCredentialTestDB(t)
	manager := New(&Config{}, db, testMailboxCredentialKey)
	credentials := []struct {
		context      oauthCredentialContext
		accessToken  string
		refreshToken string
		legacyAAD    bool
	}{
		{
			context: oauthCredentialContext{
				ID: "version-one-delimited", AccountID: "gmail-one",
				Provider: providers.OAuthGoogle, ProviderAccountID: "google-one",
			},
			accessToken: "delimited-access", refreshToken: "delimited-refresh", legacyAAD: true,
		},
		{
			context: oauthCredentialContext{
				ID: "version-one-length-prefixed", AccountID: "gmail-two",
				Provider: providers.OAuthGoogle, ProviderAccountID: "google-two",
			},
			accessToken: "prefixed-access", refreshToken: "prefixed-refresh",
		},
	}
	originalCiphertext := map[string][]byte{}
	for _, credential := range credentials {
		accessAAD := oauthTokenAAD(credential.context, "access")
		refreshAAD := oauthTokenAAD(credential.context, "refresh")
		if credential.legacyAAD {
			accessAAD = legacyOAuthTokenAAD(credential.context, "access")
			refreshAAD = legacyOAuthTokenAAD(credential.context, "refresh")
		}
		accessCiphertext := testOAuthTokenCiphertext(
			t, manager, credential.accessToken,
			mailboxCredentialLegacyKeyVersion, accessAAD,
		)
		refreshCiphertext := testOAuthTokenCiphertext(
			t, manager, credential.refreshToken,
			mailboxCredentialLegacyKeyVersion, refreshAAD,
		)
		originalCiphertext[credential.context.ID] = append([]byte(nil), accessCiphertext...)
		if _, err := db.Write().Exec(`
			INSERT INTO oauth_accounts (
				id, account_id, provider, provider_account_id,
				access_token_ciphertext, refresh_token_ciphertext, key_version
			) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			credential.context.ID, credential.context.AccountID,
			credential.context.Provider, credential.context.ProviderAccountID,
			accessCiphertext, refreshCiphertext, mailboxCredentialLegacyKeyVersion,
		); err != nil {
			t.Fatal(err)
		}
	}

	if err := manager.SecureOAuthCredentials(ctx); err != nil {
		t.Fatalf("SecureOAuthCredentials(v1 upgrade) error = %v", err)
	}
	for _, credential := range credentials {
		var accessCiphertext []byte
		var keyVersion int
		if err := db.Read().QueryRow(`
			SELECT access_token_ciphertext, key_version
			FROM oauth_accounts WHERE id = ?`, credential.context.ID,
		).Scan(&accessCiphertext, &keyVersion); err != nil {
			t.Fatal(err)
		}
		if keyVersion != mailboxCredentialKeyVersion || bytes.Equal(accessCiphertext, originalCiphertext[credential.context.ID]) {
			t.Fatalf("upgraded %s = version:%d ciphertext-changed:%t", credential.context.ID, keyVersion, !bytes.Equal(accessCiphertext, originalCiphertext[credential.context.ID]))
		}
		stored := storedOAuthTokenRecord(
			t, manager, ctx, credential.context.AccountID, credential.context.Provider,
			credential.accessToken, credential.refreshToken,
		)
		if stored.AccessToken != credential.accessToken || stored.RefreshToken != credential.refreshToken {
			t.Fatalf("upgraded %s token = access:%q refresh:%q", credential.context.ID, stored.AccessToken, stored.RefreshToken)
		}
	}
	if err := manager.SecureOAuthCredentials(ctx); err != nil {
		t.Fatalf("SecureOAuthCredentials(v2 idempotent) error = %v", err)
	}
}

func TestSecureOAuthCredentialsRollsBackVersionOneUpgradeOnAuthenticationFailure(t *testing.T) {
	ctx := context.Background()
	db := newMailboxCredentialTestDB(t)
	manager := New(&Config{}, db, testMailboxCredentialKey)
	credentials := []oauthCredentialContext{
		{ID: "credential-one", AccountID: "gmail-one", Provider: providers.OAuthGoogle, ProviderAccountID: "google-one"},
		{ID: "credential-two", AccountID: "gmail-two", Provider: providers.OAuthGoogle, ProviderAccountID: "google-two"},
	}
	originalCiphertext := map[string][]byte{}
	for index, credential := range credentials {
		accessCiphertext := testOAuthTokenCiphertext(
			t, manager, "access-secret",
			mailboxCredentialLegacyKeyVersion, legacyOAuthTokenAAD(credential, "access"),
		)
		refreshCiphertext := testOAuthTokenCiphertext(
			t, manager, "refresh-secret",
			mailboxCredentialLegacyKeyVersion, legacyOAuthTokenAAD(credential, "refresh"),
		)
		if index == 1 {
			accessCiphertext[len(accessCiphertext)-1] ^= 0xff
		}
		originalCiphertext[credential.ID] = append([]byte(nil), accessCiphertext...)
		if _, err := db.Write().Exec(`
			INSERT INTO oauth_accounts (
				id, account_id, provider, provider_account_id,
				access_token_ciphertext, refresh_token_ciphertext, key_version
			) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			credential.ID, credential.AccountID, credential.Provider, credential.ProviderAccountID,
			accessCiphertext, refreshCiphertext, mailboxCredentialLegacyKeyVersion,
		); err != nil {
			t.Fatal(err)
		}
	}

	if err := manager.SecureOAuthCredentials(ctx); err == nil || !strings.Contains(err.Error(), "authenticate mailbox credential") {
		t.Fatalf("SecureOAuthCredentials(corrupt v1) error = %v", err)
	}
	for _, credential := range credentials {
		var accessCiphertext []byte
		var keyVersion int
		if err := db.Read().QueryRow(`
			SELECT access_token_ciphertext, key_version
			FROM oauth_accounts WHERE id = ?`, credential.ID,
		).Scan(&accessCiphertext, &keyVersion); err != nil {
			t.Fatal(err)
		}
		if keyVersion != mailboxCredentialLegacyKeyVersion || !bytes.Equal(accessCiphertext, originalCiphertext[credential.ID]) {
			t.Fatalf("rolled-back %s = version:%d ciphertext-preserved:%t", credential.ID, keyVersion, bytes.Equal(accessCiphertext, originalCiphertext[credential.ID]))
		}
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
