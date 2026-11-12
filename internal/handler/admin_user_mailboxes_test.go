package handler

import (
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestAdminUserMailboxesTabReadsOnlyThatPersonsAccounts(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	if _, err := f.system.AddMailAllowlistEntry(t.Context(), storage.MailAllowlistServer, "mail.allowed.test", "administrator"); err != nil {
		t.Fatal(err)
	}
	if err := f.system.SetMailAllowlistExempt(t.Context(), "bob", true, "administrator"); err != nil {
		t.Fatal(err)
	}
	failedAt := time.Date(2026, time.November, 12, 8, 30, 0, 0, time.UTC)
	if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error {
		return db.MarkEmailSyncError(t.Context(), f.accounts["alice"].ID, "authentication failed for the invented server", failedAt)
	}); err != nil {
		t.Fatal(err)
	}

	alice := f.request("/admin/users/alice/mailboxes")
	body := alice.Body.String()
	if alice.Code != 200 {
		t.Fatalf("alice's mailboxes = %d %q", alice.Code, body)
	}
	for _, want := range []string{
		"data-admin-user-mailbox>", "alice@example.com", "imap.example.com:", "smtp.example.com:",
		"authentication failed for the invented server", "data-admin-user-mailbox-refused",
		"The server imap.example.com isn&#39;t allowed.", "Mail sync off",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("alice's mailboxes lack %q: %q", want, body)
		}
	}
	// Someone else's accounts, credentials and mail never show.
	for _, forbidden := range []string{"bob@example.com", "synthetic", "private subject", "private body", "central@invalid.test"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("alice's mailboxes show %q", forbidden)
		}
	}

	// A disabled person's accounts can be read; an exempt person's aren't
	// limited.
	bob := f.request("/admin/users/bob/mailboxes").Body.String()
	if !strings.Contains(bob, "bob@example.com") || !strings.Contains(bob, "Not limited for this person") || strings.Contains(bob, "alice@example.com") {
		t.Fatalf("bob's mailboxes = %q", bob)
	}
	// An administrator has no mail of their own.
	administrator := f.request("/admin/users/administrator/mailboxes").Body.String()
	if !strings.Contains(administrator, "This person has no mail accounts of their own.") {
		t.Fatalf("administrator's mailboxes = %q", administrator)
	}
	// Someone who never added an account has an empty list, and no store is
	// made for them.
	unused := f.request("/admin/users/unused/mailboxes").Body.String()
	if !strings.Contains(unused, "They haven&#39;t added a mail account yet.") {
		t.Fatalf("unused person's mailboxes = %q", unused)
	}
}
