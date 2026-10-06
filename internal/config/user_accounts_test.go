package config

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newUserAccountTestStore(t *testing.T) (*storage.DB, *storage.AccountRouting, *UserAccountStore) {
	t.Helper()
	system, _ := newAccountStoreTestStore(t)
	for _, user := range []string{"alice", "bob"} {
		if _, err := system.Write().Exec(`INSERT INTO users(id, username, username_normalized) VALUES (?, ?, ?)`, user, user, user); err != nil {
			t.Fatal(err)
		}
	}
	stores, err := storage.NewUserStores(system, storage.UserStoreOptions{MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stores.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	routing, err := storage.NewAccountRouting(stores)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := NewUserAccountStore(routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return system, routing, accounts
}

func TestUserAccountStoreKeepsConfigurationAndPasswordsLocal(t *testing.T) {
	system, _, accounts := newUserAccountTestStore(t)
	byOwner := make(map[string]*models.Account)
	for _, owner := range []string{"alice", "bob"} {
		req := secureAccountStoreTestRequest("same@example.com")
		req.Password = owner + "-private-password"
		account, err := accounts.CreateAccount(t.Context(), owner, req)
		if err != nil {
			t.Fatal(err)
		}
		byOwner[owner] = account
	}
	if byOwner["alice"].ID == byOwner["bob"].ID {
		t.Fatal("same mailbox collided across users")
	}
	if _, err := accounts.CreateAccount(t.Context(), "alice", secureAccountStoreTestRequest("SAME@example.com")); err == nil {
		t.Fatal("duplicate mailbox for the same owner accepted")
	}
	for owner, account := range byOwner {
		if err := accounts.WithAccount(t.Context(), account.ID, func(local *AccountStore, db *storage.DB, resolved string) error {
			password, err := local.DecryptPassword(t.Context(), account.ID)
			if err != nil {
				return err
			}
			if resolved != owner || password != owner+"-private-password" {
				t.Fatalf("owner=%q password mismatch", resolved)
			}
			cfg, err := local.GetConfig(t.Context(), account.ID)
			if err != nil {
				return err
			}
			if cfg.AccountID != account.ID {
				t.Error("configuration has wrong ID")
			}
			var encrypted []byte
			if err := db.Read().QueryRow(`SELECT encrypted_password FROM accounts WHERE id = ?`, account.ID).Scan(&encrypted); err != nil {
				return err
			}
			if string(encrypted) == password {
				t.Error("password stored without encryption")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.WithAccountForUser(t.Context(), "bob", byOwner["alice"].ID, func(*AccountStore, *storage.DB) error { t.Error("foreign account service invoked"); return nil }); !errors.Is(err, storage.ErrAccountRoute) {
		t.Fatal(err)
	}
	var count int
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("central mailbox copies=%d err=%v", count, err)
	}
}

func TestUserAccountStoreReadsLiveCentralTransportPolicy(t *testing.T) {
	system, routing, accounts := newUserAccountTestStore(t)
	if err := system.AddPlaintextTransportException(t.Context(), "imap", "mail.test", 1143, "alice"); err != nil {
		t.Fatal(err)
	}
	req := secureAccountStoreTestRequest("plaintext@example.com")
	req.IMAPHost, req.IMAPPort, req.IMAPTLSMode = "mail.test", 1143, "plaintext"
	account, err := accounts.CreateAccount(t.Context(), "alice", req)
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.WithAccount(t.Context(), account.ID, func(local *AccountStore, db *storage.DB, _ string) error {
		cfg, err := local.GetConfig(t.Context(), account.ID)
		if err != nil {
			return err
		}
		if !cfg.IMAPAllowPlaintext {
			t.Error("central policy not consulted")
		}
		localExceptions, err := db.ListMailSecurityExceptions(t.Context())
		if err != nil {
			return err
		}
		if len(localExceptions) != 0 {
			t.Error("central policy copied into user database")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	exceptions, err := system.ListMailSecurityExceptions(t.Context())
	if err != nil || len(exceptions) != 1 {
		t.Fatalf("exceptions=%d err=%v", len(exceptions), err)
	}
	if err := system.DeleteMailSecurityException(t.Context(), exceptions[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WithAccount(t.Context(), account.ID, func(local *AccountStore, _ *storage.DB, _ string) error {
		_, err := local.GetConfig(t.Context(), account.ID)
		return err
	}); err == nil {
		t.Fatal("revoked central policy still allowed")
	}
	if _, err := accounts.CreateAccount(t.Context(), "alice", req); err == nil {
		t.Fatal("invalid transport accepted")
	}
	pending, err := routing.ListAccounts(t.Context(), "alice", storage.AccountCreating, "", 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("invalid request reserved account: %v %v", pending, err)
	}
}

func TestUserAccountStoreDeletionRemainsRetryableUntilExternalCleanup(t *testing.T) {
	_, routing, accounts := newUserAccountTestStore(t)
	account, err := accounts.CreateAccount(t.Context(), "alice", secureAccountStoreTestRequest("mail@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.WithAccountForUser(t.Context(), "alice", account.ID, func(_ *AccountStore, db *storage.DB) error {
		return db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{ID: "inbox", AccountID: account.ID, Name: "Inbox", Selectable: true}})
	}); err != nil {
		t.Fatal(err)
	}
	externalFailure := errors.New("blob removal failed")
	if err := accounts.DeleteAccount(t.Context(), "alice", account.ID, func(context.Context, string) error { return externalFailure }); !errors.Is(err, externalFailure) {
		t.Fatal(err)
	}
	pending, err := routing.ListAccounts(t.Context(), "alice", storage.AccountDeleting, "", 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending deletion=%v err=%v", pending, err)
	}
	if err := accounts.WithAccount(t.Context(), account.ID, func(*AccountStore, *storage.DB, string) error { t.Error("deleting account routed"); return nil }); !errors.Is(err, storage.ErrAccountRoute) {
		t.Fatal(err)
	}
	var calls int
	cleanup := func(_ context.Context, id string) error {
		calls++
		if id != account.ID {
			t.Error("wrong external cleanup ID")
		}
		return nil
	}
	if err := accounts.DeleteAccount(t.Context(), "alice", account.ID, cleanup); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		var count int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM folders`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Error("local account data remains")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.DeleteAccount(t.Context(), "alice", account.ID, cleanup); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("cleanup retry calls=%d", calls)
	}
}

func TestUserAccountStoreExposesInterruptedCreationForRecovery(t *testing.T) {
	system, routing, accounts := newUserAccountTestStore(t)
	if _, err := system.Write().Exec(`CREATE TRIGGER reject_activation BEFORE UPDATE OF state ON gofer_account_directory WHEN NEW.state = 'active' BEGIN SELECT RAISE(ABORT, 'simulated crash boundary'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := accounts.CreateAccount(t.Context(), "alice", secureAccountStoreTestRequest("mail@example.com"))
	var pending *PendingAccountCreationError
	if !errors.As(err, &pending) || pending.AccountID == "" {
		t.Fatalf("recoverable error: %v", err)
	}
	if _, err := system.Write().Exec(`DROP TRIGGER reject_activation`); err != nil {
		t.Fatal(err)
	}
	if err := routing.CompleteAccountCreation(t.Context(), "alice", pending.AccountID, nil); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WithAccount(t.Context(), pending.AccountID, func(local *AccountStore, _ *storage.DB, owner string) error {
		_, err := local.GetConfig(t.Context(), pending.AccountID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
