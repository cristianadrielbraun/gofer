package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"
)

func seedOwnedRetention(t *testing.T, r *AccountRouting, owner string, n int) string {
	t.Helper()
	account := createRoutingTestAccount(t, r, owner)
	if err := r.WithUser(t.Context(), owner, func(db *DB) error {
		tx, err := db.Write().BeginTx(t.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for i := 0; i < n; i++ {
			if _, err := tx.Exec(`INSERT INTO outgoing_sends(id,account_id,transport,envelope_from,envelope_recipients,status,sent_copy_status,send_after,updated_at) VALUES(printf('same-send-%d',?),?,'smtp','test@example.com','[]','sent','not_required',datetime('now','-31 days'),datetime('now','-31 days'))`, i, account.AccountID); err != nil {
				return err
			}
		}
		return tx.Commit()
	}); err != nil {
		t.Fatal(err)
	}
	return account.AccountID
}

func TestRetainedUserMailRetentionIsolationDisabledAndRecoveryBarriers(t *testing.T) {
	system, _, r := newAccountRoutingTest(t, 1)
	a := seedOwnedRetention(t, r, "alice", 7)
	seedOwnedRetention(t, r, "bob", 1)
	if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
		_, err := db.Write().Exec(`INSERT INTO calendar_sources(id,user_id,account_id,provider,remote_id,name,is_selected) VALUES('source','alice',?,'caldav','/primary/','Calendar',1);
 INSERT INTO calendar_reply_jobs(id,user_id,source_id,resource_id,remote_id,version,response,payload,state) VALUES('same-send-1','alice','source','resource','remote','v1','accepted','{}','pending'),('same-send-2','alice','source','resource2','remote2','v1','accepted','{}','conflict');
 INSERT INTO calendar_response_requests(user_id,source_id,remote_id,version,response,claim_id) VALUES('alice','source','remote','v1','accepted','preserved-claim');
 INSERT INTO gofer_provider_draft_states(account_id,draft_key,provider,mailbox_subject,folder_id) VALUES(?,'provider-draft','gmail','original-subject','drafts');
 INSERT INTO gofer_provider_draft_operations(account_id,draft_key,kind,status) VALUES(?,'provider-draft','delete','blocked');
 UPDATE outgoing_sends SET draft_id='provider-draft' WHERE id='same-send-3';
 UPDATE outgoing_sends SET status='ambiguous' WHERE id='same-send-4';
 UPDATE outgoing_sends SET sent_copy_status='pending' WHERE id='same-send-5';
 UPDATE outgoing_sends SET mime_data=X'01' WHERE id='same-send-6';`, a, a, a)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	result, err := r.PruneRetainedUserMailJobs(t.Context(), "alice", time.Now(), 100)
	if err != nil || result.OutgoingSends != 1 {
		t.Fatal("retained disabled owner pruning", result, err)
	}
	if err := r.withStartupStore(t.Context(), "alice", func(db *DB) error {
		var sends, claims int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM outgoing_sends`).Scan(&sends); err != nil {
			return err
		}
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_response_requests WHERE claim_id='preserved-claim'`).Scan(&claims); err != nil {
			return err
		}
		if sends != 6 || claims != 1 {
			t.Fatal("recovery work lost", sends, claims)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.WithUser(t.Context(), "bob", func(db *DB) error {
		var n int
		err := db.Read().QueryRow(`SELECT COUNT(*) FROM outgoing_sends`).Scan(&n)
		if n != 1 {
			t.Error("foreign same-ID send pruned", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"admin", "alice"} {
		if owner == "alice" {
			_, err = system.Write().Exec(`UPDATE users SET status='disabled',deletion_pending=1 WHERE id='alice'`)
			if err != nil {
				t.Fatal(err)
			}
		}
		result, err = r.PruneRetainedUserMailJobs(t.Context(), owner, time.Now(), 100)
		if err == nil || result.Total() != 0 {
			t.Fatal("invalid maintenance owner admitted", owner, result, err)
		}
	}
}

func TestRetainedUserMailRetentionDoesNotCreateOrReplaceStores(t *testing.T) {
	_, stores, r := newAccountRoutingTest(t, 1)
	result, err := r.PruneRetainedUserMailJobs(t.Context(), "alice", time.Now(), 100)
	if err != nil || result.Total() != 0 {
		t.Fatal("unused owner", result, err)
	}
	if _, err := os.Lstat(stores.userPath("alice")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unused store created", err)
	}
	seedOwnedRetention(t, r, "alice", 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// Closing/reopening the manager makes the missing-file test independent of
	// any cached connection still retaining the old inode.
	if err := stores.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(stores.userPath("alice")); err != nil {
		t.Fatal(err)
	}
	replacement := newUserStoreTestManager(t, r.System(), UserStoreOptions{MaxOpen: 1})
	rr, err := NewAccountRouting(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rr.PruneRetainedUserMailJobs(t.Context(), "alice", time.Now(), 100); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing historical store accepted", err)
	}
	if _, err := os.Lstat(replacement.userPath("alice")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing store replaced", err)
	}
}

func TestRetainedUserMailRetentionRechecksWriterWaitAndLateGuard(t *testing.T) {
	system, _, r := newAccountRoutingTest(t, 1)
	seedOwnedRetention(t, r, "alice", 1)
	if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
		tx, err := db.Write().BeginTx(t.Context(), nil)
		if err != nil {
			return err
		}
		previousWaits := db.Write().Stats().WaitCount
		done := make(chan error, 1)
		go func() { _, err := r.PruneRetainedUserMailJobs(t.Context(), "alice", time.Now(), 100); done <- err }()
		deadline := time.Now().Add(5 * time.Second)
		for {
			waiting := db.Write().Stats().WaitCount > previousWaits
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				tx.Rollback()
				return errors.New("retention did not reach writer wait")
			}
			time.Sleep(time.Millisecond)
		}
		if _, err := system.Write().Exec(`UPDATE users SET status='disabled',deletion_pending=1 WHERE id='alice'`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Rollback(); err != nil {
			return err
		}
		if err := <-done; !errors.Is(err, ErrUserStoreOwner) {
			return errors.New("revoked maintenance passed writer wait")
		}
		if _, err := system.Write().Exec(`UPDATE users SET status='active',deletion_pending=0 WHERE id='alice'`); err != nil {
			return err
		}
		calls := 0
		result, err := db.pruneDurableMailJobs(t.Context(), time.Now(), 100, func(*sql.Tx) error {
			calls++
			if calls > 1 {
				return ErrUserStoreOwner
			}
			return nil
		})
		if !errors.Is(err, ErrUserStoreOwner) || result.Total() != 0 {
			return errors.New("late guard did not roll back prune")
		}
		var n int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM outgoing_sends`).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return errors.New("revoked retention deleted send")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
