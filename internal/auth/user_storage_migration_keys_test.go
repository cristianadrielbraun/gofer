package auth

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/go-webauthn/webauthn/webauthn"
)

var authMigrationKey = []byte("0123456789abcdef0123456789abcdef")

func newAuthMigrationKeyFixture(t *testing.T) (*storage.DB, *Manager) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "shared.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized,status,deletion_pending) VALUES('alice','alice','alice','disabled',1),('bob','bob','bob','active',0)`); err != nil {
		t.Fatal(err)
	}
	return db, &Manager{bucketHashKey: authMigrationKey}
}

func TestUserStorageMigrationSecretsAuthenticatesCredentialsAndOriginalBindings(t *testing.T) {
	for _, damage := range []string{"", "totp-owner", "totp-version", "passkey-owner", "passkey-lookup", "passkey-version", "passkey-cipher-only", "passkey-version-only"} {
		t.Run("damaged-"+damage, func(t *testing.T) {
			db, codec := newAuthMigrationKeyFixture(t)
			seed, err := codec.encryptTOTPSeed("alice", "totp", "private-TOTP-seed")
			if err != nil {
				t.Fatal(err)
			}
			credential := webauthn.Credential{ID: []byte("credential-id"), PublicKey: []byte("public-key")}
			record, err := credential.MarshalMsg(nil)
			if err != nil {
				t.Fatal(err)
			}
			ciphertext, err := codec.encryptPasskeyCredential("alice", "passkey", record)
			if err != nil {
				t.Fatal(err)
			}
			if decoded, err := codec.decryptPasskeyCredential("alice", "passkey", ciphertext, 1); err != nil || validatePasskeyCredentialBinding(decoded, credential.ID, credential.PublicKey) != nil {
				t.Fatal("invalid passkey test record", err)
			}
			if _, err := db.Write().Exec(`INSERT INTO totp_credentials(id,user_id,encrypted_seed,key_version,revoked_at) VALUES('totp','alice',?,1,CURRENT_TIMESTAMP)`, seed); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Write().Exec(`INSERT INTO webauthn_credentials(id,user_id,credential_id,public_key,name,credential_ciphertext,key_version,revoked_at) VALUES('passkey','alice',?,?,'Revoked passkey',?,1,CURRENT_TIMESTAMP)`, credential.ID, credential.PublicKey, ciphertext); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Write().Exec(`INSERT INTO webauthn_credentials(id,user_id,credential_id,public_key,name) VALUES('legacy','bob',x'12',x'34','Legacy public metadata')`); err != nil {
				t.Fatal(err)
			}
			queries := map[string]string{
				"totp-owner": `UPDATE totp_credentials SET user_id='bob'`, "totp-version": `UPDATE totp_credentials SET key_version=2`,
				"passkey-owner": `UPDATE webauthn_credentials SET user_id='bob' WHERE id='passkey'`, "passkey-lookup": `UPDATE webauthn_credentials SET public_key=x'FF' WHERE id='passkey'`,
				"passkey-version": `UPDATE webauthn_credentials SET key_version=2 WHERE id='passkey'`, "passkey-cipher-only": `UPDATE webauthn_credentials SET key_version=NULL WHERE id='passkey'`,
				"passkey-version-only": `UPDATE webauthn_credentials SET credential_ciphertext=NULL WHERE id='passkey'`,
			}
			if damage != "" {
				if _, err := db.Write().Exec(queries[damage]); err != nil {
					t.Fatal(err)
				}
			}
			path := db.Path()
			if err := db.Close(); err != nil {
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
			err = ValidateUserStorageMigrationSecrets(t.Context(), source, authMigrationKey)
			if (err == nil) != (damage == "") {
				t.Fatal("credential verification", err)
			}
			if err := ValidateUserStorageMigrationSecrets(t.Context(), source, []byte(strings.Repeat("x", 32))); err == nil {
				t.Fatal("wrong key accepted")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := ValidateUserStorageMigrationSecrets(ctx, source, authMigrationKey); err != context.Canceled {
				t.Fatal("cancellation ignored", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("authentication source changed", err)
			}
		})
	}
}

func TestUserStorageMigrationSecretsAuthenticatesEveryChallengeCodecWithoutConsuming(t *testing.T) {
	type encrypt func(*Manager, *PreAuthChallenge) ([]byte, error)
	codecs := map[string]encrypt{
		"setup": func(m *Manager, c *PreAuthChallenge) ([]byte, error) {
			return m.encryptSetupOwnerDraft(c, &SetupOwnerDraft{TOTPSecret: "private-seed"})
		},
		"recovery": func(m *Manager, c *PreAuthChallenge) ([]byte, error) {
			return m.encryptRecoveryRepairDraft(c, &RecoveryRepairDraft{TOTPSecret: "private-seed"})
		},
		"passkey-registration": func(m *Manager, c *PreAuthChallenge) ([]byte, error) {
			return m.encryptPasskeyRegistrationDraft(c, &passkeyRegistrationDraft{Version: 1, Name: "Original"})
		},
		"passkey-assertion": func(m *Manager, c *PreAuthChallenge) ([]byte, error) {
			return m.encryptPasskeyAssertionDraft(c, &passkeyAssertionDraft{Version: 1, RPID: "fixture.invalid"})
		},
		"security": func(m *Manager, c *PreAuthChallenge) ([]byte, error) {
			return m.encryptSecurityManagementDraft(c, &securityManagementDraft{Kind: securityManagementKindTOTP, AuthVersion: 1, NewTOTP: "original-credential", TOTPSecret: "private-seed"})
		},
		"mfa": func(m *Manager, c *PreAuthChallenge) ([]byte, error) {
			return m.encryptMFAContinuationDraft(c, &mfaContinuationDraft{Version: 1, AuthVersion: 1, PrimaryMethod: AuthenticationMethodPassword, PrimaryAssurance: AssuranceLevelSingleFactor})
		},
		"google": func(m *Manager, c *PreAuthChallenge) ([]byte, error) {
			return m.encryptGoogleLoginDraft(c, &googleLoginDraft{Version: 1, CodeVerifier: strings.Repeat("a", 43)})
		},
		"microsoft": func(m *Manager, c *PreAuthChallenge) ([]byte, error) {
			return m.encryptMicrosoftLoginDraft(c, &microsoftLoginDraft{Version: 1, CodeVerifier: strings.Repeat("b", 43)})
		},
		"oidc": func(m *Manager, c *PreAuthChallenge) ([]byte, error) {
			return m.encryptOIDCLoginDraft(c, &oidcLoginDraft{Version: 1, CodeVerifier: strings.Repeat("c", 43)})
		},
	}
	for name, encrypt := range codecs {
		t.Run(name, func(t *testing.T) {
			db, codec := newAuthMigrationKeyFixture(t)
			challenge := &PreAuthChallenge{ID: "original", UserID: "alice", Purpose: ChallengePurposeEnrollment, Origin: "https://fixture.invalid"}
			payload, err := encrypt(codec, challenge)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Write().Exec(`INSERT INTO auth_challenges(id,user_id,challenge_hash,purpose,origin,payload_ciphertext,expires_at,consumed_at) VALUES(?,?,?,?,?,?, '2000-01-01',CURRENT_TIMESTAMP)`, challenge.ID, challenge.UserID, strings.Repeat("a", 64), challenge.Purpose, challenge.Origin, payload); err != nil {
				t.Fatal(err)
			}
			if err := ValidateUserStorageMigrationSecrets(t.Context(), db, authMigrationKey); err != nil {
				t.Fatal(err)
			}
			if err := ValidateUserStorageMigrationSecrets(t.Context(), db, []byte(strings.Repeat("x", 32))); err == nil {
				t.Fatal("wrong challenge key accepted")
			}
			var original []byte
			if err := db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id='original'`).Scan(&original); err != nil || !bytes.Equal(original, payload) {
				t.Fatal("challenge modified", err)
			}
			if _, err := db.Write().Exec(`UPDATE auth_challenges SET origin='https://changed.invalid'`); err != nil {
				t.Fatal(err)
			}
			if err := ValidateUserStorageMigrationSecrets(t.Context(), db, authMigrationKey); err == nil {
				t.Fatal("original origin binding lost")
			}
		})
	}
}
