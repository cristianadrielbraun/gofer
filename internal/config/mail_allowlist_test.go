package config

import (
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// A mailbox the allowed mail lists leave out can't be added, changed to such
// servers, or connected; an identity can't send through such a server.
func TestAccountsKeepToTheAllowedMailLists(t *testing.T) {
	db, store := newAccountStoreTestStore(t)
	ctx := t.Context()
	seedAccountStoreTestUser(t, ctx, db)
	for _, entry := range [][2]string{{storage.MailAllowlistServer, "*.example.com"}, {storage.MailAllowlistDomain, "example.com"}} {
		if _, err := db.AddMailAllowlistEntry(ctx, entry[0], entry[1], "admin"); err != nil {
			t.Fatal(err)
		}
	}

	elsewhere := secureAccountStoreTestRequest("ana@example.com")
	elsewhere.SMTPHost = "smtp.elsewhere.test"
	if _, err := store.CreateAccount(ctx, "default", elsewhere); !storage.IsMailAllowlistError(err) {
		t.Fatal("account on a server outside the list created:", err)
	}
	if _, err := store.CreateAccount(ctx, "default", secureAccountStoreTestRequest("ana@example.org")); !storage.IsMailAllowlistError(err) {
		t.Fatal("account with an address outside the domains created:", err)
	}
	if _, err := store.CreateAccount(ctx, "default", &models.CreateAccountRequest{Provider: "gmail", EmailAddress: "ana@example.com", AuthMethod: "oauth2"}); !storage.IsMailAllowlistError(err) {
		t.Fatal("Gmail account created while servers are limited:", err)
	}
	account, err := store.CreateAccount(ctx, "default", secureAccountStoreTestRequest("ana@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.updateAccount(ctx, account.ID, "default", &models.CreateAccountRequest{IMAPHost: "imap.elsewhere.test", IMAPPort: 993, IMAPTLSMode: "tls"}); !storage.IsMailAllowlistError(err) {
		t.Fatal("account moved to a server outside the list:", err)
	}
	if err := store.updateAccount(ctx, account.ID, "default", &models.CreateAccountRequest{DisplayName: "Ana"}); err != nil {
		t.Fatal("unrelated change refused:", err)
	}
	if _, err := store.GetConfig(ctx, account.ID); err != nil {
		t.Fatal("allowed account refused:", err)
	}
	if _, err := store.IdentitySMTPConfig(ctx, models.Identity{AccountID: account.ID, Email: "ana@example.com", SMTPHost: "smtp.elsewhere.test", SMTPPort: 465, SMTPTLSMode: "tls"}); !storage.IsMailAllowlistError(err) {
		t.Fatal("identity sends through a server outside the list:", err)
	}

	// The administrator narrows the list: the saved account stops connecting,
	// though its settings can still be read.
	entries, err := db.ListMailAllowlistEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Kind == storage.MailAllowlistServer {
			if err := db.DeleteMailAllowlistEntry(ctx, entry.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.AddMailAllowlistEntry(ctx, storage.MailAllowlistServer, "imap.example.com", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetConfig(ctx, account.ID); !storage.IsMailAllowlistError(err) {
		t.Fatal("account outside the narrowed list connected:", err)
	}
	if _, err := store.GetSettingsConfig(ctx, account.ID); err != nil {
		t.Fatal("settings of a refused account unreadable:", err)
	}
	if err := db.SetMailAllowlistExempt(ctx, "default", true, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetConfig(ctx, account.ID); err != nil {
		t.Fatal("exempt person's account refused:", err)
	}
}
