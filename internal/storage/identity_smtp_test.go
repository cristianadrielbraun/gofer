package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestMigrateV128ToV129AddsIdentitySMTP(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `
		ALTER TABLE account_identities DROP COLUMN smtp_host;
		ALTER TABLE account_identities DROP COLUMN smtp_port;
		ALTER TABLE account_identities DROP COLUMN smtp_tls_mode;
		ALTER TABLE account_identities DROP COLUMN smtp_username;
		ALTER TABLE account_identities DROP COLUMN encrypted_smtp_password;
		DELETE FROM schema_version;
		INSERT INTO schema_version (version) VALUES (128);
		INSERT INTO accounts (id, user_id, email_address, provider) VALUES ('work', 'default', 'ana@example.com', 'imap');
		INSERT INTO account_identities (id, account_id, email) VALUES ('one', 'work', 'sales@example.com');`); err != nil {
		t.Fatalf("set up schema 128: %v", err)
	}
	for range 2 {
		if err := migrateV128ToV129(db.Write()); err != nil {
			t.Fatalf("migrateV128ToV129() error = %v", err)
		}
	}
	identity, err := db.GetAccountIdentity(ctx, "default", "work", "one")
	if err != nil || identity.HasOwnSMTP() || identity.HasSMTPPassword {
		t.Fatalf("identity after migration = %+v, %v", identity, err)
	}
}

func TestIdentityOwnSMTP(t *testing.T) {
	ctx := context.Background()
	db := newIdentitiesTestDB(t)
	if _, err := db.Write().Exec(`
		INSERT INTO accounts (id, user_id, email_address, provider) VALUES ('mail', 'default', 'ana@example.org', 'gmail');
		INSERT INTO accounts (id, user_id, email_address, provider) VALUES ('office', 'default', 'ana@example.edu', 'outlook');`); err != nil {
		t.Fatal(err)
	}
	own := func(account, email string) models.Identity {
		return models.Identity{AccountID: account, Email: email, SMTPHost: " smtp.example.com ", SMTPPort: 465, SMTPTLSMode: "TLS", SMTPUsername: " ana "}
	}

	for _, bad := range []models.Identity{
		{AccountID: "work", Email: "a@example.com", SMTPHost: "smtp.example.com", SMTPPort: 0, SMTPTLSMode: "tls"},
		{AccountID: "work", Email: "a@example.com", SMTPHost: "smtp.example.com", SMTPPort: 465, SMTPTLSMode: "ssl"},
		{AccountID: "work", Email: "a@example.com", SMTPHost: "smtp.example.com/x", SMTPPort: 465, SMTPTLSMode: "tls"},
	} {
		if _, err := db.SaveAccountIdentityGuarded(ctx, "default", bad, nil); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if _, err := db.SaveAccountIdentityGuarded(ctx, "default", own("office", "shop@example.edu"), nil); !errors.Is(err, ErrIdentitySMTPNotSupported) {
		t.Fatalf("Outlook account err = %v", err)
	}

	// Gmail sends only its own aliases, but an address with its own server
	// needs no say from Gmail.
	shop, err := db.SaveAccountIdentityGuarded(ctx, "default", own("mail", "shop@example.org"), nil)
	if err != nil {
		t.Fatalf("Gmail account with own server: %v", err)
	}
	if shop.SMTPHost != "smtp.example.com" || shop.SMTPPort != 465 || shop.SMTPTLSMode != "tls" || shop.SMTPUsername != "ana" || shop.HasSMTPPassword {
		t.Fatalf("saved = %+v", shop)
	}
	if err := db.SetAccountIdentitySMTPPasswordGuarded(ctx, "default", "mail", shop.ID, []byte("sealed"), nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountIdentitySMTPPasswordGuarded(ctx, "intruder", "mail", shop.ID, []byte("sealed"), nil); err == nil {
		t.Fatal("another user set the password")
	}
	owner, sealed, err := db.AccountIdentitySMTPPassword(ctx, "mail", shop.ID)
	if err != nil || owner != "default" || string(sealed) != "sealed" {
		t.Fatalf("password = %q %q %v", owner, sealed, err)
	}

	// The password stays through other changes, and goes with a new server
	// or login.
	shop.Name, shop.SMTPPort = "Shop", 587
	shop.SMTPTLSMode = "starttls"
	if shop, err = db.SaveAccountIdentityGuarded(ctx, "default", shop, nil); err != nil || !shop.HasSMTPPassword {
		t.Fatalf("rename = %+v, %v", shop, err)
	}
	shop.SMTPHost = "mail.example.com"
	if shop, err = db.SaveAccountIdentityGuarded(ctx, "default", shop, nil); err != nil || shop.HasSMTPPassword {
		t.Fatalf("new server kept the password: %+v, %v", shop, err)
	}
	_ = db.SetAccountIdentitySMTPPasswordGuarded(ctx, "default", "mail", shop.ID, []byte("sealed"), nil)
	shop.SMTPUsername = "someone"
	if shop, err = db.SaveAccountIdentityGuarded(ctx, "default", shop, nil); err != nil || shop.HasSMTPPassword {
		t.Fatalf("new login kept the password: %+v, %v", shop, err)
	}

	// Back to the account's way of sending, which Gmail refuses for it.
	plain := shop
	plain.SMTPHost = ""
	if _, err := db.SaveAccountIdentityGuarded(ctx, "default", plain, nil); !errors.Is(err, ErrIdentityAddressNotAllowed) {
		t.Fatalf("drop own server on Gmail err = %v", err)
	}
	// Where the account allows it, the server's settings go with it.
	work, err := db.SaveAccountIdentityGuarded(ctx, "default", own("work", "shop@example.com"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SetAccountIdentitySMTPPasswordGuarded(ctx, "default", "work", work.ID, []byte("sealed"), nil)
	work.SMTPHost = ""
	if work, err = db.SaveAccountIdentityGuarded(ctx, "default", work, nil); err != nil || work.HasOwnSMTP() || work.SMTPPort != 0 || work.SMTPUsername != "" || work.HasSMTPPassword {
		t.Fatalf("drop own server = %+v, %v", work, err)
	}
	if err := db.SetAccountIdentitySMTPPasswordGuarded(ctx, "default", "work", work.ID, []byte("sealed"), nil); !errors.Is(err, ErrIdentitySMTPNotSupported) {
		t.Fatalf("password without a server err = %v", err)
	}

	// Provider identities send the provider's way.
	if err := db.ReplaceProviderIdentities(ctx, "home", []models.Identity{{ProviderIdentityID: "p1", Email: "news@example.net"}}); err != nil {
		t.Fatal(err)
	}
	list, _ := db.ListAccountIdentities(ctx, "default", "home")
	provider := list[0]
	provider.SMTPHost, provider.SMTPPort, provider.SMTPTLSMode = "smtp.example.net", 465, "tls"
	if _, err := db.SaveAccountIdentityGuarded(ctx, "default", provider, nil); !errors.Is(err, ErrIdentitySMTPNotSupported) {
		t.Fatalf("provider identity with own server err = %v", err)
	}
}
