package storage

import (
	"errors"
	"path/filepath"
	"strings"
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
 DROP TABLE outbound_proxy_users; DROP TABLE outbound_proxy_policy; DROP INDEX idx_outbound_proxies_name;
 DROP INDEX idx_outbound_proxies_default; DROP TABLE outbound_proxies; ALTER TABLE accounts DROP COLUMN proxy_id;
 DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(140);`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV140ToV141(db.Write()); err != nil {
			t.Fatal(err)
		}
		if err := migrateV141ToV142(db.Write()); err != nil {
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

// Schema 142 keeps the proxies schema 141 saved, as the administrator's and
// for everyone, and makes names unique per owner.
func TestOutboundProxiesMigrationKeepsSavedProxies(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('ana','ana','ana'),('ben','ben','ben');
 DROP TABLE outbound_proxy_users; DROP TABLE outbound_proxy_policy; DROP INDEX idx_outbound_proxies_name;
 DROP INDEX idx_outbound_proxies_default; DROP TABLE outbound_proxies;`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{outboundProxiesTableSQLv141, outboundProxiesDefaultIndexSQL,
		`INSERT INTO outbound_proxies (id, name, type, host, port, is_default) VALUES ('office', 'Office', 'http', 'proxy.example.test', 3128, 1)`,
		`DELETE FROM schema_version`, `INSERT INTO schema_version(version) VALUES(141)`} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := migrateV141ToV142(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	ctx := t.Context()
	office, err := db.GetOutboundProxy(ctx, "office")
	if err != nil || office.OwnerUserID != "" || !office.Everyone || !office.IsDefault {
		t.Fatalf("proxy after migration = %+v, %v", office, err)
	}
	for _, user := range []string{"ana", "ben"} {
		if ok, err := db.OutboundProxyAvailableTo(ctx, office, user); err != nil || !ok {
			t.Fatalf("office for %s = %v, %v", user, ok, err)
		}
	}
	// Two people may each name a proxy Office; one person may not twice.
	if err := db.SetPeopleMayAddOutboundProxies(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveOutboundProxy(ctx, OutboundProxy{ID: "ana-office", Name: "Office", Type: "http", Host: "a.example.test", Port: 1, OwnerUserID: "ana"}, "ana"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveOutboundProxy(ctx, OutboundProxy{ID: "ana-again", Name: "Office", Type: "http", Host: "b.example.test", Port: 1, OwnerUserID: "ana"}, "ana"); !errors.Is(err, ErrOutboundProxyNameTaken) {
		t.Fatalf("second proxy of ana's named Office: %v", err)
	}
	// A person's own proxy is never the default, and only theirs.
	if err := db.SaveOutboundProxy(ctx, OutboundProxy{ID: "ben-own", Name: "Mine", Type: "http", Host: "c.example.test", Port: 1, OwnerUserID: "ben", IsDefault: true}, "ben"); err != nil {
		t.Fatal(err)
	}
	if def, ok, err := db.DefaultOutboundProxy(ctx); err != nil || !ok || def.ID != "office" {
		t.Fatalf("default = %+v %v %v, want the administrator's", def, ok, err)
	}
	own, _ := db.GetOutboundProxy(ctx, "ben-own")
	if ok, _ := db.OutboundProxyAvailableTo(ctx, own, "ana"); ok {
		t.Fatal("ben's own proxy is available to ana")
	}
	if ok, _ := db.OutboundProxyAvailableTo(ctx, own, "ben"); !ok {
		t.Fatal("ben's own proxy isn't available to ben")
	}
	if err := db.SetPeopleMayAddOutboundProxies(ctx, false); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.OutboundProxyAvailableTo(ctx, own, "ben"); ok {
		t.Fatal("ben's own proxy is still used after people were stopped from adding their own")
	}
	// An administrator's proxy for chosen people.
	if err := db.SetOutboundProxyEveryone(ctx, "office", false); err != nil {
		t.Fatal(err)
	}
	if err := db.SetOutboundProxyUser(ctx, "office", "ana", true); err != nil {
		t.Fatal(err)
	}
	office, _ = db.GetOutboundProxy(ctx, "office")
	if ok, _ := db.OutboundProxyAvailableTo(ctx, office, "ana"); !ok {
		t.Fatal("office isn't available to ana, who was chosen")
	}
	if ok, _ := db.OutboundProxyAvailableTo(ctx, office, "ben"); ok {
		t.Fatal("office is available to ben, who wasn't chosen")
	}
	if ok, _ := db.OutboundProxyAvailableTo(ctx, office, ""); !ok {
		t.Fatal("office isn't available to work for nobody in particular")
	}
	if listed, _ := db.OutboundProxiesForUser(ctx, "ben"); len(listed) != 0 {
		t.Fatalf("ben is offered %+v", listed)
	}
}

// Schema 143 lets a private target exception approve a proxy, keeping the
// exceptions there were.
func TestMailSecurityExceptionsMigrationAllowsProxies(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := strings.Replace(mailSecurityExceptionsTableSQL, "'smtp', 'proxy')", "'smtp')", 1)
	for _, statement := range []string{`DROP INDEX idx_mail_security_exceptions_lookup`, `DROP TABLE mail_security_exceptions`, old,
		`INSERT INTO mail_security_exceptions (id, kind, protocol, host, port) VALUES ('kept', 'private_target', 'imap', '192.168.1.10', 993)`,
		`DELETE FROM schema_version`, `INSERT INTO schema_version(version) VALUES(142)`} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	ctx := t.Context()
	if err := db.AddPrivateTargetException(ctx, "proxy", "192.168.1.5", 3128, "admin"); err == nil {
		t.Fatal("the old table accepted a proxy exception")
	}
	for range 2 {
		if err := migrateV142ToV143(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := db.IsPrivateTargetAllowed(ctx, "imap", "192.168.1.10", 993); err != nil || !ok {
		t.Fatalf("kept exception = %v, %v", ok, err)
	}
	if err := db.AddPrivateTargetException(ctx, "proxy", "192.168.1.5", 3128, "admin"); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.IsPrivateTargetAllowed(ctx, "proxy", "192.168.1.5", 3128); err != nil || !ok {
		t.Fatalf("proxy exception = %v, %v", ok, err)
	}
}
