package storage

import (
	"sync"
	"testing"
	"time"
)

func TestActivePollingUsesConnectedOwnersAndCentralMetadata(t *testing.T) {
	_, stores, r := newAccountRoutingTest(t, 1)
	alice := createRoutingTestAccount(t, r, "alice")
	createRoutingTestAccount(t, r, "bob")
	lease, err := stores.AcquireExisting(t.Context(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	now := time.Now().UTC()
	connected := []ActivePollOwner{{UserID: "alice", Since: now.Add(-time.Second)}}
	page, err := r.ListActivePollAccounts(t.Context(), connected, "", now, 1)
	if err != nil || len(page) != 1 || page[0].AccountID != alice.AccountID {
		t.Fatalf("connected owner page: %+v %v", page, err)
	}
	reservation, ok, err := r.ReserveActivePoll(t.Context(), "alice", alice.AccountID, connected[0].Since, now, now.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("reserve with other store pinned: %+v %v %v", reservation, ok, err)
	}
	if _, ok, err := r.ReserveActivePoll(t.Context(), "bob", alice.AccountID, now, now, now.Add(time.Minute)); err != nil || ok {
		t.Fatalf("foreign reserve: %v %v", ok, err)
	}
	if changed, err := r.CompleteActivePoll(t.Context(), "bob", alice.AccountID, reservation.Revision, now, 0); err != nil || changed {
		t.Fatalf("foreign completion: %v %v", changed, err)
	}
	page, err = r.ListActivePollAccounts(t.Context(), connected, "", now, 1)
	if err != nil || len(page) != 0 {
		t.Fatalf("reserved deadline: %+v %v", page, err)
	}
}
func TestActivePollingReconnectDoesNotBypassBackoffOrProviderCooldown(t *testing.T) {
	_, _, r := newAccountRoutingTest(t, 1)
	alice := createRoutingTestAccount(t, r, "alice")
	now := time.Now().UTC()
	first := now.Add(-time.Second)
	reservation, ok, err := r.ReserveActivePoll(t.Context(), "alice", alice.AccountID, first, now, now.Add(time.Minute))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	newer := now.Add(time.Second)
	owners := []ActivePollOwner{{UserID: "alice", Since: newer}}
	page, err := r.ListActivePollAccounts(t.Context(), owners, "", newer, 8)
	if err != nil || len(page) != 1 {
		t.Fatalf("new session must advance successful deadline: %+v %v", page, err)
	}
	if _, err := r.CompleteActivePoll(t.Context(), "alice", alice.AccountID, reservation.Revision, now.Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	page, err = r.ListActivePollAccounts(t.Context(), owners, "", newer, 8)
	if err != nil || len(page) != 0 {
		t.Fatalf("new session bypassed backoff: %+v %v", page, err)
	}
	if _, ok, err := r.ReserveActivePoll(t.Context(), "alice", alice.AccountID, newer, newer, newer.Add(time.Minute)); err != nil || ok {
		t.Fatalf("new session reserved backoff: %v %v", ok, err)
	}
	until := now.Add(2 * time.Hour)
	if err := r.DeferProviderRetry(t.Context(), "alice", alice.AccountID, until); err != nil {
		t.Fatal(err)
	}
	if err := r.ResetActivePolling(t.Context(), "alice", alice.AccountID); err != nil {
		t.Fatal(err)
	}
	page, err = r.ListActivePollAccounts(t.Context(), owners, "", newer, 8)
	if err != nil || len(page) != 0 {
		t.Fatalf("settings reset bypassed provider cooldown: %+v %v", page, err)
	}
	if _, ok, err := r.ReserveActivePoll(t.Context(), "alice", alice.AccountID, newer, newer, newer.Add(time.Minute)); err != nil || ok {
		t.Fatalf("cooldown reserve: %v %v", ok, err)
	}
	page, err = r.ListActivePollAccounts(t.Context(), owners, "", until.Add(time.Second), 8)
	if err != nil || len(page) != 1 {
		t.Fatalf("cooldown expiry: %+v %v", page, err)
	}
}
func TestActivePollingResetAndDeletionRejectLateCompletion(t *testing.T) {
	system, _, r := newAccountRoutingTest(t, 1)
	alice := createRoutingTestAccount(t, r, "alice")
	bob := createRoutingTestAccount(t, r, "bob")
	now := time.Now().UTC()
	reservation, ok, err := r.ReserveActivePoll(t.Context(), "alice", alice.AccountID, now.Add(-time.Second), now, now.Add(time.Minute))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := r.ResetActivePolling(t.Context(), "alice", ""); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.CompleteActivePoll(t.Context(), "alice", alice.AccountID, reservation.Revision, now.Add(time.Hour), 4); err != nil || changed {
		t.Fatalf("stale completion overwrote reset: %v %v", changed, err)
	}
	owners := []ActivePollOwner{{UserID: "alice", Since: now}, {UserID: "bob", Since: now}}
	page, err := r.ListActivePollAccounts(t.Context(), owners, "", now, 1)
	if err != nil || len(page) != 1 {
		t.Fatal(page, err)
	}
	second, err := r.ListActivePollAccounts(t.Context(), owners, page[0].AccountID, now, 1)
	if err != nil || len(second) != 1 || second[0].AccountID == page[0].AccountID {
		t.Fatal(second, err)
	}
	if _, err := system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	if err := r.RequestAccountDeletion(t.Context(), "alice", alice.AccountID); err != nil {
		t.Fatal(err)
	}
	page, err = r.ListActivePollAccounts(t.Context(), owners, "", now, 8)
	if err != nil || len(page) != 0 {
		t.Fatalf("inactive/deleting accounts discovered: %+v %v", page, err)
	}
	if _, ok, err := r.ReserveActivePoll(t.Context(), "bob", bob.AccountID, now, now, now); err != nil || ok {
		t.Fatalf("inactive owner reserved: %v %v", ok, err)
	}
	if changed, err := r.CompleteActivePoll(t.Context(), "alice", alice.AccountID, reservation.Revision+1, now, 0); err != nil || changed {
		t.Fatalf("deleting completion: %v %v", changed, err)
	}
}
func TestActivePollingConcurrentReservationHasOneWinner(t *testing.T) {
	_, _, r := newAccountRoutingTest(t, 1)
	alice := createRoutingTestAccount(t, r, "alice")
	now := time.Now().UTC()
	results := make(chan bool, 12)
	var workers sync.WaitGroup
	for i := 0; i < 12; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, ok, err := r.ReserveActivePoll(t.Context(), "alice", alice.AccountID, now.Add(-time.Second), now, now.Add(time.Minute))
			if err != nil {
				t.Error(err)
			}
			results <- ok
		}()
	}
	workers.Wait()
	close(results)
	count := 0
	for ok := range results {
		if ok {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("reservation winners: %d", count)
	}
}
