package storage

import (
	"testing"
	"time"
)

func TestAccountPollingMetadataAndSettingsResetRejectStaleDeadline(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 1)
	alice := createRoutingTestAccount(t, r, "alice")
	bob := createRoutingTestAccount(t, r, "bob")
	revision, err := r.PollRevision(t.Context(), "alice", alice.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if updated, err := r.DeferAccountPoll(t.Context(), "alice", alice.AccountID, revision, time.Now().Add(time.Hour)); err != nil || !updated {
		t.Fatalf("defer: %v %v", updated, err)
	}
	// Central discovery works even while another owner's file is pinned and
	// Alice cannot acquire the one-slot user cache.
	lease, err := stores.AcquireExisting(t.Context(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	due, err := r.ListDueAccounts(t.Context(), "", time.Now(), 64)
	if err != nil || len(due) != 1 || due[0].AccountID != bob.AccountID {
		t.Fatalf("due with pinned store: %#v %v", due, err)
	}
	if err := r.ResetUserPolling(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	if updated, err := r.DeferAccountPoll(t.Context(), "alice", alice.AccountID, revision, time.Now().Add(10*time.Hour)); err != nil || updated {
		t.Fatalf("stale completion overwrote reset: %v %v", updated, err)
	}
	due, err = r.ListDueAccounts(t.Context(), "", time.Now(), 64)
	if err != nil || len(due) != 2 {
		t.Fatalf("reset due: %#v %v", due, err)
	}
	if updated, err := r.DeferAccountPoll(t.Context(), "bob", alice.AccountID, revision, time.Now().Add(time.Hour)); err != nil || updated {
		t.Fatalf("foreign deadline: %v %v", updated, err)
	}
	if err := r.ResetAccountPolling(t.Context(), "bob", alice.AccountID); err != ErrAccountRoute {
		t.Fatalf("foreign account reset: %v", err)
	}
	revision, err = r.PollRevision(t.Context(), "alice", alice.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ResetAccountPolling(t.Context(), "alice", alice.AccountID); err != nil {
		t.Fatal(err)
	}
	if updated, err := r.DeferAccountPoll(t.Context(), "alice", alice.AccountID, revision, time.Now().Add(time.Hour)); err != nil || updated {
		t.Fatalf("stale completion overwrote account wake: %v %v", updated, err)
	}
	if _, err := system.Write().Exec("UPDATE users SET status='disabled' WHERE id='bob'"); err != nil {
		t.Fatal(err)
	}
	due, err = r.ListDueAccounts(t.Context(), "", time.Now(), 64)
	if err != nil || len(due) != 1 || due[0].UserID != "alice" {
		t.Fatalf("inactive owner discovered: %#v %v", due, err)
	}
	if err := r.RequestAccountDeletion(t.Context(), "alice", alice.AccountID); err != nil {
		t.Fatal(err)
	}
	due, err = r.ListDueAccounts(t.Context(), "", time.Now(), 64)
	if err != nil || len(due) != 0 {
		t.Fatalf("deleting account discovered: %#v %v", due, err)
	}
}
