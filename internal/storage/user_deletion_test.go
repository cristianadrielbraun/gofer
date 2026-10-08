package storage

import (
	"context"
	"errors"
	"os"
	"sync"
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

func TestPendingUserDeletionRecoveryRequiresConfirmedIntent(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 1)
	entry := createRoutingTestAccount(t, r, "alice")
	for _, update := range []string{
		`UPDATE users SET status='active',deletion_pending=0 WHERE id='alice'`,
		`UPDATE users SET status='disabled',deletion_pending=1,deletion_started_by=NULL WHERE id='alice'`,
	} {
		if _, err := system.Write().Exec(update); err != nil {
			t.Fatal(err)
		}
		if pending, err := r.RecoverPendingUserDeletion(t.Context(), "alice"); pending || !errors.Is(err, ErrUserDeletionIncomplete) {
			t.Fatal("unconfirmed recovery accepted", pending, err)
		}
		if storeDirectoryState(t, system, "alice") != "present" {
			t.Fatal("invalid intent changed file admission")
		}
	}
	if _, err := system.Write().Exec(`UPDATE users SET deletion_started_by='admin' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	if pending, err := r.RecoverPendingUserDeletion(t.Context(), "alice"); !pending || err != nil {
		t.Fatal(pending, err)
	}
	if entries, err := r.PendingUserAccounts(t.Context(), "alice", "", 1); err != nil || len(entries) != 1 || entries[0].AccountID != entry.AccountID {
		t.Fatal(entries, err)
	}
	if entries, err := r.PendingAccountCleanupPage(t.Context(), "", 64); err != nil || len(entries) != 0 {
		t.Fatal("whole-owner deletion leaked into account recovery", entries, err)
	}
	if _, err := os.Stat(stores.userPath("alice")); err != nil {
		t.Fatal("metadata recovery changed files", err)
	}
	if pending, err := r.RecoverPendingUserDeletion(t.Context(), "absent"); pending || err != nil {
		t.Fatal("completed identity recovery", pending, err)
	}
	if pending, err := r.RecoverPendingUserDeletion(t.Context(), "admin"); pending || !errors.Is(err, ErrUserDeletionIncomplete) {
		t.Fatal("management owner recovery", pending, err)
	}
	if _, err := system.Write().Exec(`INSERT INTO auth_system_state(id,initialized,owner_user_id) VALUES(1,1,'alice') ON CONFLICT(id) DO UPDATE SET owner_user_id='alice'`); err != nil {
		t.Fatal(err)
	}
	if pending, err := r.RecoverPendingUserDeletion(t.Context(), "alice"); pending || !errors.Is(err, ErrUserDeletionIncomplete) {
		t.Fatal("setup owner recovery", pending, err)
	}
}

func TestAccountDeletionIntentRechecksOwnerAfterTransitionWait(t *testing.T) {
	system, _, r := newAccountRoutingTest(t, 1)
	entry := createRoutingTestAccount(t, r, "alice")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	transitionDone, requestDone := make(chan error, 1), make(chan error, 1)
	go func() {
		transitionDone <- r.transition(t.Context(), entry.AccountID, func(*accountRouteScope) error { close(entered); <-release; return nil })
	}()
	<-entered
	go func() { requestDone <- r.RequestAccountDeletion(t.Context(), "alice", entry.AccountID) }()
	deadline := time.After(5 * time.Second)
	for {
		r.mu.Lock()
		waiting := r.scopes[entry.AccountID] != nil && r.scopes[entry.AccountID].refs == 2
		r.mu.Unlock()
		if waiting {
			break
		}
		select {
		case <-deadline:
			t.Fatal("deletion request did not reach transition wait")
		case <-time.After(time.Millisecond):
		}
	}
	if _, err := system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	if err := <-transitionDone; err != nil {
		t.Fatal(err)
	}
	if err := <-requestDone; !errors.Is(err, ErrAccountRoute) {
		t.Fatal("revoked owner committed a delayed deletion intent", err)
	}
	if state, err := r.AccountStateForUser(t.Context(), "alice", entry.AccountID); err != nil || state != AccountActive {
		t.Fatal("revoked request changed account lifecycle", state, err)
	}
}
