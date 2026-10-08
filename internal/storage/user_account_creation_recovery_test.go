package storage

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func pendingCreationFixture(t *testing.T, r *AccountRouting, owner string, committed bool) AccountRoute {
	t.Helper()
	// The real account repository creates/validates the store before reserving.
	if err := r.WithUser(t.Context(), owner, func(*DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	entry, err := r.ReserveAccount(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		if err := r.WithUser(t.Context(), owner, func(db *DB) error {
			_, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,email_address,encrypted_password) VALUES(?,?,?,X'010203');
 INSERT INTO folders(id,account_id,name,selectable) VALUES('same-folder',?,'Inbox',1)`, entry.AccountID, owner, owner+"@example.com", entry.AccountID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return entry
}

func TestPendingAccountCreationRecoveryReopensBothFilesAndPreservesDisabledOwner(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 1)
	a := pendingCreationFixture(t, r, "alice", true)
	b := pendingCreationFixture(t, r, "bob", false)
	if _, err := system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	path := system.Path()
	if err := stores.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := system.Close(); err != nil {
		t.Fatal(err)
	}
	system, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = system.Close() })
	stores = newUserStoreTestManager(t, system, UserStoreOptions{MaxOpen: 1})
	r, err = NewAccountRouting(stores)
	if err != nil {
		t.Fatal(err)
	}
	page, err := r.PendingAccountCreationPage(t.Context(), "", 64)
	if err != nil || len(page) != 2 {
		t.Fatal(page, err)
	}
	for _, entry := range page {
		want := AccountActive
		if entry.AccountID == b.AccountID {
			want = AccountDeleted
		}
		result, err := r.RecoverPendingAccountCreation(t.Context(), entry.UserID, entry.AccountID)
		if err != nil || result.State != want || !result.Changed {
			t.Fatal("recovery", result, err)
		}
		result, err = r.RecoverPendingAccountCreation(t.Context(), entry.UserID, entry.AccountID)
		if err != nil || result.State != want || result.Changed {
			t.Fatal("idempotent recovery", result, err)
		}
	}
	if err := r.withStartupStore(t.Context(), "alice", func(db *DB) error {
		var id, folder, encrypted string
		if err := db.Read().QueryRow(`SELECT a.id,f.id,hex(a.encrypted_password) FROM accounts a JOIN folders f ON f.account_id=a.id WHERE a.user_id='alice'`).Scan(&id, &folder, &encrypted); err != nil {
			return err
		}
		if id != a.AccountID || folder != "same-folder" || encrypted != "010203" {
			t.Fatal("local committed data changed", id, folder)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var status string
	var hint int
	if err := system.Read().QueryRow(`SELECT status,(SELECT COUNT(*) FROM gofer_contact_queue_schedule WHERE user_id='alice') FROM users WHERE id='alice'`).Scan(&status, &hint); err != nil || status != "disabled" || hint != 1 {
		t.Fatal("owner lifecycle/discovery changed", status, hint, err)
	}
	if err := r.WithAccount(t.Context(), a.AccountID, func(*DB, string) error { t.Error("disabled account dispatched"); return nil }); !errors.Is(err, ErrUserStoreOwner) {
		t.Fatal(err)
	}
	// Canceling a verified empty reservation must not make future store creation
	// impossible: the existing store remains, with no invented mailbox rows.
	createRoutingTestAccount(t, r, "bob")
}

func TestPendingAccountCreationRecoveryRefusesMissingCorruptAndDeletingData(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "deleting", "foreign", "pending-user", "foreign-request", "credentials"} {
		t.Run(mode, func(t *testing.T) {
			system, stores, r := newAccountRoutingTest(t, 1)
			entry := pendingCreationFixture(t, r, "alice", mode != "credentials")
			if mode == "deleting" || mode == "foreign" {
				if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
					if mode == "deleting" {
						_, err := db.Write().Exec(`UPDATE accounts SET is_deleting=1 WHERE id=?`, entry.AccountID)
						return err
					}
					// Corrupt legacy rows must not activate through a filtered lookup.
					_, err := db.Write().Exec(`DROP TRIGGER gofer_store_owner_insert; DROP TRIGGER gofer_store_owner_accounts_update; INSERT INTO users(id,username,username_normalized) VALUES('foreign','foreign','foreign'); UPDATE accounts SET user_id='foreign' WHERE id=?`, entry.AccountID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "pending-user" {
				if _, err := system.Write().Exec(`UPDATE users SET status='disabled',deletion_pending=1 WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "credentials" {
				if _, err := system.Write().Exec(`CREATE TABLE gofer_mailbox_credentials(account_id TEXT,user_id TEXT); INSERT INTO gofer_mailbox_credentials VALUES(?,'alice')`, entry.AccountID); err != nil {
					t.Fatal(err)
				}
			}
			path := stores.userPath("alice")
			if mode == "missing" || mode == "corrupt" {
				if err := stores.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if mode == "corrupt" {
					if err := os.WriteFile(path, []byte("damaged store"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				stores = newUserStoreTestManager(t, system, UserStoreOptions{MaxOpen: 1})
				var err error
				r, err = NewAccountRouting(stores)
				if err != nil {
					t.Fatal(err)
				}
			}
			owner := "alice"
			if mode == "foreign-request" {
				owner = "bob"
			}
			if result, err := r.RecoverPendingAccountCreation(t.Context(), owner, entry.AccountID); err == nil || result.Changed {
				t.Fatal("unverifiable reservation published", result, err)
			}
			state, err := r.AccountStateForUser(t.Context(), "alice", entry.AccountID)
			if err != nil || state != AccountCreating {
				t.Fatal("pending intent lost", state, err)
			}
			if mode == "missing" {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("missing store replaced", err)
				}
			}
			if mode == "corrupt" {
				if data, err := os.ReadFile(path); err != nil || string(data) != "damaged store" {
					t.Fatal("corrupt store replaced", err)
				}
			}
		})
	}
}

func TestPendingAccountCreationRecoveryRechecksCentralWriterWaitAndTriggerEffects(t *testing.T) {
	for _, mode := range []string{"writer-revocation", "trigger-owner", "trigger-state", "trigger-hint"} {
		t.Run(mode, func(t *testing.T) {
			system, _, r := newAccountRoutingTest(t, 1)
			entry := pendingCreationFixture(t, r, "alice", true)
			if mode == "writer-revocation" {
				tx, err := system.Write().BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				waits := system.Write().Stats().WaitCount
				done := make(chan error, 1)
				go func() { _, err := r.RecoverPendingAccountCreation(t.Context(), "alice", entry.AccountID); done <- err }()
				deadline := time.Now().Add(5 * time.Second)
				for system.Write().Stats().WaitCount == waits {
					if time.Now().After(deadline) {
						t.Fatal("did not reach central writer wait")
					}
					time.Sleep(time.Millisecond)
				}
				if _, err := tx.Exec(`UPDATE users SET status='disabled',deletion_pending=1 WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if !errors.Is(err, ErrUserStoreOwner) {
						t.Fatal("revoked owner published", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("recovery did not finish")
				}
			} else {
				effect := `UPDATE users SET status='disabled',deletion_pending=1 WHERE id=NEW.user_id;`
				if mode == "trigger-state" {
					effect = `UPDATE gofer_account_directory SET state='creating' WHERE account_id=NEW.account_id;`
				}
				if mode == "trigger-hint" {
					effect = `DELETE FROM gofer_contact_queue_schedule WHERE user_id=NEW.user_id;`
				}
				if _, err := system.Write().Exec(`CREATE TRIGGER disturb_creation AFTER UPDATE OF state ON gofer_account_directory WHEN NEW.state='active' BEGIN ` + effect + ` END`); err != nil {
					t.Fatal(err)
				}
				if result, err := r.RecoverPendingAccountCreation(t.Context(), "alice", entry.AccountID); err == nil || result.Changed {
					t.Fatal("trigger effect published", result, err)
				}
				if _, err := system.Write().Exec(`DROP TRIGGER disturb_creation`); err != nil {
					t.Fatal(err)
				}
				if result, err := r.RecoverPendingAccountCreation(t.Context(), "alice", entry.AccountID); err != nil || !result.Changed {
					t.Fatal("retry failed", result, err)
				}
			}
		})
	}
}

func TestPendingAccountCreationRecoveryCancellationAndCreationSerialization(t *testing.T) {
	system, _, r := newAccountRoutingTest(t, 1)
	entry := pendingCreationFixture(t, r, "alice", false)
	entered, release, created := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		created <- r.CompleteAccountCreation(t.Context(), "alice", entry.AccountID, func(db *DB) error {
			close(entered)
			<-release
			_, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,email_address) VALUES(?,'alice','mail@example.com')`, entry.AccountID)
			return err
		})
	}()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	_, err := r.RecoverPendingAccountCreation(ctx, "alice", entry.AccountID)
	cancel()
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("creation serialization", err)
	}
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	result, err := r.RecoverPendingAccountCreation(t.Context(), "alice", entry.AccountID)
	if err != nil || result.Changed || result.State != AccountActive {
		t.Fatal("serialized retry", result, err)
	}
	var n int
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_account_directory WHERE account_id=? AND state='active'`, entry.AccountID).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestPendingAccountCreationRecoveryPagesExcludeManagementAndUserDeletion(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 1)
	for i := 0; i < 65; i++ {
		if _, err := r.ReserveAccount(t.Context(), "alice"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.ReserveAccount(t.Context(), "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'; UPDATE users SET status='disabled',deletion_pending=1 WHERE id='bob'; INSERT INTO gofer_account_directory(account_id,user_id,state) VALUES('management-reservation','admin','creating')`); err != nil {
		t.Fatal(err)
	}
	after, count := "", 0
	for {
		page, err := r.PendingAccountCreationPage(t.Context(), after, 64)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		if len(page) > 64 {
			t.Fatal("unbounded discovery", len(page))
		}
		for _, entry := range page {
			if entry.UserID != "alice" || entry.State != AccountCreating || entry.AccountID <= after {
				t.Fatal("invalid discovery", entry)
			}
			after = entry.AccountID
			count++
		}
	}
	if count != 65 {
		t.Fatal("discovery lost a page", count)
	}
	stores.mu.Lock()
	opened := len(stores.entries)
	stores.mu.Unlock()
	if opened != 0 {
		t.Fatal("discovery opened user stores", opened)
	}
	for _, limit := range []int{0, 65} {
		if _, err := r.PendingAccountCreationPage(t.Context(), "", limit); !errors.Is(err, ErrAccountRoute) {
			t.Fatal("unbounded page admitted", err)
		}
	}
}

func TestPendingAccountCreationRecoveryCancelsLocalWriterWaitWithoutLosingIntent(t *testing.T) {
	_, _, r := newAccountRoutingTest(t, 1)
	entry := pendingCreationFixture(t, r, "alice", true)
	if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
		tx, err := db.Write().BeginTx(t.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		work, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		waits := db.Write().Stats().WaitCount
		go func() { _, err := r.RecoverPendingAccountCreation(work, "alice", entry.AccountID); done <- err }()
		deadline := time.Now().Add(5 * time.Second)
		for db.Write().Stats().WaitCount == waits {
			if time.Now().After(deadline) {
				return errors.New("did not reach local writer wait")
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				return err
			}
		case <-time.After(5 * time.Second):
			return errors.New("local writer wait did not cancel")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if state, err := r.AccountStateForUser(t.Context(), "alice", entry.AccountID); err != nil || state != AccountCreating {
		t.Fatal("cancellation lost intent", state, err)
	}
	if result, err := r.RecoverPendingAccountCreation(t.Context(), "alice", entry.AccountID); err != nil || !result.Changed || result.State != AccountActive {
		t.Fatal("retry", result, err)
	}
}
