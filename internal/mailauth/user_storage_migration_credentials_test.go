package mailauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

type migrationCredentialFixture struct {
	source, central *storage.DB
	stores          *storage.UserStores
	routing         *storage.AccountRouting
	originals       map[string]legacyOAuthCredential
}

func TestUserStorageMigrationCredentialVerifierPreservesAllFormatsReadOnly(t *testing.T) {
	for _, damage := range []string{"", "ciphertext", "original-principal", "key-version"} {
		t.Run("damaged-"+damage, func(t *testing.T) {
			f := newMigrationCredentialFixture(t)
			queries := map[string]string{
				"ciphertext":         `UPDATE oauth_accounts SET access_token_ciphertext=x'00' WHERE account_id='disabled'`,
				"original-principal": `UPDATE oauth_accounts SET provider_account_id='changed-original-binding' WHERE account_id='deleting'`,
				"key-version":        `UPDATE oauth_accounts SET key_version=3 WHERE account_id='refresh-only'`,
			}
			if damage != "" {
				if _, err := f.source.Write().Exec(queries[damage]); err != nil {
					t.Fatal(err)
				}
			}
			path := f.source.Path()
			if err := f.source.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			source, err := storage.OpenReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			err = ValidateUserStorageMigrationCredentials(t.Context(), source, testMailboxCredentialKey)
			if (err == nil) != (damage == "") {
				t.Fatal("grant verification", err)
			}
			if err := ValidateUserStorageMigrationCredentials(t.Context(), source, []byte(strings.Repeat("x", 32))); err == nil {
				t.Fatal("wrong grant key accepted")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := ValidateUserStorageMigrationCredentials(ctx, source, testMailboxCredentialKey); !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation ignored", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("original grants rewritten during verification", err)
			}
		})
	}
}

