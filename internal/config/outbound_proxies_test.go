package config

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func stringPointer(s string) *string { return &s }

func TestSaveOutboundProxyValidatesAndEncryptsThePassword(t *testing.T) {
	ctx := context.Background()
	db, store := newAccountStoreTestStore(t)

	for _, input := range []OutboundProxyInput{
		{Name: "", Type: "http", Host: "proxy.example.test", Port: 3128},
		{Name: "Office", Type: "https", Host: "proxy.example.test", Port: 3128},
		{Name: "Office", Type: "socks4", Host: "proxy.example.test", Port: 1080},
		{Name: "Office", Type: "http", Host: "http://proxy.example.test", Port: 3128},
		{Name: "Office", Type: "http", Host: "proxy.example.test", Port: 0},
	} {
		if _, err := store.SaveOutboundProxy(ctx, input, "admin"); err == nil {
			t.Errorf("SaveOutboundProxy(%+v) accepted an invalid proxy", input)
		}
	}

	id, err := store.SaveOutboundProxy(ctx, OutboundProxyInput{Name: " Office ", Type: "SOCKS5", Host: "proxy.example.test", Port: 1080,
		Username: "ana", Password: stringPointer("s3cret")}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := db.GetOutboundProxy(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Name != "Office" || saved.Type != netguard.ProxySOCKS5 || saved.Username != "ana" {
		t.Fatalf("saved %+v", saved)
	}
	if len(saved.EncryptedPassword) == 0 || bytes.Contains(saved.EncryptedPassword, []byte("s3cret")) {
		t.Fatal("the proxy password was not stored encrypted")
	}

	// Copied into another proxy's row, the password doesn't decrypt.
	other, err := store.SaveOutboundProxy(ctx, OutboundProxyInput{Name: "Other", Type: "http", Host: "other.example.test", Port: 3128}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(ctx, `UPDATE outbound_proxies SET username = 'ana', encrypted_password = ? WHERE id = ?`, saved.EncryptedPassword, other); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveProxyChoice(ctx, other); err == nil {
		t.Fatal("a password copied from another proxy decrypted")
	}

	if _, err := store.SaveOutboundProxy(ctx, OutboundProxyInput{Name: "Office", Type: "http", Host: "x.example.test", Port: 3128}, "admin"); !errors.Is(err, storage.ErrOutboundProxyNameTaken) {
		t.Fatalf("duplicate name error = %v", err)
	}
}

func TestSaveOutboundProxyKeepsOrReplacesThePassword(t *testing.T) {
	ctx := context.Background()
	_, store := newAccountStoreTestStore(t)
	id, err := store.SaveOutboundProxy(ctx, OutboundProxyInput{Name: "Office", Type: "http", Host: "proxy.example.test", Port: 3128,
		Username: "ana", Password: stringPointer("first")}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	password := func() string {
		t.Helper()
		p, err := store.ResolveProxyChoice(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return p.Password
	}

	// Editing other fields without a password keeps the saved one.
	if _, err := store.SaveOutboundProxy(ctx, OutboundProxyInput{ID: id, Name: "Office", Type: "http", Host: "proxy2.example.test", Port: 8080, Username: "ana"}, "admin"); err != nil {
		t.Fatal(err)
	}
	if got := password(); got != "first" {
		t.Fatalf("password after an edit = %q, want it kept", got)
	}
	if _, err := store.SaveOutboundProxy(ctx, OutboundProxyInput{ID: id, Name: "Office", Type: "http", Host: "proxy2.example.test", Port: 8080, Username: "ana", Password: stringPointer("second")}, "admin"); err != nil {
		t.Fatal(err)
	}
	if got := password(); got != "second" {
		t.Fatalf("password = %q, want the new one", got)
	}
	// Without a username there is no password to send, so none is kept.
	if _, err := store.SaveOutboundProxy(ctx, OutboundProxyInput{ID: id, Name: "Office", Type: "http", Host: "proxy2.example.test", Port: 8080}, "admin"); err != nil {
		t.Fatal(err)
	}
	if got := password(); got != "" {
		t.Fatalf("password without a username = %q", got)
	}
	if _, err := store.SaveOutboundProxy(ctx, OutboundProxyInput{ID: "missing", Name: "Ghost", Type: "http", Host: "proxy.example.test", Port: 3128}, "admin"); !errors.Is(err, storage.ErrOutboundProxyNotFound) {
		t.Fatalf("editing a missing proxy error = %v", err)
	}
}

func TestAccountProxyFollowsTheChoiceAndTheDefault(t *testing.T) {
	ctx := context.Background()
	db, store := newAccountStoreTestStore(t)
	seedAccountStoreTestUser(t, ctx, db)
	account, err := store.CreateAccount(ctx, "default", secureAccountStoreTestRequest("ana@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	resolve := func() *netguard.Proxy {
		t.Helper()
		p, err := store.AccountProxy(ctx, account.ID)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	if p := resolve(); p != nil {
		t.Fatalf("a new account with no default proxy connects through %s", p)
	}

	office, err := store.SaveOutboundProxy(ctx, OutboundProxyInput{Name: "Office", Type: "http", Host: "office.example.test", Port: 3128, IsDefault: true}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	home, err := store.SaveOutboundProxy(ctx, OutboundProxyInput{Name: "Home", Type: "socks5", Host: "home.example.test", Port: 1080,
		Username: "ana", Password: stringPointer("pw")}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if p := resolve(); p == nil || p.Host != "office.example.test" {
		t.Fatalf("account on the default connects through %v, want the default proxy", p)
	}

	if err := store.SetAccountProxy(ctx, "default", account.ID, home); err != nil {
		t.Fatal(err)
	}
	if p := resolve(); p == nil || p.Host != "home.example.test" || p.Password != "pw" {
		t.Fatalf("account that chose Home connects through %+v", p)
	}

	if err := store.SetAccountProxy(ctx, "default", account.ID, storage.AccountProxyDirect); err != nil {
		t.Fatal(err)
	}
	if p := resolve(); p != nil {
		t.Fatalf("account set to direct connects through %s", p)
	}

	if err := store.SetAccountProxy(ctx, "default", account.ID, "no-such-proxy"); !errors.Is(err, storage.ErrOutboundProxyNotFound) {
		t.Fatalf("choosing a missing proxy error = %v", err)
	}
	if err := store.SetAccountProxy(ctx, "someone-else", account.ID, home); err == nil {
		t.Fatal("another user changed the account's proxy")
	}

	// A deleted proxy leaves its accounts on the default.
	if err := store.SetAccountProxy(ctx, "default", account.ID, home); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteOutboundProxy(ctx, home); err != nil {
		t.Fatal(err)
	}
	if p := resolve(); p == nil || p.Host != "office.example.test" {
		t.Fatalf("account whose proxy was deleted connects through %v, want the default", p)
	}

	// Only one proxy is the default at a time.
	if _, err := store.SaveOutboundProxy(ctx, OutboundProxyInput{Name: "Branch", Type: "http", Host: "branch.example.test", Port: 3128, IsDefault: true}, "admin"); err != nil {
		t.Fatal(err)
	}
	if p := resolve(); p == nil || p.Host != "branch.example.test" {
		t.Fatalf("account on the default connects through %v, want the new default", p)
	}
	saved, err := db.GetOutboundProxy(ctx, office)
	if err != nil || saved.IsDefault {
		t.Fatalf("old default %+v, %v: want it no longer the default", saved, err)
	}
}

// An account set to use a proxy that can't be read fails to connect rather
// than connecting directly.
func TestAccountProxyNeverFallsBackToDirectOnErrors(t *testing.T) {
	ctx := context.Background()
	db, store := newAccountStoreTestStore(t)
	id, err := store.SaveOutboundProxy(ctx, OutboundProxyInput{Name: "Office", Type: "http", Host: "proxy.example.test", Port: 3128,
		Username: "ana", Password: stringPointer("pw"), IsDefault: true}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(ctx, `UPDATE outbound_proxies SET encrypted_password = X'0102' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	for _, choice := range []string{id, storage.AccountProxyDefault} {
		if p, err := store.ResolveProxyChoice(ctx, choice); err == nil {
			t.Fatalf("ResolveProxyChoice(%q) = %v with an unreadable password, want an error", choice, p)
		}
	}
}

// Loading an account to connect fills in its proxy, as does sending through
// one of its identities' own servers; loading it for settings works even when
// the proxy can't be read, so the choice can still be changed.
func TestAccountConfigsCarryTheAccountProxy(t *testing.T) {
	ctx := context.Background()
	db, store := newAccountStoreTestStore(t)
	seedAccountStoreTestUser(t, ctx, db)
	account, err := store.CreateAccount(ctx, "default", secureAccountStoreTestRequest("ana@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.GetConfig(ctx, account.ID)
	if err != nil || cfg.Proxy != nil {
		t.Fatalf("GetConfig without proxies = %+v, %v; want a direct connection", cfg, err)
	}
	id, err := store.SaveOutboundProxy(ctx, OutboundProxyInput{Name: "Office", Type: "socks5", Host: "proxy.example.test", Port: 1080,
		Username: "ana", Password: stringPointer("pw")}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAccountProxy(ctx, "default", account.ID, id); err != nil {
		t.Fatal(err)
	}
	cfg, err = store.GetConfig(ctx, account.ID)
	if err != nil || cfg.Proxy == nil || cfg.Proxy.Host != "proxy.example.test" || cfg.Proxy.Password != "pw" {
		t.Fatalf("GetConfig = %+v, %v; want the Office proxy", cfg, err)
	}
	identity, err := store.IdentitySMTPConfig(ctx, models.Identity{AccountID: account.ID, Email: "ana@example.com", SMTPHost: "smtp.example.com", SMTPPort: 465, SMTPTLSMode: "tls"})
	if err != nil || identity.Proxy == nil || identity.Proxy.Host != "proxy.example.test" {
		t.Fatalf("IdentitySMTPConfig = %+v, %v; want the account's proxy", identity, err)
	}
	edit, err := store.GetEditData(ctx, account.ID)
	if err != nil || edit.ProxyID != id {
		t.Fatalf("GetEditData proxy = %q, %v; want %q", edit.ProxyID, err, id)
	}

	if _, err := db.Write().ExecContext(ctx, `UPDATE outbound_proxies SET encrypted_password = X'0102' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if cfg, err := store.GetConfig(ctx, account.ID); err == nil {
		t.Fatalf("GetConfig with an unreadable proxy = %+v, want an error", cfg)
	}
	if _, err := store.GetSettingsConfig(ctx, account.ID); err != nil {
		t.Fatalf("GetSettingsConfig with an unreadable proxy: %v", err)
	}
}

// Accounts in a person's own database use the central saved proxies.
func TestUserAccountSnapshotsCarryTheAccountProxy(t *testing.T) {
	_, _, accounts, owners := newServiceAccountFixture(t)
	id, err := accounts.base.SaveOutboundProxy(t.Context(), OutboundProxyInput{Name: "Office", Type: "http", Host: "proxy.example.test", Port: 3128}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	alice := owners["alice"].ID
	if err := accounts.WithAccountForUser(t.Context(), "alice", alice, func(local *AccountStore, _ *storage.DB) error {
		return local.SetAccountProxy(t.Context(), "alice", alice, id)
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := serviceSnapshotFor(t, accounts, "alice", alice).SMTPConfig(t.Context())
	if err != nil || cfg.Proxy == nil || cfg.Proxy.Host != "proxy.example.test" {
		t.Fatalf("alice's SMTPConfig = %+v, %v; want the Office proxy", cfg, err)
	}
	cfg, err = serviceSnapshotFor(t, accounts, "bob", owners["bob"].ID).SMTPConfig(t.Context())
	if err != nil || cfg.Proxy != nil {
		t.Fatalf("bob's SMTPConfig = %+v, %v; want a direct connection", cfg, err)
	}
	var loaded *models.AccountConfig
	if err := accounts.WithAccountForUser(t.Context(), "alice", alice, func(local *AccountStore, _ *storage.DB) error {
		var err error
		loaded, err = local.GetConfig(t.Context(), alice)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if loaded.Proxy == nil || loaded.Proxy.Host != "proxy.example.test" {
		t.Fatalf("alice's GetConfig proxy = %v", loaded.Proxy)
	}
}
