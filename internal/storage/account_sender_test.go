package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func accountByID(t *testing.T, db *DB, id string) models.Account {
	t.Helper()
	accounts, err := db.GetAccounts(context.Background(), "default")
	if err != nil {
		t.Fatalf("GetAccounts() error = %v", err)
	}
	for _, account := range accounts {
		if account.ID == id {
			return account
		}
	}
	t.Fatalf("account %s not listed", id)
	return models.Account{}
}

func TestMigrateV143ToV144AddsSenderNameAndDefaultIdentity(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `
		ALTER TABLE accounts DROP COLUMN sender_name;
		ALTER TABLE accounts DROP COLUMN default_identity_id;
		DELETE FROM schema_version;
		INSERT INTO schema_version (version) VALUES (143);
		INSERT INTO accounts (id, user_id, email_address, display_name, provider) VALUES ('work', 'default', 'ana@example.com', 'Mailbox', 'imap');`); err != nil {
		t.Fatalf("set up schema 143: %v", err)
	}
	for range 2 {
		if err := migrateV143ToV144(db.Write()); err != nil {
			t.Fatalf("migrateV143ToV144() error = %v", err)
		}
	}
	account := accountByID(t, db, "work")
	if account.SenderName != "" || account.DefaultIdentityID != "" || account.FromName() != "Mailbox" {
		t.Fatalf("account after migration = %+v", account)
	}
}

func TestAccountSenderName(t *testing.T) {
	ctx := context.Background()
	db := newIdentitiesTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `UPDATE accounts SET display_name = 'Work mail' WHERE id = 'work'`); err != nil {
		t.Fatal(err)
	}
	if got := accountByID(t, db, "work").FromName(); got != "Work mail" {
		t.Fatalf("FromName() without a sender name = %q, want the account name", got)
	}

	if err := db.SetAccountSenderNameGuarded(ctx, "default", "work", "  Ana \n  Example ", nil); err != nil {
		t.Fatalf("set sender name: %v", err)
	}
	account := accountByID(t, db, "work")
	if account.Name != "Work mail" || account.SenderName != "Ana Example" || account.FromName() != "Ana Example" {
		t.Fatalf("account = %+v, FromName %q", account, account.FromName())
	}

	if err := db.SetAccountSenderNameGuarded(ctx, "default", "work", strings.Repeat("a", 300), nil); err != nil {
		t.Fatal(err)
	}
	if got := accountByID(t, db, "work").SenderName; len(got) != maxAccountSenderName {
		t.Fatalf("long sender name kept %d characters", len(got))
	}

	if err := db.SetAccountSenderNameGuarded(ctx, "default", "work", "   ", nil); err != nil {
		t.Fatal(err)
	}
	if got := accountByID(t, db, "work").FromName(); got != "Work mail" {
		t.Fatalf("FromName() after clearing = %q, want the account name", got)
	}

	if err := db.SetAccountSenderNameGuarded(ctx, "someone-else", "work", "Mallory", nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("other owner err = %v", err)
	}
	if err := db.SetAccountSenderNameGuarded(ctx, "default", "missing", "Ana", nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown account err = %v", err)
	}
}

func TestAccountDefaultIdentity(t *testing.T) {
	ctx := context.Background()
	db := newIdentitiesTestDB(t)
	sales, err := db.SaveAccountIdentityGuarded(ctx, "default", models.Identity{AccountID: "work", Name: "Ana Sales", Email: "sales@example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := accountByID(t, db, "work").DefaultIdentity(); found {
		t.Fatal("a new account starts from an identity")
	}

	if err := db.SetAccountDefaultIdentityGuarded(ctx, "default", "work", sales.ID, nil); err != nil {
		t.Fatalf("set default: %v", err)
	}
	if identity, found := accountByID(t, db, "work").DefaultIdentity(); !found || identity.ID != sales.ID {
		t.Fatalf("DefaultIdentity() = %+v, %v", identity, found)
	}

	// Another account's identity, an unknown one and another owner are refused.
	if err := db.SetAccountDefaultIdentityGuarded(ctx, "default", "home", sales.ID, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("other account's identity err = %v", err)
	}
	if err := db.SetAccountDefaultIdentityGuarded(ctx, "default", "work", "idn_missing", nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown identity err = %v", err)
	}
	if err := db.SetAccountDefaultIdentityGuarded(ctx, "someone-else", "work", "", nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("other owner err = %v", err)
	}
	if identity, _ := accountByID(t, db, "work").DefaultIdentity(); identity.ID != sales.ID {
		t.Fatalf("a refused change moved the default to %+v", identity)
	}

	// Back to the account's own address.
	if err := db.SetAccountDefaultIdentityGuarded(ctx, "default", "work", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, found := accountByID(t, db, "work").DefaultIdentity(); found {
		t.Fatal("still starts from an identity after choosing the account's own address")
	}

	// Deleting the default identity starts new messages from the account's
	// own address again.
	if err := db.SetAccountDefaultIdentityGuarded(ctx, "default", "work", sales.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAccountIdentityGuarded(ctx, "default", "work", sales.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := accountByID(t, db, "work").DefaultIdentityID; got != "" {
		t.Fatalf("default identity after deleting it = %q", got)
	}
}

func TestAccountDefaultIdentityGoneFromProvider(t *testing.T) {
	ctx := context.Background()
	db := newIdentitiesTestDB(t)
	reported := []models.Identity{{ProviderIdentityID: "p1", Name: "Ana", Email: "ana@team.example.net"}, {ProviderIdentityID: "p2", Email: "help@example.net"}}
	if err := db.ReplaceProviderIdentities(ctx, "home", reported); err != nil {
		t.Fatal(err)
	}
	identities, err := db.ListAccountIdentities(ctx, "default", "home")
	if err != nil || len(identities) != 2 {
		t.Fatalf("identities = %+v, %v", identities, err)
	}
	if err := db.SetAccountDefaultIdentityGuarded(ctx, "default", "home", identities[0].ID, nil); err != nil {
		t.Fatal(err)
	}
	// A refresh that still lists it keeps it.
	if err := db.ReplaceProviderIdentities(ctx, "home", reported); err != nil {
		t.Fatal(err)
	}
	if got := accountByID(t, db, "home").DefaultIdentityID; got != identities[0].ID {
		t.Fatalf("default after a refresh that kept it = %q", got)
	}
	if err := db.ReplaceProviderIdentities(ctx, "home", reported[1:]); err != nil {
		t.Fatal(err)
	}
	if got := accountByID(t, db, "home").DefaultIdentityID; got != "" {
		t.Fatalf("default after the provider dropped it = %q", got)
	}
}
