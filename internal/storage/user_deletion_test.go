package storage

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func prepareRoutingUserDeletion(t *testing.T, r *AccountRouting, owner string) []string {
	t.Helper()
	tx, err := r.System().Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), `UPDATE users SET status='disabled',deletion_pending=1 WHERE id=?`, owner); err != nil {
		t.Fatal(err)
	}
	ids, err := r.PreparePendingUserDeletionTx(t.Context(), tx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestPendingUserDeletionDrainsAccountWithoutReopeningRemovedStore(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 1)
	alice := createRoutingTestAccount(t, r, "alice")
	bob := createRoutingTestAccount(t, r, "bob")
	entered, unblock := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	}()
	go func() {
		done <- r.WithAccountActivityForUser(t.Context(), "alice", alice.AccountID, func() error { close(entered); <-unblock; return nil })
	}()
	<-entered
	ids := prepareRoutingUserDeletion(t, r, "alice")
	if len(ids) != 1 || ids[0] != alice.AccountID || storeDirectoryState(t, system, "alice") != "removing" {
		t.Fatal("central intent incomplete", ids)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := r.DrainPendingUserAccount(ctx, "alice", alice.AccountID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("active external operation was not drained", err)
	}
	if err := r.WithAccountActivityForUser(t.Context(), "alice", alice.AccountID, func() error { t.Error("new activity admitted"); return nil }); err == nil {
		t.Fatal("pending account activity accepted")
	}
	if err := r.WithAccountForUser(t.Context(), "bob", bob.AccountID, func(*DB) error { return nil }); err != nil {
		t.Fatal("another owner stalled", err)
	}
	close(unblock)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := r.DrainPendingUserAccount(t.Context(), "alice", bob.AccountID); !errors.Is(err, ErrAccountRoute) {
		t.Fatal("foreign drain accepted", err)
	}
	if err := r.DrainPendingUserAccount(t.Context(), "alice", alice.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := r.RemovePendingUserAccount(t.Context(), "alice", alice.AccountID, func(context.Context) error { t.Error("cleanup before database removal"); return nil }); !errors.Is(err, ErrUserDeletionIncomplete) {
		t.Fatal(err)
	}
	if err := r.RemovePendingUserStore(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("interrupted blob cleanup")
	if err := r.RemovePendingUserAccount(t.Context(), "alice", alice.AccountID, func(context.Context) error { return failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	if state, err := r.AccountStateForUser(t.Context(), "alice", alice.AccountID); err != nil || state != AccountDeleting {
		t.Fatal("failed cleanup tombstoned account", state, err)
	}
	for i := 0; i < 2; i++ {
		if err := r.RemovePendingUserAccount(t.Context(), "alice", alice.AccountID, func(context.Context) error {
			if _, err := os.Stat(stores.userPath("alice")); !errors.Is(err, os.ErrNotExist) {
				t.Error("cleanup recreated removed database", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.CompletePendingUserCleanup(t.Context(), "alice", func(context.Context) error { return failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	tx, err := system.Read().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = r.ValidatePendingUserCleanupTx(t.Context(), tx, "alice")
	_ = tx.Rollback()
	if !errors.Is(err, ErrUserDeletionIncomplete) {
		t.Fatal("failed compose cleanup published receipt", err)
	}
	if err := r.CompletePendingUserCleanup(t.Context(), "alice", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	tx, err = system.Read().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = r.ValidatePendingUserCleanupTx(t.Context(), tx, "alice")
	_ = tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
}

func TestPendingUserDeletionCompletesAccountsWithSeparateCredentials(t *testing.T) {
	system, _, r := newAccountRoutingTest(t, 1)
	one := createRoutingTestAccount(t, r, "alice")
	two := createRoutingTestAccount(t, r, "alice")
	// The storage boundary needs only ownership and account IDs, not ciphertext.
	// Actual repository encryption and credential cleanup have separate coverage.
	if _, err := system.Write().Exec(`CREATE TABLE gofer_mailbox_credentials(account_id TEXT PRIMARY KEY,user_id TEXT NOT NULL);
 INSERT INTO gofer_mailbox_credentials SELECT account_id,user_id FROM gofer_account_directory WHERE user_id='alice'`); err != nil {
		t.Fatal(err)
	}
	prepareRoutingUserDeletion(t, r, "alice")
	if err := r.RemovePendingUserStore(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	if err := r.RemovePendingUserAccount(t.Context(), "alice", one.AccountID, func(context.Context) error { return nil }); !errors.Is(err, ErrUserDeletionIncomplete) {
		t.Fatal("retained credentials accepted", err)
	}
	for _, entry := range []AccountRoute{one, two} {
		if err := r.RemovePendingUserAccount(t.Context(), "alice", entry.AccountID, func(ctx context.Context) error {
			_, err := system.Write().ExecContext(ctx, `DELETE FROM gofer_mailbox_credentials WHERE account_id=?`, entry.AccountID)
			return err
		}); err != nil {
			t.Fatal("per-account cleanup was blocked by sibling credentials", err)
		}
	}
	if err := r.CompletePendingUserCleanup(t.Context(), "alice", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