func newMigrationCredentialFixture(t *testing.T) *migrationCredentialFixture {
	t.Helper()
	root := t.TempDir()
	source, err := storage.New(filepath.Join(root, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	central, err := storage.New(filepath.Join(root, "central.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close(); central.Close() })
	for _, owner := range []string{"alice", "bob", "disabled", "deleting"} {
		if _, err := source.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES(?,?,?)`, owner, owner, owner); err != nil {
			t.Fatal(err)
		}
	}
	codec := New(nil, source, testMailboxCredentialKey)
	f := &migrationCredentialFixture{source: source, central: central, originals: make(map[string]legacyOAuthCredential)}
	for _, row := range []struct {
		account, owner, provider string
		version                  int
		access, refresh          string
		legacyAAD                bool
	}{
		{"current", "alice", "google", 2, "current-access", "current-refresh", false},
		{"legacy-one", "alice", "google", 1, "v1-access", "v1-refresh", true},
		{"legacy-two", "alice", "microsoft", 1, "v1-two-access", "v1-two-refresh", false},
		{"plaintext", "bob", "microsoft", 0, "plain-access", "plain-refresh", false},
		{"refresh-only", "bob", "google", 2, "", "only-refresh", false},
		{"disabled", "disabled", "google", 2, "disabled-access", "disabled-refresh", false},
		{"deleting", "deleting", "google", 2, "deleting-access", "deleting-refresh", false},
		{"old-binding", "alice", "google", 2, "old-access", "old-refresh", false},
		{"empty-grant", "alice", "google", 0, "", "", false},
	} {
		provider := "gmail"
		if row.provider == "microsoft" {
			provider = "outlook"
		}
		identity := oauthCredentialContext{ID: "credential-" + row.account, AccountID: row.account, Provider: row.provider, ProviderAccountID: row.account + "-principal"}
		if _, err := source.Write().Exec(`INSERT INTO accounts(id,user_id,provider,provider_account_id,auth_method,email_address) VALUES(?,?,?,?, 'oauth2',?)`, row.account, row.owner, provider, identity.ProviderAccountID, row.account+"@mail.test"); err != nil {
			t.Fatal(err)
		}
		original := legacyOAuthCredential{Context: identity, AccessToken: row.access, RefreshToken: row.refresh}
		var version any
		plainAccess, plainRefresh := row.access, row.refresh
		if row.version != 0 {
			version = row.version
			original.KeyVersion = sql.NullInt64{Int64: int64(row.version), Valid: true}
			plainAccess, plainRefresh = "", ""
			for _, token := range []struct {
				kind, value string
				output      *[]byte
			}{{"access", row.access, &original.AccessCiphertext}, {"refresh", row.refresh, &original.RefreshCiphertext}} {
				if token.value == "" {
					continue
				}
				aad := oauthTokenAAD(identity, token.kind)
				if row.legacyAAD {
					aad = legacyOAuthTokenAAD(identity, token.kind)
				}
				*token.output = testOAuthTokenCiphertext(t, codec, token.value, row.version, aad)
			}
		}
		if _, err := source.Write().Exec(`INSERT INTO oauth_accounts(id,account_id,provider,provider_account_id,access_token,refresh_token,access_token_ciphertext,refresh_token_ciphertext,key_version,token_type,expires_at,scopes,created_at,updated_at)
 VALUES(?,?,?,?,?,?,?,?,?,'OriginalTokenType','2099-10-08T09:10:11.123400+03:30','recorded scopes only','2026-10-07T09:10:11.123400+03:30','2026-10-08T09:10:11.123400+03:30')`, identity.ID, row.account, row.provider, identity.ProviderAccountID, plainAccess, plainRefresh, original.AccessCiphertext, original.RefreshCiphertext, version); err != nil {
			t.Fatal(err)
		}
		f.originals[row.account] = original
	}
	if _, err := source.Write().Exec(`UPDATE users SET status='disabled' WHERE id='disabled'; UPDATE users SET status='disabled',deletion_pending=1 WHERE id='deleting'; UPDATE accounts SET is_deleting=1 WHERE id='deleting'; UPDATE accounts SET provider_account_id='new-principal' WHERE id='old-binding'`); err != nil {
		t.Fatal(err)
	}
	stores, err := storage.NewUserStores(central, storage.UserStoreOptions{MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	f.stores = stores
	t.Cleanup(func() { stores.Close(context.Background()) })
	f.routing, err = storage.NewAccountRouting(stores)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *migrationCredentialFixture) begin(t *testing.T) (*sql.Conn, *sql.Tx, []byte) {
	t.Helper()
	path := f.source.Path()
	if err := f.source.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := f.central.Write().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	if _, err := conn.ExecContext(t.Context(), `ATTACH DATABASE ? AS migration_source`, uri.String()); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		tx.Rollback()
		conn.ExecContext(context.Background(), `DETACH DATABASE migration_source`)
		conn.Close()
	})
	if _, err := tx.Exec(`INSERT INTO main.users SELECT * FROM migration_source.users;
 INSERT INTO main.gofer_account_directory(account_id,user_id,state) SELECT id,user_id,CASE WHEN is_deleting<>0 THEN 'deleting' ELSE 'active' END FROM migration_source.accounts`); err != nil {
		t.Fatal(err)
	}
	return conn, tx, before
}

func TestUserStorageMigrationCredentialsAuthenticateAndPreserveOriginalBindings(t *testing.T) {
	f := newMigrationCredentialFixture(t)
	if _, err := f.source.Write().Exec(`UPDATE oauth_accounts SET expires_at=NULL,scopes='' WHERE account_id='empty-grant'`); err != nil {
		t.Fatal(err)
	}
	conn, tx, before := f.begin(t)
	if err := ImportUserStorageMigrationCredentials(t.Context(), tx, testMailboxCredentialKey); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), `DETACH DATABASE migration_source`); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	codec := New(nil, nil, testMailboxCredentialKey)
	for account, original := range f.originals {
		var id, subject, access, refresh, scopes, granted string
		var expiry sql.NullString
		var accessBytes, refreshBytes []byte
		var version int
		if err := f.central.Read().QueryRow(`SELECT id,provider_account_id,access_token_ciphertext,refresh_token_ciphertext,key_version,CAST(expires_at AS TEXT),scopes,granted_scopes FROM gofer_mailbox_credentials WHERE account_id=?`, account).Scan(&id, &subject, &accessBytes, &refreshBytes, &version, &expiry, &scopes, &granted); err != nil {
			t.Fatal(err)
		}
		if id != original.Context.ID || subject != original.Context.ProviderAccountID || version != 2 || granted != scopes {
			t.Fatal("credential metadata or grants rewritten", account)
		}
		if account == "empty-grant" {
			if expiry.Valid || scopes != "" {
				t.Fatal("expiry or grant evidence invented for empty grant")
			}
		} else if !expiry.Valid || expiry.String != "2099-10-08T09:10:11.123400+03:30" || scopes != "recorded scopes only" {
			t.Fatal("original expiry or scope representation changed", account)
		}
		access, err := codec.decryptOAuthToken(original.Context, "access", accessBytes, version)
		if err == nil {
			refresh, err = codec.decryptOAuthToken(original.Context, "refresh", refreshBytes, version)
		}
		if err != nil || access != original.AccessToken || refresh != original.RefreshToken {
			t.Fatal("grant changed", account, err)
		}
		if original.KeyVersion.Int64 == 2 && (!bytes.Equal(accessBytes, original.AccessCiphertext) || !bytes.Equal(refreshBytes, original.RefreshCiphertext)) {
			t.Fatal("current ciphertext replaced", account)
		}
		if account == "refresh-only" && accessBytes != nil {
			t.Fatal("access token invented for refresh-only grant")
		}
	}
	var status string
	var deleting, guards int
	if err := f.central.Read().QueryRow(`SELECT status FROM users WHERE id='disabled'`).Scan(&status); err != nil || status != "disabled" {
		t.Fatal("disabled owner enabled", status, err)
	}
	if err := f.central.Read().QueryRow(`SELECT deletion_pending FROM users WHERE id='deleting'`).Scan(&deleting); err != nil || deleting != 1 {
		t.Fatal("owner deletion canceled", deleting, err)
	}
	if err := f.central.Read().QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='trigger' AND tbl_name='gofer_mailbox_credentials'`).Scan(&guards); err != nil || guards != 3 {
		t.Fatal("runtime guards missing", guards, err)
	}
	if _, err := f.central.Write().Exec(`UPDATE gofer_mailbox_credentials SET scopes='new scope' WHERE account_id='disabled'`); err == nil {
		t.Fatal("import left disabled owner grants writable")
	}
	if _, err := f.central.Write().Exec(`UPDATE gofer_mailbox_credentials SET user_id='bob' WHERE account_id='current'`); err == nil {
		t.Fatal("import left ownership mutable")
	}
	after, err := os.ReadFile(f.source.Path())
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("source mutated by credential import", err)
	}
}

func TestUserStorageMigrationCredentialsFailuresRestoreSavepoint(t *testing.T) {
	for _, failure := range []string{"wrong-key", "tampered", "late-tampered", "swapped-ciphertexts", "changed-credential-id", "changed-provider-subject", "unknown-key-version", "wrong-owner", "missing-route", "enabled-disabled-owner", "wrong-lifecycle", "existing-table", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			f := newMigrationCredentialFixture(t)
			if failure == "tampered" {
				if _, err := f.source.Write().Exec(`UPDATE oauth_accounts SET access_token_ciphertext=x'0200010203' WHERE account_id='current'`); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "unknown-key-version" {
				if _, err := f.source.Write().Exec(`UPDATE oauth_accounts SET key_version=99 WHERE account_id='current'`); err != nil {
					t.Fatal(err)
				}
			}
			for name, query := range map[string]string{
				"late-tampered":            `UPDATE oauth_accounts SET refresh_token_ciphertext=x'0200010203' WHERE account_id='refresh-only'`,
				"swapped-ciphertexts":      `UPDATE oauth_accounts SET access_token_ciphertext=(SELECT access_token_ciphertext FROM oauth_accounts WHERE account_id='disabled') WHERE account_id='current'`,
				"changed-credential-id":    `UPDATE oauth_accounts SET id='different-identity' WHERE account_id='current'`,
				"changed-provider-subject": `UPDATE oauth_accounts SET provider_account_id='different-principal' WHERE account_id='current'`,
			} {
				if failure == name {
					if _, err := f.source.Write().Exec(query); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, tx, _ := f.begin(t)
			queries := map[string]string{
				"wrong-owner":            `UPDATE gofer_account_directory SET user_id='bob' WHERE account_id='current'`,
				"missing-route":          `DELETE FROM gofer_account_directory WHERE account_id='current'`,
				"enabled-disabled-owner": `UPDATE users SET status='active' WHERE id='disabled'`,
				"wrong-lifecycle":        `UPDATE gofer_account_directory SET state='active' WHERE account_id='deleting'`,
				"existing-table":         userMailboxCredentialSchema + userMailboxCredentialGuards,
			}
			if query := queries[failure]; query != "" {
				// An immutable directory edit is itself rejected. Use a delete/insert
				// mismatch to exercise the trusted import's independent validation.
				if failure == "wrong-owner" {
					query = `DELETE FROM gofer_account_directory WHERE account_id='current'; INSERT INTO gofer_account_directory(account_id,user_id,state) VALUES('current','bob','active')`
				}
				if _, err := tx.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "existing-table" {
				row := f.originals["current"]
				if _, err := tx.Exec(`INSERT INTO gofer_mailbox_credentials(id,account_id,user_id,provider,provider_account_id,access_token_ciphertext,key_version) VALUES(?,?,'alice','google',?,?,2)`, row.Context.ID, row.Context.AccountID, row.Context.ProviderAccountID, row.AccessCiphertext); err != nil {
					t.Fatal(err)
				}
			}
			key := testMailboxCredentialKey
			if failure == "wrong-key" {
				key = []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure == "cancel" {
				cancel()
			}
			if err := ImportUserStorageMigrationCredentials(ctx, tx, key); err == nil {
				t.Fatal("unsafe credential migration accepted")
			}
			var tableCount int
			if err := tx.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='gofer_mailbox_credentials'`).Scan(&tableCount); err != nil {
				t.Fatal(err)
			}
			want := 0
			if failure == "existing-table" {
				want = 1
			}
			if tableCount != want {
				t.Fatal("failed import left partial credentials", tableCount, want)
			}
			if failure == "existing-table" {
				var ciphertext []byte
				if err := tx.QueryRow(`SELECT access_token_ciphertext FROM gofer_mailbox_credentials`).Scan(&ciphertext); err != nil || !bytes.Equal(ciphertext, f.originals["current"].AccessCiphertext) {
					t.Fatal("existing grant overwritten by import", err)
				}
			}
		})
	}
}

func TestUserStorageMigrationCredentialsRefreshOnlyGrantWorksThroughRuntime(t *testing.T) {
	f := newMigrationCredentialFixture(t)
	conn, tx, _ := f.begin(t)
	if err := ImportUserStorageMigrationCredentials(t.Context(), tx, testMailboxCredentialKey); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	conn.ExecContext(t.Context(), `DETACH DATABASE migration_source`)
	conn.Close()
	for _, account := range []string{"refresh-only", "old-binding"} {
		owner, subject := "bob", "refresh-only-principal"
		if account == "old-binding" {
			owner, subject = "alice", "new-principal"
		}
		lease, err := f.stores.Acquire(t.Context(), owner)
		if err != nil {
			t.Fatal(err)
		}
		_, err = lease.DB().Write().Exec(`INSERT INTO accounts(id,user_id,provider,provider_account_id,auth_method,email_address) VALUES(?,?,'gmail',?,'oauth2',?)`, account, owner, subject, account+"@mail.test")
		lease.Release()
		if err != nil {
			t.Fatal(err)
		}
	}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "only-refresh" {
			t.Error("original refresh grant not used", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"refreshed-access","refresh_token":"rotated-refresh","token_type":"Bearer","expires_in":3600}`)
	}))
	t.Cleanup(server.Close)
	cfg := &Config{GoogleClient: &oauth2.Config{ClientID: "test", Endpoint: oauth2.Endpoint{TokenURL: server.URL, AuthStyle: oauth2.AuthStyleInParams}}}
	work, cancel := context.WithCancel(t.Context())
	service, err := NewUserCredentials(work, cfg, f.routing, testMailboxCredentialKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); service.Wait() })
	if requests.Load() != 0 {
		t.Fatal("offline migration contacted provider")
	}
	if value, err := service.GetOAuthTokenForUser(t.Context(), "alice", "old-binding"); !errors.Is(err, sql.ErrNoRows) || value != "" || requests.Load() != 0 {
		t.Fatal("original stale grant adopted new principal", err)
	}
	if value, err := service.GetOAuthTokenForUser(t.Context(), "bob", "refresh-only"); err != nil || value != "refreshed-access" || requests.Load() != 1 {
		t.Fatal("imported refresh-only grant unusable", err, requests.Load())
	}
	var ciphertext []byte
	if err := f.central.Read().QueryRow(`SELECT access_token_ciphertext FROM gofer_mailbox_credentials WHERE account_id='refresh-only'`).Scan(&ciphertext); err != nil || len(ciphertext) == 0 || bytes.Contains(ciphertext, []byte("refreshed-access")) {
		t.Fatal("runtime refresh did not publish encrypted access", err)
	}
}

func TestUserStorageMigrationCredentialCodecRefusesMixedAndUnboundFormats(t *testing.T) {
	codec := New(nil, nil, testMailboxCredentialKey)
	for _, row := range []legacyOAuthCredential{
		{AccessToken: "secret", KeyVersion: sql.NullInt64{Int64: 2, Valid: true}},
		{AccessCiphertext: []byte("ciphertext-without-version")},
		{KeyVersion: sql.NullInt64{Int64: 99, Valid: true}},
	} {
		if _, _, err := migrationCredentialCiphertext(codec, row); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid format accepted or secret emitted", err)
		}
	}
}

func TestUserStorageMigrationCredentialsFinalVerifierChecksAllOriginalFormats(t *testing.T) {
	f := newMigrationCredentialFixture(t)
	_, tx, _ := f.begin(t)
	if err := ImportUserStorageMigrationCredentials(t.Context(), tx, testMailboxCredentialKey); err != nil {
		t.Fatal(err)
	}
	if err := VerifyUserStorageMigrationCredentials(t.Context(), tx, testMailboxCredentialKey); err != nil {
		t.Fatal("retained formats failed final verification", err)
	}
	if err := VerifyUserStorageMigrationCredentials(t.Context(), tx, []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")); err == nil {
		t.Fatal("wrong application key accepted")
	}
	if err := VerifyUserStorageMigrationCredentials(t.Context(), nil, testMailboxCredentialKey); err == nil {
		t.Fatal("missing transaction accepted")
	}
	if _, err := tx.Exec(`DROP TRIGGER gofer_mailbox_credential_owner_update`); err != nil {
		t.Fatal(err)
	}
	if err := VerifyUserStorageMigrationCredentials(t.Context(), tx, testMailboxCredentialKey); err == nil {
		t.Fatal("missing lifecycle guard accepted")
	}
}
