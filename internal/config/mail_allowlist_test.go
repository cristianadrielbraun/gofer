package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/policy"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// setTestMailList sets one of the Default policy's allowed mail lists.
func setTestMailList(t *testing.T, db *storage.DB, setting *policy.Setting, entries ...string) {
	t.Helper()
	inTestPolicyTx(t, db, func(ctx context.Context, tx *sql.Tx) error {
		current, err := policy.Get(ctx, tx, policy.DefaultID)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(entries)
		if err != nil {
			return err
		}
		_, err = policy.SetValues(ctx, tx, policy.DefaultID, current.Version, []policy.ValueChange{{Key: setting.Key, JSON: raw}}, "", time.Now())
		return err
	})
}

// exemptFromTestMailLists assigns someone a policy that sets every list to
// nothing, as exempting them does.
func exemptFromTestMailLists(t *testing.T, db *storage.DB, userID string) {
	t.Helper()
	inTestPolicyTx(t, db, func(ctx context.Context, tx *sql.Tx) error {
		created, err := policy.Create(ctx, tx, policy.NewPolicy{ID: policy.MailExemptionID, Name: policy.MailExemptionName, At: time.Now()})
		if err != nil {
			return err
		}
		var none []policy.ValueChange
		for _, setting := range []*policy.Setting{policy.MailAllowedServers, policy.MailAllowedDomains, policy.MailAllowedProviders} {
			none = append(none, policy.ValueChange{Key: setting.Key, JSON: json.RawMessage(`[]`)})
		}
		if _, err := policy.SetValues(ctx, tx, created.ID, created.Version, none, "", time.Now()); err != nil {
			return err
		}
		_, err = policy.Assign(ctx, tx, created.ID, []policy.Target{{Kind: policy.TargetUser, ID: userID}}, "", time.Now())
		return err
	})
}

func inTestPolicyTx(t *testing.T, db *storage.DB, change func(context.Context, *sql.Tx) error) {
	t.Helper()
	tx, err := db.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := change(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// A mailbox the allowed mail lists leave out can't be added, changed to such
// servers, or connected; an identity can't send through such a server.
func TestAccountsKeepToTheAllowedMailLists(t *testing.T) {
	db, store := newAccountStoreTestStore(t)
	ctx := t.Context()
	seedAccountStoreTestUser(t, ctx, db)
	setTestMailList(t, db, policy.MailAllowedServers, "*.example.com")
	setTestMailList(t, db, policy.MailAllowedDomains, "example.com")

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
	setTestMailList(t, db, policy.MailAllowedServers, "imap.example.com")
	if _, err := store.GetConfig(ctx, account.ID); !storage.IsMailAllowlistError(err) {
		t.Fatal("account outside the narrowed list connected:", err)
	}
	if _, err := store.GetSettingsConfig(ctx, account.ID); err != nil {
		t.Fatal("settings of a refused account unreadable:", err)
	}
	exemptFromTestMailLists(t, db, "default")
	if _, err := store.GetConfig(ctx, account.ID); err != nil {
		t.Fatal("exempt person's account refused:", err)
	}
}
