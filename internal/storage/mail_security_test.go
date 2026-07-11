package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMailSecurityExceptionsAreExactAndTrackAffectedAccounts(t *testing.T) {
	ctx := context.Background()
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.AddHTTPDiscoveryException(ctx, "LAB.EXAMPLE.TEST.", "admin"); err != nil {
		t.Fatalf("AddHTTPDiscoveryException() error = %v", err)
	}
	allowed, err := db.IsHTTPDiscoveryAllowed(ctx, "lab.example.test")
	if err != nil || !allowed {
		t.Fatalf("IsHTTPDiscoveryAllowed() = %v, %v; want true", allowed, err)
	}
	allowed, err = db.IsHTTPDiscoveryAllowed(ctx, "other.example.test")
	if err != nil || allowed {
		t.Fatalf("other IsHTTPDiscoveryAllowed() = %v, %v; want false", allowed, err)
	}

	if err := db.AddPlaintextTransportException(ctx, "imap", "MAIL.TEST.", 1143, "admin"); err != nil {
		t.Fatalf("AddPlaintextTransportException() error = %v", err)
	}
	allowed, err = db.IsPlaintextTransportAllowed(ctx, "IMAP", "mail.test", 1143)
	if err != nil || !allowed {
		t.Fatalf("IsPlaintextTransportAllowed() = %v, %v; want true", allowed, err)
	}
	allowed, err = db.IsPlaintextTransportAllowed(ctx, "imap", "mail.test", 143)
	if err != nil || allowed {
		t.Fatalf("other port IsPlaintextTransportAllowed() = %v, %v; want false", allowed, err)
	}

	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users (id, email, name, is_admin) VALUES ('owner', 'owner@example.com', 'Owner', 0);
		INSERT INTO accounts (
			id, user_id, provider, email_address, imap_host, imap_port, imap_tls_mode,
			smtp_host, smtp_port, smtp_tls_mode, username, auth_method
		) VALUES (
			'account', 'owner', 'imap', 'owner@example.com', 'mail.test', 1143, 'plaintext',
			'mail.test', 2465, 'tls', 'owner@example.com', 'plain'
		)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	items, err := db.ListMailSecurityExceptions(ctx)
	if err != nil {
		t.Fatalf("ListMailSecurityExceptions() error = %v", err)
	}
	var plaintextID string
	for _, item := range items {
		if item.Kind != "plaintext_transport" {
			continue
		}
		plaintextID = item.ID
		if len(item.Accounts) != 1 || item.Accounts[0].ID != "account" {
			t.Fatalf("plaintext exception accounts = %#v", item.Accounts)
		}
	}
	if plaintextID == "" {
		t.Fatal("plaintext exception not listed")
	}
	if err := db.DeleteMailSecurityException(ctx, plaintextID); err != nil {
		t.Fatalf("DeleteMailSecurityException() error = %v", err)
	}
	allowed, err = db.IsPlaintextTransportAllowed(ctx, "imap", "mail.test", 1143)
	if err != nil || allowed {
		t.Fatalf("revoked IsPlaintextTransportAllowed() = %v, %v; want false", allowed, err)
	}
}
