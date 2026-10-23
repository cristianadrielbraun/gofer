package storage

import (
	"path/filepath"
	"slices"
	"testing"
)

// Accounts from before keep their order by ID until the owner changes it,
// and only a list naming each of the owner's accounts once is accepted.
func TestAccountOrder(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('owner','owner','owner'),('other','other','other');
 INSERT INTO accounts(id,user_id,provider,email_address) VALUES
  ('acc_a','owner','imap','ana@example.test'),('acc_b','owner','imap','ben@example.test'),('acc_c','owner','imap','cleo@example.test'),
  ('acc_d','other','imap','dana@example.test');
 INSERT INTO accounts(id,user_id,provider,email_address,is_deleting) VALUES('acc_e','owner','imap','eli@example.test',1);
 ALTER TABLE accounts DROP COLUMN sort_order; DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(123);`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV123ToV124(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	ctx := t.Context()
	order := func() []string {
		t.Helper()
		accounts, err := db.GetAccounts(ctx, "owner")
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, account := range accounts {
			ids = append(ids, account.ID)
		}
		return ids
	}
	if got := order(); !slices.Equal(got, []string{"acc_a", "acc_b", "acc_c"}) {
		t.Fatalf("after migration = %v", got)
	}
	for _, ids := range [][]string{
		{"acc_c", "acc_a"},
		{"acc_c", "acc_a", "acc_a"},
		{"acc_c", "acc_a", "acc_b", "acc_e"},
		{"acc_c", "acc_a", "acc_d"},
		{"acc_c", "acc_a", "acc_b", "acc_b"},
	} {
		if err := db.SetAccountOrder(ctx, "owner", ids); err != ErrAccountOrderChanged {
			t.Fatalf("SetAccountOrder(%v) = %v", ids, err)
		}
	}
	if got := order(); !slices.Equal(got, []string{"acc_a", "acc_b", "acc_c"}) {
		t.Fatalf("after refused orders = %v", got)
	}
	if err := db.SetAccountOrder(ctx, "owner", []string{"acc_c", "acc_a", "acc_b"}); err != nil {
		t.Fatal(err)
	}
	if got := order(); !slices.Equal(got, []string{"acc_c", "acc_a", "acc_b"}) {
		t.Fatalf("after reordering = %v", got)
	}
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 124 {
		t.Fatalf("schema version = %d %v", version, err)
	}
}
