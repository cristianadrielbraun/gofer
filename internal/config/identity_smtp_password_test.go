package config

import (
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestIdentitySMTPPasswordsOnlyWorkForTheirIdentity(t *testing.T) {
	ctx := t.Context()
	db, store := newAccountStoreTestStore(t)
	seedAccountStoreTestUser(t, ctx, db)
	account, err := store.CreateAccount(ctx, "default", secureAccountStoreTestRequest("rosa@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	save := func(email string) models.Identity {
		identity, err := db.SaveAccountIdentityGuarded(ctx, "default", models.Identity{AccountID: account.ID, Email: email, SMTPHost: "smtp.example.com", SMTPPort: 465, SMTPTLSMode: "tls", SMTPUsername: "rosa"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return identity
	}
	shop, club := save("shop@example.com"), save("club@example.com")
	if err := store.SetIdentitySMTPPassword(ctx, "default", account.ID, shop.ID, "shop-smtp-secret", nil); err != nil {
		t.Fatal(err)
	}
	if password, err := store.DecryptIdentitySMTPPassword(ctx, account.ID, shop.ID); err != nil || password != "shop-smtp-secret" {
		t.Fatalf("DecryptIdentitySMTPPassword() = %q, %v", password, err)
	}
	if password, err := store.DecryptIdentitySMTPPassword(ctx, account.ID, club.ID); err != nil || password != "" {
		t.Fatalf("identity without a password = %q, %v", password, err)
	}
	if err := ValidateUserStorageMigrationPasswords(ctx, db, []byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatalf("migration check: %v", err)
	}

	// Copied to another identity, or read as the account's own sending
	// password, it does not decrypt.
	if _, err := db.Write().ExecContext(ctx, `UPDATE account_identities SET encrypted_smtp_password = (SELECT encrypted_smtp_password FROM account_identities WHERE id = ?) WHERE id = ?`, shop.ID, club.ID); err != nil {
		t.Fatal(err)
	}
	if password, err := store.DecryptIdentitySMTPPassword(ctx, account.ID, club.ID); err == nil {
		t.Fatalf("copied password decrypted for another identity: %q", password)
	}
	if err := ValidateUserStorageMigrationPasswords(ctx, db, []byte("0123456789abcdef0123456789abcdef")); err == nil {
		t.Fatal("migration check accepted a password it can't read")
	}
	if _, err := db.Write().ExecContext(ctx, `UPDATE accounts SET encrypted_smtp_password = (SELECT encrypted_smtp_password FROM account_identities WHERE id = ?) WHERE id = ?`, shop.ID, account.ID); err != nil {
		t.Fatal(err)
	}
	if password, err := store.DecryptSmtpPassword(ctx, account.ID); err == nil {
		t.Fatalf("identity password decrypted as the account's: %q", password)
	}

	// An empty password removes it.
	if err := store.SetIdentitySMTPPassword(ctx, "default", account.ID, shop.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if password, err := store.DecryptIdentitySMTPPassword(ctx, account.ID, shop.ID); err != nil || password != "" {
		t.Fatalf("removed password = %q, %v", password, err)
	}
}
