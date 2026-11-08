package storage

import (
	"path/filepath"
	"testing"
)

// Accounts from before connect as on the default, and there is no default
// proxy, so nothing changes until an administrator saves one.
func TestOutboundProxiesMigration(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('owner','owner','owner');
 INSERT INTO accounts(id,user_id,provider,email_address) VALUES('acc_a','owner','imap','ana@example.test');
 DROP INDEX idx_outbound_proxies_default; DROP TABLE outbound_proxies; ALTER TABLE accounts DROP COLUMN proxy_id;
 DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(140);`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV140ToV141(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	ctx := t.Context()
	choice, err := db.GetAccountProxyChoice(ctx, "acc_a")
	if err != nil || choice != AccountProxyDefault {
		t.Fatalf("account proxy after migration = %q, %v; want the default", choice, err)
	}
	if _, ok, err := db.DefaultOutboundProxy(ctx); err != nil || ok {
		t.Fatalf("default proxy after migration: ok = %v, err = %v; want none", ok, err)
	}
	if err := db.SaveOutboundProxy(ctx, OutboundProxy{ID: NewOutboundProxyID(), Name: "Office", Type: "http", Host: "proxy.example.test", Port: 3128}, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveOutboundProxy(ctx, OutboundProxy{ID: NewOutboundProxyID(), Name: "Bad", Type: "https", Host: "proxy.example.test", Port: 3128}, "admin"); err == nil {
		t.Fatal("the table accepted an https proxy")
	}
	if err := db.SaveOutboundProxy(ctx, OutboundProxy{ID: AccountProxyDirect, Name: "Direct", Type: "http", Host: "proxy.example.test", Port: 3128}, "admin"); err == nil {
		t.Fatal("a proxy took the id that means a direct connection")
	}
}
