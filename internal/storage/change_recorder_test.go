package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// Every administrative write that the admin console records runs its
// recorder once when it changes something, never when it changes nothing,
// and fails, unchanged, when its recorder fails.
func TestAdministrativeWritesRunTheirRecorderOnlyForChanges(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT INTO users (id, username, username_normalized, name, status, user_type, is_admin, created_at, updated_at)
		VALUES ('pia-lund', 'pia-lund', 'pia-lund', 'Pia Lund', 'active', 'webmail', 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	calls := 0
	recorded := WithChangeRecorder(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		calls++
		return nil
	})
	expect := func(name string, want int, write func(context.Context) error) {
		t.Helper()
		calls = 0
		if err := write(recorded); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if calls != want {
			t.Fatalf("%s recorded %d times, want %d", name, calls, want)
		}
	}

	expect("add discovery exception", 1, func(ctx context.Context) error { return db.AddHTTPDiscoveryException(ctx, "mail.sample.test", "admin") })
	expect("add it again", 0, func(ctx context.Context) error { return db.AddHTTPDiscoveryException(ctx, "mail.sample.test", "admin") })
	expect("add plaintext exception", 1, func(ctx context.Context) error {
		return db.AddPlaintextTransportException(ctx, "imap", "mail.sample.test", 143, "admin")
	})
	expect("add private target", 1, func(ctx context.Context) error {
		return db.AddPrivateTargetException(ctx, "http", "127.0.0.1", 8080, "admin")
	})
	exceptions, err := db.ListMailSecurityExceptions(context.Background())
	if err != nil || len(exceptions) != 3 {
		t.Fatalf("exceptions = %+v, %v", exceptions, err)
	}
	expect("delete exception", 1, func(ctx context.Context) error { return db.DeleteMailSecurityException(ctx, exceptions[0].ID) })
	expect("delete it again", 0, func(ctx context.Context) error { return db.DeleteMailSecurityException(ctx, exceptions[0].ID) })

	expect("allow a server", 1, func(ctx context.Context) error {
		_, err := db.AddMailAllowlistEntry(ctx, MailAllowlistServer, "imap.sample.test", "admin")
		return err
	})
	expect("allow it again", 0, func(ctx context.Context) error {
		_, err := db.AddMailAllowlistEntry(ctx, MailAllowlistServer, "imap.sample.test", "admin")
		return err
	})
	expect("allow Gmail", 1, func(ctx context.Context) error { return db.SetMailAllowlistProvider(ctx, "gmail", true, "admin") })
	expect("stop allowing Gmail", 1, func(ctx context.Context) error { return db.SetMailAllowlistProvider(ctx, "gmail", false, "admin") })
	expect("stop again", 0, func(ctx context.Context) error { return db.SetMailAllowlistProvider(ctx, "gmail", false, "admin") })
	expect("exempt a person", 1, func(ctx context.Context) error { return db.SetMailAllowlistExempt(ctx, "pia-lund", true, "admin") })
	expect("exempt again", 0, func(ctx context.Context) error { return db.SetMailAllowlistExempt(ctx, "pia-lund", true, "admin") })

	proxy := OutboundProxy{ID: NewOutboundProxyID(), Name: "Office", Type: "http", Host: "proxy.sample.test", Port: 3128}
	expect("save a proxy", 1, func(ctx context.Context) error { return db.SaveOutboundProxy(ctx, proxy, "admin") })
	expect("offer it to everyone", 1, func(ctx context.Context) error { return db.SetOutboundProxyEveryone(ctx, proxy.ID, true) })
	expect("choose a person", 1, func(ctx context.Context) error { return db.SetOutboundProxyUser(ctx, proxy.ID, "pia-lund", true) })
	expect("choose them again", 0, func(ctx context.Context) error { return db.SetOutboundProxyUser(ctx, proxy.ID, "pia-lund", true) })
	expect("let people add proxies", 1, func(ctx context.Context) error { return db.SetPeopleMayAddOutboundProxies(ctx, true) })
	expect("let them again", 0, func(ctx context.Context) error { return db.SetPeopleMayAddOutboundProxies(ctx, true) })
	expect("delete the proxy", 1, func(ctx context.Context) error { return db.DeleteOutboundProxy(ctx, "", proxy.ID) })

	// Without a recorder, nothing changes about how they work.
	if err := db.AddHTTPDiscoveryException(context.Background(), "other.sample.test", "admin"); err != nil {
		t.Fatal(err)
	}

	refused := errors.New("audit log unavailable")
	failing := WithChangeRecorder(context.Background(), func(context.Context, *sql.Tx) error { return refused })
	if err := db.AddHTTPDiscoveryException(failing, "refused.sample.test", "admin"); !errors.Is(err, refused) {
		t.Fatalf("change with a failing record = %v", err)
	}
	if allowed, err := db.IsHTTPDiscoveryAllowed(context.Background(), "refused.sample.test"); err != nil || allowed {
		t.Fatalf("a change whose record failed stayed: %v, %v", allowed, err)
	}
	if err := db.SaveOutboundProxy(failing, OutboundProxy{ID: NewOutboundProxyID(), Name: "Refused", Type: "http", Host: "refused.sample.test", Port: 8080}, "admin"); !errors.Is(err, refused) {
		t.Fatalf("proxy saved with a failing record: %v", err)
	}
	var proxies int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM outbound_proxies WHERE name = 'Refused'`).Scan(&proxies); err != nil || proxies != 0 {
		t.Fatalf("refused proxy rows = %d, %v", proxies, err)
	}
}
