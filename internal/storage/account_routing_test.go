package storage

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func newAccountRoutingTest(t *testing.T, maxOpen int) (*DB, *UserStores, *AccountRouting) {
	t.Helper()
	system := newUserStoreTestSystem(t)
	stores := newUserStoreTestManager(t, system, UserStoreOptions{MaxOpen: maxOpen})
	routing, err := NewAccountRouting(stores)
	if err != nil {
		t.Fatal(err)
	}
	return system, stores, routing
}

func createRoutingTestAccount(t *testing.T, r *AccountRouting, userID string) AccountRoute {
	t.Helper()
	entry, err := r.ReserveAccount(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CompleteAccountCreation(t.Context(), userID, entry.AccountID, func(db *DB) error {
		_, err := db.Write().ExecContext(t.Context(), `INSERT INTO accounts(id, user_id, email_address) VALUES (?, ?, 'shared@example.com')`, entry.AccountID, userID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	entry.State = AccountActive
	return entry
}

func TestAccountRoutingUsesExplicitOwnershipAndCentralPolicyScope(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 2)
	again, err := NewAccountRouting(stores)
	if err != nil || again != r {
		t.Fatalf("coordinator identity: %p %p %v", r, again, err)
	}
	accounts := map[string]AccountRoute{"alice": createRoutingTestAccount(t, r, "alice"), "bob": createRoutingTestAccount(t, r, "bob")}
	if accounts["alice"].AccountID == accounts["bob"].AccountID {
		t.Fatal("new account IDs overlap")
	}
	for owner, entry := range accounts {
		if err := r.WithAccountForUser(t.Context(), owner, entry.AccountID, func(db *DB) error {
			return db.SetUISettings(t.Context(), owner, map[string]string{"theme": owner})
		}); err != nil {
			t.Fatal(err)
		}
		if err := r.WithAccount(t.Context(), entry.AccountID, func(db *DB, resolved string) error {
			value := db.GetUISettings(t.Context(), owner)["theme"]
			if resolved != owner || value != owner {
				t.Errorf("owner=%q theme=%q", resolved, value)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []string{"bob", "", "admin"} {
		if err := r.WithAccountForUser(t.Context(), user, accounts["alice"].AccountID, func(*DB) error { t.Error("foreign callback invoked"); return nil }); !errors.Is(err, ErrAccountRoute) {
			t.Fatalf("foreign route: %v", err)
		}
	}
	if err := r.WithAccount(t.Context(), "missing", func(*DB, string) error { t.Error("missing callback invoked"); return nil }); !errors.Is(err, ErrAccountRoute) {
		t.Fatal(err)
	}
	if _, err := system.Write().Exec(`UPDATE gofer_account_directory SET user_id = 'bob' WHERE account_id = ?`, accounts["alice"].AccountID); err == nil {
		t.Fatal("ownership changed")
	}
	var count int
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("central configuration rows=%d err=%v", count, err)
	}
	if len(r.scopes) != 0 {
		t.Fatal("idle routing scopes retained")
	}
}

func TestAccountRoutingCreationRecoversCommittedLocalRow(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 1)
	entry, err := r.ReserveAccount(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := system.Write().Exec(`CREATE TRIGGER reject_activation BEFORE UPDATE OF state ON gofer_account_directory
		WHEN NEW.state = 'active' BEGIN SELECT RAISE(ABORT, 'simulated cutover failure'); END`); err != nil {
		t.Fatal(err)
	}
	var creates int
	create := func(db *DB) error {
		creates++
		_, err := db.Write().Exec(`INSERT INTO accounts(id, user_id, email_address) VALUES (?, 'alice', 'mail@example.com')`, entry.AccountID)
		return err
	}
	if err := r.CompleteAccountCreation(t.Context(), "alice", entry.AccountID, create); err == nil {
		t.Fatal("activation should fail")
	}
	if err := r.CancelAccountCreation(t.Context(), "alice", entry.AccountID); !errors.Is(err, ErrAccountRoute) {
		t.Fatalf("committed row discarded: %v", err)
	}
	if _, err := system.Write().Exec(`DROP TRIGGER reject_activation`); err != nil {
		t.Fatal(err)
	}
	if err := stores.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	stores = newUserStoreTestManager(t, system, UserStoreOptions{MaxOpen: 1})
	r, err = NewAccountRouting(stores)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CompleteAccountCreation(t.Context(), "alice", entry.AccountID, create); err != nil {
		t.Fatal(err)
	}
	if creates != 1 {
		t.Fatalf("recovery recreated account %d times", creates)
	}
	if err := r.CompleteAccountCreation(t.Context(), "bob", entry.AccountID, nil); !errors.Is(err, ErrAccountRoute) {
		t.Fatal(err)
	}
	if err := r.WithAccount(t.Context(), entry.AccountID, func(*DB, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestAccountRoutingEmptyCreationCanBeCancelledButNeverActivated(t *testing.T) {
	_, stores, r := newAccountRoutingTest(t, 1)
	entry, err := r.ReserveAccount(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	stores.mu.Lock()
	opened := len(stores.entries)
	stores.mu.Unlock()
	if opened != 0 {
		t.Fatal("reservation opened a user database")
	}
	if err := r.CompleteAccountCreation(t.Context(), "alice", entry.AccountID, nil); !errors.Is(err, ErrAccountRoute) {
		t.Fatal(err)
	}
	if err := r.CancelAccountCreation(t.Context(), "alice", entry.AccountID); !errors.Is(err, ErrAccountRoute) || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing store cannot prove an empty reservation", err)
	}
	if err := r.WithUser(t.Context(), "alice", func(*DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.CancelAccountCreation(t.Context(), "alice", entry.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := r.CompleteAccountCreation(t.Context(), "alice", entry.AccountID, nil); !errors.Is(err, ErrAccountRoute) {
		t.Fatal(err)
	}
	if _, err := r.route(t.Context(), entry.AccountID, "alice", AccountDeleted); err != nil {
		t.Fatal(err)
	}
}

func TestAccountRoutingDeletionDrainsCallbacksAndResumesAfterTimeout(t *testing.T) {
	system, _, r := newAccountRoutingTest(t, 1)
	entry := createRoutingTestAccount(t, r, "alice")
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- r.WithAccount(t.Context(), entry.AccountID, func(*DB, string) error { close(entered); <-release; return nil })
	}()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := r.BeginAccountDeletion(ctx, "alice", entry.AccountID); !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		t.Fatalf("drain: %v", err)
	}
	if err := r.WithAccount(t.Context(), entry.AccountID, func(*DB, string) error { t.Error("new operation accepted during deletion"); return nil }); !errors.Is(err, ErrAccountRoute) {
		t.Fatal(err)
	}
	if err := r.CompleteAccountDeletion(t.Context(), "alice", entry.AccountID, func(*DB) error { t.Error("cleanup while callback pinned"); return nil }); !errors.Is(err, ErrAccountRoute) {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := system.Write().Exec(`UPDATE users SET status = 'disabled', deletion_pending = 1 WHERE id = 'alice'`); err != nil {
		t.Fatal(err)
	}
	if err := r.BeginAccountDeletion(t.Context(), "alice", entry.AccountID); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("simulated external cleanup failure")
	if err := r.CompleteAccountDeletion(t.Context(), "alice", entry.AccountID, func(*DB) error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := r.CompleteAccountDeletion(t.Context(), "alice", entry.AccountID, nil); !errors.Is(err, ErrAccountRoute) {
		t.Fatalf("nonempty account tombstoned: %v", err)
	}
	if err := r.CompleteAccountDeletion(t.Context(), "alice", entry.AccountID, func(db *DB) error {
		_, err := db.Write().Exec(`DELETE FROM accounts WHERE id = ?`, entry.AccountID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.BeginAccountDeletion(t.Context(), "alice", entry.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := r.CompleteAccountDeletion(t.Context(), "alice", entry.AccountID, nil); err != nil {
		t.Fatal(err)
	}
	if len(r.scopes) != 0 {
		t.Fatal("idle routing scopes retained")
	}
}

func TestAccountRoutingRejectsInactiveOwnersAndUnindexedLocalAccounts(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 1)
	entry := createRoutingTestAccount(t, r, "alice")
	for _, status := range []string{"disabled", "pending"} {
		if _, err := system.Write().Exec(`UPDATE users SET status = ? WHERE id = 'alice'`, status); err != nil {
			t.Fatal(err)
		}
		if err := r.WithUser(t.Context(), "alice", func(*DB) error { t.Error("inactive user routed"); return nil }); !errors.Is(err, ErrUserStoreOwner) {
			t.Fatal(err)
		}
		if err := r.WithAccount(t.Context(), entry.AccountID, func(*DB, string) error { t.Error("inactive worker routed"); return nil }); !errors.Is(err, ErrUserStoreOwner) {
			t.Fatal(err)
		}
		if _, err := r.ReserveAccount(t.Context(), "alice"); !errors.Is(err, ErrUserStoreOwner) {
			t.Fatal(err)
		}
	}
	if _, err := r.ReserveAccount(t.Context(), "admin"); !errors.Is(err, ErrUserStoreOwner) {
		t.Fatal(err)
	}
	if err := stores.WithUser(t.Context(), "alice", func(db *DB) error {
		_, err := db.Write().Exec(`INSERT INTO accounts(id, user_id, email_address) VALUES ('unindexed', 'alice', 'a@example.com')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.WithAccount(t.Context(), "unindexed", func(*DB, string) error { t.Error("directory fallback used"); return nil }); !errors.Is(err, ErrAccountRoute) {
		t.Fatal(err)
	}
}

func TestAccountRoutingPagesWithoutOpeningStores(t *testing.T) {
	_, stores, r := newAccountRoutingTest(t, 1)
	for i := 0; i < 5; i++ {
		if _, err := r.ReserveAccount(t.Context(), "alice"); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var cursor string
	for {
		page, err := r.ListAccounts(t.Context(), "alice", AccountCreating, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, entry := range page {
			if entry.AccountID <= cursor {
				t.Fatal("unstable pagination")
			}
			cursor = entry.AccountID
			count++
		}
	}
	if count != 5 {
		t.Fatalf("page count=%d", count)
	}
	stores.mu.Lock()
	opened := len(stores.entries)
	stores.mu.Unlock()
	if opened != 0 {
		t.Fatal("directory listing opened user stores")
	}
}

func TestAccountRoutingSerializesCreationAndCancellation(t *testing.T) {
	_, _, r := newAccountRoutingTest(t, 1)
	entry, err := r.ReserveAccount(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	entered, release, created := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		created <- r.CompleteAccountCreation(t.Context(), "alice", entry.AccountID, func(db *DB) error {
			close(entered)
			<-release
			_, err := db.Write().Exec(`INSERT INTO accounts(id, user_id, email_address) VALUES (?, 'alice', 'a@example.com')`, entry.AccountID)
			return err
		})
	}()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := r.CancelAccountCreation(ctx, "alice", entry.AccountID); !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		t.Fatalf("cancel raced creation: %v", err)
	}
	close(release)
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	if err := r.CancelAccountCreation(t.Context(), "alice", entry.AccountID); !errors.Is(err, ErrAccountRoute) {
		t.Fatal(err)
	}
	if len(r.scopes) != 0 {
		t.Fatal("cancelled transition retained scope")
	}
}

func TestAccountRoutingRevalidatesOwnerAfterCacheWait(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 1)
	entry := createRoutingTestAccount(t, r, "bob")
	lease := acquireUserStore(t, stores, "alice")
	var called atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- r.WithAccount(t.Context(), entry.AccountID, func(*DB, string) error { called.Store(true); return nil })
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		r.mu.Lock()
		scope := r.scopes[entry.AccountID]
		waiting := scope != nil && scope.running == 1
		r.mu.Unlock()
		if waiting {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("worker never registered")
		case <-time.After(time.Millisecond):
		}
	}
	if _, err := system.Write().Exec(`UPDATE users SET status = 'disabled' WHERE id = 'bob'`); err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if err := <-done; !errors.Is(err, ErrUserStoreOwner) || called.Load() {
		t.Fatalf("waiting owner: err=%v called=%v", err, called.Load())
	}
}

func TestAccountRoutingNeverReplacesMissingActiveStoreWithEmptyDatabase(t *testing.T) {
	_, stores, r := newAccountRoutingTest(t, 1)
	entry := createRoutingTestAccount(t, r, "alice")
	if err := r.WithUser(t.Context(), "bob", func(*DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	path := stores.userPath("alice")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := r.WithAccount(t.Context(), entry.AccountID, func(*DB, string) error { t.Error("missing store routed"); return nil }); !errors.Is(err, ErrAccountRoute) {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty replacement created: %v", err)
	}
	if err := r.CompleteAccountCreation(t.Context(), "alice", entry.AccountID, nil); !errors.Is(err, ErrAccountRoute) {
		t.Fatal(err)
	}
}

func TestAccountRoutingNeverReplacesMissingHistoricalStore(t *testing.T) {
	for _, state := range []AccountRouteState{AccountActive, AccountDeleting, AccountDeleted} {
		t.Run(string(state), func(t *testing.T) {
			system, stores, routing := newAccountRoutingTest(t, 1)
			entry := createRoutingTestAccount(t, routing, "alice")
			if err := routing.WithUser(t.Context(), "alice", func(db *DB) error {
				return db.SetSetting(t.Context(), "alice", "retained-contact-preference", "retained")
			}); err != nil {
				t.Fatal(err)
			}
			if state == AccountDeleting {
				if err := routing.RequestAccountDeletion(t.Context(), "alice", entry.AccountID); err != nil {
					t.Fatal(err)
				}
			}
			if state == AccountDeleted {
				if err := routing.BeginAccountDeletion(t.Context(), "alice", entry.AccountID); err != nil {
					t.Fatal(err)
				}
				if err := routing.CompleteAccountDeletion(t.Context(), "alice", entry.AccountID, func(db *DB) error {
					_, err := db.Write().Exec(`DELETE FROM accounts WHERE id=?`, entry.AccountID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			// Eviction checkpoints/closes Alice before deleting only her disposable DB.
			if err := routing.WithUser(t.Context(), "bob", func(*DB) error { return nil }); err != nil {
				t.Fatal(err)
			}
			path := stores.userPath("alice")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := routing.WithUser(t.Context(), "alice", func(*DB) error { t.Error("missing historical owner store routed"); return nil }); !errors.Is(err, ErrAccountRoute) || !errors.Is(err, os.ErrNotExist) {
				t.Fatal("lost historical store accepted", state, err)
			}
			if state != AccountDeleted {
				if err := routing.BeginAccountDeletion(t.Context(), "alice", entry.AccountID); !errors.Is(err, ErrAccountRoute) || !errors.Is(err, os.ErrNotExist) {
					t.Fatal("deletion recreated store", err)
				}
				if err := routing.CompleteAccountDeletion(t.Context(), "alice", entry.AccountID, func(*DB) error { t.Error("lost store cleanup invoked"); return nil }); !errors.Is(err, ErrAccountRoute) || !errors.Is(err, os.ErrNotExist) {
					t.Fatal("lost store reported deleted", err)
				}
				var current AccountRouteState
				if err := system.Read().QueryRow(`SELECT state FROM gofer_account_directory WHERE account_id=?`, entry.AccountID).Scan(&current); err != nil || current != AccountDeleting {
					t.Fatal("failed deletion erased recoverable intent", current, err)
				}
			}
			// A new intent cannot bypass a historical owner's lost-store requirement.
			fresh, err := routing.ReserveAccount(t.Context(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			if err := routing.CompleteAccountCreation(t.Context(), "alice", fresh.AccountID, func(*DB) error { t.Error("new creation bypassed missing historical storage"); return nil }); !errors.Is(err, ErrAccountRoute) {
				t.Fatal(err)
			}
			if err := routing.CancelAccountCreation(t.Context(), "alice", fresh.AccountID); !errors.Is(err, ErrAccountRoute) {
				t.Fatal("cancellation created empty historical storage", err)
			}
			if _, err := routing.route(t.Context(), fresh.AccountID, "alice", AccountCreating); err != nil {
				t.Fatal("failed cancellation discarded recoverable intent", err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("replacement database created", err)
			}
		})
	}
}

func TestAccountRoutingDisabledOwnerDeletionUsesExistingStore(t *testing.T) {
	system, _, routing := newAccountRoutingTest(t, 1)
	entry := createRoutingTestAccount(t, routing, "alice")
	if err := routing.WithUser(t.Context(), "alice", func(db *DB) error { return db.SetSetting(t.Context(), "alice", "retained-preference", "kept") }); err != nil {
		t.Fatal(err)
	}
	if _, err := system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	if err := routing.WithUser(t.Context(), "alice", func(*DB) error { t.Error("disabled owner browsed store"); return nil }); !errors.Is(err, ErrUserStoreOwner) {
		t.Fatal(err)
	}
	if err := routing.BeginAccountDeletion(t.Context(), "alice", entry.AccountID); err != nil {
		t.Fatal("disabled owner maintenance rejected", err)
	}
	if err := routing.CompleteAccountDeletion(t.Context(), "alice", entry.AccountID, func(db *DB) error {
		value, err := db.GetSetting(t.Context(), "alice", "retained-preference")
		if err != nil {
			return err
		}
		if value != "kept" {
			t.Error("cleanup lost unrelated owner data")
		}
		_, err = db.Write().Exec(`DELETE FROM accounts WHERE id=?`, entry.AccountID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.route(t.Context(), entry.AccountID, "alice", AccountDeleted); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := system.Read().QueryRow(`SELECT status FROM users WHERE id='alice'`).Scan(&status); err != nil || status != "disabled" {
		t.Fatal("maintenance reactivated owner", status, err)
	}
}
