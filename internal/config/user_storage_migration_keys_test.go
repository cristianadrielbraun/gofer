package config

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserStorageMigrationPasswordsAuthenticatesAllRetainedConfigurations(t *testing.T) {
	for _, damaged := range []string{"", "imap", "smtp", "carddav", "caldav"} {
		t.Run("damaged-"+damaged, func(t *testing.T) {
			db, codec := newAccountStoreTestStore(t)
			seedAccountStoreTestUser(t, t.Context(), db)
			ciphertext, err := codec.encrypt("private-password")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,email_address,is_deleting,encrypted_password,encrypted_smtp_password) VALUES('mail','default','alice@example.com',1,?,?);
 INSERT INTO account_contact_sync_configs(account_id,user_id,enabled,encrypted_password) VALUES('mail','default',0,?);
 INSERT INTO account_caldav_configs(account_id,user_id,base_url,use_account_credentials,encrypted_password) VALUES('mail','default','https://fixture.invalid/calendar/',0,?);
 UPDATE users SET status='disabled',deletion_pending=1 WHERE id='default'`, ciphertext, ciphertext, ciphertext, ciphertext); err != nil {
				t.Fatal(err)
			}
			if damaged != "" {
				queries := map[string]string{"imap": `UPDATE accounts SET encrypted_password=x'00'`, "smtp": `UPDATE accounts SET encrypted_smtp_password=x'00'`, "carddav": `UPDATE account_contact_sync_configs SET encrypted_password=x'00'`, "caldav": `UPDATE account_caldav_configs SET encrypted_password=x'00'`}
				if _, err := db.Write().Exec(queries[damaged]); err != nil {
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
			key := []byte("0123456789abcdef0123456789abcdef")
			err = ValidateUserStorageMigrationPasswords(t.Context(), source, key)
			if (err == nil) != (damaged == "") || (err != nil && strings.Contains(err.Error(), "private-password")) {
				t.Fatal("password verification", err)
			}
			if err := ValidateUserStorageMigrationPasswords(t.Context(), source, []byte(strings.Repeat("x", 32))); err == nil {
				t.Fatal("wrong key accepted")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := ValidateUserStorageMigrationPasswords(ctx, source, key); err != context.Canceled {
				t.Fatal("cancellation ignored", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("source password changed", err)
			}
		})
	}
}
