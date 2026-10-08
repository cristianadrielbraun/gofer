package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserAccountCreationStartupRecoveryAfterPhysicalRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "system.db")
	key := []byte("0123456789abcdef0123456789abcdef")
	var system *storage.DB
	var stores *storage.UserStores
	var routing *storage.AccountRouting
	var accounts *UserAccountStore
	open := func() {
		t.Helper()
		var err error
		system, err = storage.New(path)
		if err != nil {
			t.Fatal(err)
		}
		stores, err = storage.NewUserStores(system, storage.UserStoreOptions{MaxOpen: 1})
		if err != nil {
			t.Fatal(err)
		}
		routing, err = storage.NewAccountRouting(stores)
		if err != nil {
			t.Fatal(err)
		}
		accounts, err = NewUserAccountStore(routing, key)
		if err != nil {
			t.Fatal(err)
		}
	}
	open()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stores.Close(ctx); err != nil {
			t.Error(err)
		}
		if err := system.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, owner := range []string{"alice", "bob"} {
		if _, err := system.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES(?,?,?)`, owner, owner, owner); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := system.Write().Exec(`CREATE TRIGGER reject_activation BEFORE UPDATE OF state ON gofer_account_directory WHEN NEW.state='active' BEGIN SELECT RAISE(ABORT,'interrupted publication'); END`); err != nil {
		t.Fatal(err)
	}
	req := secureAccountStoreTestRequest("alice@example.com")
	req.Password, req.SmtpPassword = "mail-secret", "smtp-secret"
	req.SmtpUsername = "smtp-alice"
	_, err := accounts.CreateAccount(t.Context(), "alice", req)
	var pending *PendingAccountCreationError
	if !errors.As(err, &pending) {
		t.Fatal("missing durable creation error", err)
	}
	var encrypted, smtpEncrypted []byte
	if err := routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		if err := db.Read().QueryRow(`SELECT encrypted_password,encrypted_smtp_password FROM accounts WHERE id=?`, pending.AccountID).Scan(&encrypted, &smtpEncrypted); err != nil {
			return err
		}
		return db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{ID: "existing-folder", AccountID: pending.AccountID, Name: "Inbox", Selectable: true}})
	}); err != nil {
		t.Fatal(err)
	}
	// Two central pages of genuinely empty reservations in an existing store.
	if err := routing.WithUser(t.Context(), "bob", func(*storage.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 65; i++ {
		if _, err := routing.ReserveAccount(t.Context(), "bob"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := system.Write().Exec(`DROP TRIGGER reject_activation; UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	if err := stores.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := system.Close(); err != nil {
		t.Fatal(err)
	}
	open()
	result, err := accounts.RecoverPendingAccountCreations(t.Context())
	if err != nil || result.Activated != 1 || result.Canceled != 65 {
		t.Fatal("startup recovery counts", result, err)
	}
	if result, err := accounts.RecoverPendingAccountCreations(t.Context()); err != nil || result.Activated != 0 || result.Canceled != 0 {
		t.Fatal("recovery replay", result, err)
	}
	var status string
	if err := system.Read().QueryRow(`SELECT status FROM users WHERE id='alice'`).Scan(&status); err != nil || status != "disabled" {
		t.Fatal("disabled user was enabled", status, err)
	}
	// Restore access explicitly only after proving recovery preserved lifecycle.
	if _, err := system.Write().Exec(`UPDATE users SET status='active' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WithAccount(t.Context(), pending.AccountID, func(local *AccountStore, db *storage.DB, owner string) error {
		cfg, err := local.GetConfig(t.Context(), pending.AccountID)
		if err != nil {
			return err
		}
		password, err := local.DecryptPassword(t.Context(), pending.AccountID)
		if err != nil {
			return err
		}
		if owner != "alice" || cfg.AccountID != pending.AccountID || cfg.IMAPHost != req.IMAPHost || cfg.SMTPHost != req.SMTPHost || password != req.Password {
			return fmt.Errorf("saved configuration changed after recovery")
		}
		var actual, smtpActual []byte
		var folders int
		if err := db.Read().QueryRow(`SELECT encrypted_password,encrypted_smtp_password,(SELECT COUNT(*) FROM folders WHERE account_id=?) FROM accounts WHERE id=?`, pending.AccountID, pending.AccountID).Scan(&actual, &smtpActual, &folders); err != nil {
			return err
		}
		if !bytes.Equal(actual, encrypted) || !bytes.Equal(smtpActual, smtpEncrypted) || folders != 1 {
			return fmt.Errorf("encrypted data or folder rows changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var centralRows int
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&centralRows); err != nil || centralRows != 0 {
		t.Fatal("configuration copied centrally", centralRows, err)
	}
	if _, err := accounts.CreateAccount(t.Context(), "bob", secureAccountStoreTestRequest("new-bob@example.com")); err != nil {
		t.Fatal("canceled reservations blocked new account", err)
	}
}

func TestUserAccountCreationStartupRecoveryStopsOnMissingStoreAndCanRetry(t *testing.T) {
	_, routing, accounts := newUserAccountTestStore(t)
	// A reservation without an existing store cannot prove that local data was
	// never committed. Refuse startup without inventing an empty replacement.
	entry, err := routing.ReserveAccount(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if result, err := accounts.RecoverPendingAccountCreations(t.Context()); err == nil || result.Activated != 0 || result.Canceled != 0 {
		t.Fatal("missing store accepted", result, err)
	}
	if state, err := routing.AccountStateForUser(t.Context(), "alice", entry.AccountID); err != nil || state != storage.AccountCreating {
		t.Fatal("reservation lost", state, err)
	}
	// An operator restores/initializes a verified store; a retry may now cancel
	// only the proven empty reservation. No recovery path creates this file.
	if err := routing.WithUser(t.Context(), "alice", func(*storage.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := accounts.RecoverPendingAccountCreations(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled startup admitted", err)
	}
	if result, err := accounts.RecoverPendingAccountCreations(t.Context()); err != nil || result.Canceled != 1 {
		t.Fatal("retry", result, err)
	}
}
