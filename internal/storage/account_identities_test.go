package storage

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func newIdentitiesTestDB(t *testing.T) *DB {
	t.Helper()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(context.Background(), `
		INSERT INTO accounts (id, user_id, email_address, provider, sort_order) VALUES ('work', 'default', 'ana@example.com', 'imap', 0);
		INSERT INTO accounts (id, user_id, email_address, provider, sort_order) VALUES ('home', 'default', 'ana@example.net', 'jmap', 1);`); err != nil {
		t.Fatalf("insert accounts: %v", err)
	}
	return db
}

func TestMigrateV127ToV128AddsAccountIdentities(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `
		DROP TABLE account_identities;
		DELETE FROM schema_version;
		INSERT INTO schema_version (version) VALUES (127);
		INSERT INTO accounts (id, user_id, email_address, provider) VALUES ('work', 'default', 'ana@example.com', 'imap');`); err != nil {
		t.Fatalf("set up schema 127: %v", err)
	}
	if err := migrateV127ToV128(db.Write()); err != nil {
		t.Fatalf("migrateV127ToV128() error = %v", err)
	}
	if err := migrateV127ToV128(db.Write()); err != nil {
		t.Fatalf("migrateV127ToV128() rerun error = %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO account_identities (id, account_id, email) VALUES ('one', 'work', 'sales@example.com')`); err != nil {
		t.Fatalf("insert identity: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO account_identities (id, account_id, email, source) VALUES ('two', 'work', 'help@example.com', 'provider')`); err == nil {
		t.Fatal("provider identity without a provider id was accepted")
	}
}

func TestSaveAccountIdentity(t *testing.T) {
	ctx := context.Background()
	db := newIdentitiesTestDB(t)

	first, err := db.SaveAccountIdentityGuarded(ctx, "default", models.Identity{AccountID: "work", Name: " Ana Sales ", Email: " sales@example.com ", ReplyTo: "team@example.com"}, nil)
	if err != nil {
		t.Fatalf("save first: %v", err)
	}
	if first.ID == "" || first.Name != "Ana Sales" || first.Email != "sales@example.com" || first.ReplyTo != "<team@example.com>" || first.Source != models.IdentitySourceManual || first.SortOrder != 0 {
		t.Fatalf("first = %+v", first)
	}
	second, err := db.SaveAccountIdentityGuarded(ctx, "default", models.Identity{AccountID: "work", Email: "help@example.com"}, nil)
	if err != nil || second.SortOrder != 1 {
		t.Fatalf("second = %+v, %v", second, err)
	}

	first.Name = "Ana from Sales"
	first.ReplyTo = ""
	updated, err := db.SaveAccountIdentityGuarded(ctx, "default", first, nil)
	if err != nil || updated.Name != "Ana from Sales" || updated.ReplyTo != "" || updated.SortOrder != 0 {
		t.Fatalf("updated = %+v, %v", updated, err)
	}

	for _, bad := range []models.Identity{
		{AccountID: "work", Email: "not an address"},
		{AccountID: "work", Email: "Ana <ana@example.com>"},
		{AccountID: "work", Email: "ana@example.com", ReplyTo: "nobody"},
	} {
		if _, err := db.SaveAccountIdentityGuarded(ctx, "default", bad, nil); err == nil {
			t.Errorf("saved %+v", bad)
		}
	}
	if _, err := db.SaveAccountIdentityGuarded(ctx, "someone-else", models.Identity{AccountID: "work", Email: "x@example.com"}, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("other owner save err = %v", err)
	}
	if _, err := db.SaveAccountIdentityGuarded(ctx, "default", models.Identity{AccountID: "work", Email: "x@example.com", SignatureID: "missing"}, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown signature err = %v", err)
	}
	moved := first
	moved.AccountID = "home"
	if _, err := db.SaveAccountIdentityGuarded(ctx, "default", moved, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("save under another account err = %v", err)
	}

	list, err := db.ListAccountIdentities(ctx, "default", "work")
	if err != nil || len(list) != 2 || list[0].ID != first.ID || list[1].ID != second.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := db.SetAccountIdentityOrderGuarded(ctx, "default", "work", []string{second.ID, first.ID}, nil); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	if err := db.SetAccountIdentityOrderGuarded(ctx, "default", "work", []string{second.ID}, nil); err == nil {
		t.Fatal("partial order accepted")
	}
	list, _ = db.ListAccountIdentities(ctx, "default", "work")
	if len(list) != 2 || list[0].ID != second.ID {
		t.Fatalf("reordered list = %+v", list)
	}

	if err := db.DeleteAccountIdentityGuarded(ctx, "default", "work", second.ID, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := db.DeleteAccountIdentityGuarded(ctx, "default", "work", second.ID, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("delete again err = %v", err)
	}
	if _, err := db.GetAccountIdentity(ctx, "default", "work", first.ID); err != nil {
		t.Fatalf("get remaining: %v", err)
	}
}

func TestAccountIdentitySignatureAndAccountRemoval(t *testing.T) {
	ctx := context.Background()
	db := newIdentitiesTestDB(t)
	signature, err := db.SaveSignature(ctx, "default", models.Signature{Name: "Sales", TextBody: "Ana, Sales"})
	if err != nil {
		t.Fatalf("save signature: %v", err)
	}
	identity, err := db.SaveAccountIdentityGuarded(ctx, "default", models.Identity{AccountID: "work", Email: "sales@example.com", SignatureID: signature.ID}, nil)
	if err != nil || identity.SignatureID != signature.ID {
		t.Fatalf("identity = %+v, %v", identity, err)
	}
	if err := db.DeleteSignature(ctx, "default", signature.ID); err != nil {
		t.Fatalf("delete signature: %v", err)
	}
	if got, err := db.GetAccountIdentity(ctx, "default", "work", identity.ID); err != nil || got.SignatureID != "" {
		t.Fatalf("after signature delete = %+v, %v", got, err)
	}
	if _, err := db.Write().ExecContext(ctx, `DELETE FROM accounts WHERE id = 'work'`); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if list, err := db.ListUserIdentities(ctx, "default"); err != nil || len(list) != 0 {
		t.Fatalf("identities after account delete = %+v, %v", list, err)
	}
}

func TestReplaceProviderIdentities(t *testing.T) {
	ctx := context.Background()
	db := newIdentitiesTestDB(t)
	manual, err := db.SaveAccountIdentityGuarded(ctx, "default", models.Identity{AccountID: "home", Email: "old@example.net"}, nil)
	if err != nil {
		t.Fatalf("save manual: %v", err)
	}
	// The account's own address, a whole domain, a non-address and one a
	// manual identity has are left out.
	if err := db.ReplaceProviderIdentities(ctx, "home", []models.Identity{
		{ProviderIdentityID: "p1", Name: "Ana", Email: "hello@example.net"},
		{ProviderIdentityID: "p2", Email: "news@example.net", ReplyTo: "not an address"},
		{ProviderIdentityID: "own", Email: "ANA@example.net"},
		{ProviderIdentityID: "domain", Email: "*@example.org"},
		{ProviderIdentityID: "bad", Email: "nobody"},
		{ProviderIdentityID: "dup", Email: "Old@example.net"},
	}); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	list, _ := db.ListAccountIdentities(ctx, "default", "home")
	if len(list) != 3 || list[0].ID != manual.ID || list[1].ProviderIdentityID != "p1" || list[2].ProviderIdentityID != "p2" {
		t.Fatalf("after first replace = %+v", list)
	}
	provider := list[1]

	signature, _ := db.SaveSignature(ctx, "default", models.Signature{Name: "Hello", TextBody: "Ana"})
	provider.SignatureID = signature.ID
	if _, err := db.SaveAccountIdentityGuarded(ctx, "default", provider, nil); err != nil {
		t.Fatalf("set provider signature: %v", err)
	}
	renamed := provider
	renamed.Name = "Someone else"
	if _, err := db.SaveAccountIdentityGuarded(ctx, "default", renamed, nil); !errors.Is(err, ErrProviderIdentity) {
		t.Fatalf("rename provider identity err = %v", err)
	}
	if err := db.DeleteAccountIdentityGuarded(ctx, "default", "home", provider.ID, nil); !errors.Is(err, ErrProviderIdentity) {
		t.Fatalf("delete provider identity err = %v", err)
	}

	if err := db.ReplaceProviderIdentities(ctx, "home", []models.Identity{
		{ProviderIdentityID: "p1", Name: "Ana N.", Email: "hello@example.net", ReplyTo: "ana@example.net"},
	}); err != nil {
		t.Fatalf("second replace: %v", err)
	}
	list, _ = db.ListAccountIdentities(ctx, "default", "home")
	if len(list) != 2 || list[1].ID != provider.ID || list[1].Name != "Ana N." || list[1].ReplyTo != "<ana@example.net>" || list[1].SignatureID != signature.ID {
		t.Fatalf("after second replace = %+v", list)
	}

	if err := db.ReplaceProviderIdentities(ctx, "home", nil); err != nil {
		t.Fatalf("empty replace: %v", err)
	}
	list, _ = db.ListAccountIdentities(ctx, "default", "home")
	if len(list) != 1 || list[0].ID != manual.ID {
		t.Fatalf("after empty replace = %+v", list)
	}
}
