package storage

import (
	"testing"
	"time"
)

func TestAccountServiceCalendarWakeReservationAndStartupFence(t *testing.T) {
	_, _, r := newAccountRoutingTest(t, 1)
	account := createRoutingTestAccount(t, r, "alice")
	now := time.Now()
	revision, reserved, forced, err := r.ReserveServiceAccountWithWake(t.Context(), "alice", account.AccountID, ScheduledCalendar, now, now.Add(time.Minute), false)
	if err != nil || !reserved || !forced {
		t.Fatal("initial/calendar wake", revision, reserved, forced, err)
	}
	if ok, err := r.CompleteServiceAccount(t.Context(), "alice", account.AccountID, ScheduledCalendar, revision, now.Add(-time.Second)); err != nil || !ok {
		t.Fatal(err)
	}
	revision, reserved, forced, err = r.ReserveServiceAccountWithWake(t.Context(), "alice", account.AccountID, ScheduledCalendar, now, now.Add(time.Minute), false)
	if err != nil || !reserved || forced {
		t.Fatal("cadence mistaken for forced wake", reserved, forced, err)
	}
	if err := r.ResetCalendarSchedulesForStartup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ok, err := r.CompleteServiceAccount(t.Context(), "alice", account.AccountID, ScheduledCalendar, revision, now.Add(time.Hour)); err != nil || ok {
		t.Fatal("startup wake overwritten", ok, err)
	}
	_, reserved, forced, err = r.ReserveServiceAccountWithWake(t.Context(), "alice", account.AccountID, ScheduledCalendar, now, now.Add(time.Minute), false)
	if err != nil || !reserved || !forced {
		t.Fatal("startup wake not captured", reserved, forced, err)
	}
}

func TestAccountServiceScheduleActivationHintFailureRecoversLocalAccount(t *testing.T) {
	system, _, r := newAccountRoutingTest(t, 1)
	entry, err := r.ReserveAccount(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := system.Write().Exec(`CREATE TRIGGER reject_activation_hint BEFORE INSERT ON gofer_contact_queue_schedule BEGIN SELECT RAISE(ABORT,'synthetic queue hint failure'); END`); err != nil {
		t.Fatal(err)
	}
	creates := 0
	create := func(db *DB) error {
		creates++
		_, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,email_address) VALUES(?,'alice','hint@example.com')`, entry.AccountID)
		return err
	}
	if err := r.CompleteAccountCreation(t.Context(), "alice", entry.AccountID, create); err == nil {
		t.Fatal("activation succeeded without durable queue hint")
	}
	state, err := r.AccountStateForUser(t.Context(), "alice", entry.AccountID)
	if err != nil || state != AccountCreating {
		t.Fatal("failed hint escaped account activation", state, err)
	}
	var hints int
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_contact_queue_schedule WHERE user_id='alice'`).Scan(&hints); err != nil || hints != 0 {
		t.Fatal("failed activation left hint", hints, err)
	}
	if _, err := system.Write().Exec(`DROP TRIGGER reject_activation_hint`); err != nil {
		t.Fatal(err)
	}
	if err := r.CompleteAccountCreation(t.Context(), "alice", entry.AccountID, create); err != nil {
		t.Fatal(err)
	}
	if creates != 1 {
		t.Fatal("hint recovery recreated committed account", creates)
	}
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_contact_queue_schedule WHERE user_id='alice' AND next_due_ms=0`).Scan(&hints); err != nil || hints != 1 {
		t.Fatal("activation did not retain discoverable owner", hints, err)
	}
}

func TestAccountServiceScheduleOlderLayoutSeedsHintsWithoutOpeningStores(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 1)
	createRoutingTestAccount(t, r, "alice")
	createRoutingTestAccount(t, r, "bob")
	now := time.Now()
	revision, reserved, err := r.ReserveContactQueue(t.Context(), "alice", now, now.Add(time.Hour), false)
	if err != nil || !reserved {
		t.Fatal("reserve", err)
	}
	if _, err := system.Write().Exec(`DELETE FROM gofer_contact_queue_schedule WHERE user_id='bob'`); err != nil {
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
	stores.mu.Lock()
	opened := len(stores.entries)
	stores.mu.Unlock()
	if opened != 0 {
		t.Fatal("metadata bootstrap opened user stores", opened)
	}
	var due, got int64
	if err := system.Read().QueryRow(`SELECT next_due_ms,revision FROM gofer_contact_queue_schedule WHERE user_id='alice'`).Scan(&due, &got); err != nil || due != now.Add(time.Hour).UnixMilli() || got != revision {
		t.Fatal("bootstrap replaced established deadline", due, got, err)
	}
	owners, err := r.ListDueContactQueueOwners(t.Context(), "", now, 64, false)
	if err != nil || len(owners) != 1 || owners[0] != "bob" {
		t.Fatal("missing hint was not seeded", owners, err)
	}
}

func TestAccountServiceScheduleCentralDiscoveryAndRevisionFences(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 1)
	alice := createRoutingTestAccount(t, r, "alice")
	bob := createRoutingTestAccount(t, r, "bob")
	now := time.Now()
	revision, ok, err := r.ReserveServiceAccount(t.Context(), "alice", alice.AccountID, ScheduledContacts, now, now.Add(time.Hour), false)
	if err != nil || !ok {
		t.Fatal("reserve contacts", revision, ok, err)
	}
	calendar, ok, err := r.ReserveServiceAccount(t.Context(), "alice", alice.AccountID, ScheduledCalendar, now, now.Add(time.Hour), false)
	if err != nil || !ok {
		t.Fatal("independent calendar", calendar, ok, err)
	}
	lease, err := stores.AcquireExisting(t.Context(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	due, err := r.ListDueServiceAccounts(t.Context(), ScheduledContacts, "", now, 64, false)
	if err != nil || len(due) != 1 || due[0].AccountID != bob.AccountID {
		t.Fatal("discovery acquired pinned cache or ignored deadline", due, err)
	}
	if err := r.ResetServiceAccounts(t.Context(), "alice", alice.AccountID, ScheduledContacts); err != nil {
		t.Fatal(err)
	}
	if updated, err := r.CompleteServiceAccount(t.Context(), "alice", alice.AccountID, ScheduledContacts, revision, now.Add(2*time.Hour)); err != nil || updated {
		t.Fatal("completion lost a wake", updated, err)
	}
	if _, ok, err := r.ReserveServiceAccount(t.Context(), "bob", alice.AccountID, ScheduledContacts, now, now.Add(time.Hour), false); err != nil || ok {
		t.Fatal("foreign reservation", ok, err)
	}
	if err := r.ResetServiceAccounts(t.Context(), "bob", alice.AccountID, ScheduledContacts); err != ErrAccountRoute {
		t.Fatal("foreign wake", err)
	}
	until := now.Add(2 * time.Hour)
	if err := r.DeferProviderRetry(t.Context(), "bob", bob.AccountID, until); err != nil {
		t.Fatal(err)
	}
	if err := r.ResetServiceAccounts(t.Context(), "bob", "", ScheduledContacts); err != nil {
		t.Fatal(err)
	}
	due, err = r.ListDueServiceAccounts(t.Context(), ScheduledContacts, "", now, 64, true)
	if err != nil || len(due) != 1 || due[0].UserID != "alice" {
		t.Fatal("startup/wake bypassed cooldown", due, err)
	}
	if _, ok, err := r.ReserveServiceAccount(t.Context(), "bob", bob.AccountID, ScheduledContacts, now, now.Add(time.Hour), true); err != nil || ok {
		t.Fatal("reservation bypassed cooldown", ok, err)
	}
	queueRevision, ok, err := r.ReserveContactQueue(t.Context(), "alice", now, now.Add(time.Hour), false)
	if err != nil || !ok {
		t.Fatal("reserve queue", queueRevision, ok, err)
	}
	if err := r.ResetContactQueue(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	if updated, err := r.CompleteContactQueue(t.Context(), "alice", queueRevision, now.Add(2*time.Hour)); err != nil || updated {
		t.Fatal("queue completion lost a wake", updated, err)
	}
	queueRevision, ok, err = r.ReserveContactQueue(t.Context(), "alice", now, now.Add(time.Hour), false)
	if err != nil || !ok {
		t.Fatal("queue wake unavailable", queueRevision, ok, err)
	}
	if err := r.ResetContactSchedulesForStartup(t.Context()); err != nil {
		t.Fatal(err)
	}
	owners, err := r.ListDueContactQueueOwners(t.Context(), "", now, 64, false)
	if err != nil || len(owners) != 2 {
		t.Fatal("outbound discovery inherited provider cooldown", owners, err)
	}
	if updated, err := r.CompleteServiceAccount(t.Context(), "alice", alice.AccountID, ScheduledCalendar, calendar, now.Add(time.Hour)); err != nil || !updated {
		t.Fatal("contacts startup reset calendar", updated, err)
	}
	if _, err := system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	owners, err = r.ListDueContactQueueOwners(t.Context(), "", now, 64, false)
	if err != nil || len(owners) != 1 || owners[0] != "alice" {
		t.Fatal("disabled owner discovered", owners, err)
	}
	if err := r.RequestAccountDeletion(t.Context(), "alice", alice.AccountID); err != nil {
		t.Fatal(err)
	}
	due, err = r.ListDueServiceAccounts(t.Context(), ScheduledContacts, "", now, 64, false)
	if err != nil || len(due) != 0 {
		t.Fatal("deleting account discovered", due, err)
	}
}

func TestAccountServiceScheduleRejectsInvalidAndFailedReservations(t *testing.T) {
	system, _, r := newAccountRoutingTest(t, 1)
	alice := createRoutingTestAccount(t, r, "alice")
	now := time.Now()
	if _, err := r.ListDueServiceAccounts(t.Context(), "mail", "", now, 1, false); err == nil {
		t.Fatal("unknown domain accepted")
	}
	if _, err := r.ListDueContactQueueOwners(t.Context(), "", now, 65, false); err == nil {
		t.Fatal("unbounded discovery")
	}
	if _, ok, err := r.ReserveContactQueue(t.Context(), "alice", now, now, false); err == nil || ok {
		t.Fatal("invalid reservation accepted")
	}
	if _, err := system.Write().Exec(`CREATE TRIGGER reject_service_reservation BEFORE INSERT ON gofer_account_service_schedule BEGIN SELECT RAISE(ABORT,'reservation failure'); END`); err != nil {
		t.Fatal(err)
	}
	if revision, ok, err := r.ReserveServiceAccount(t.Context(), "alice", alice.AccountID, ScheduledContacts, now, now.Add(time.Hour), false); err == nil || ok || revision != 0 {
		t.Fatal("failed reservation escaped", revision, ok, err)
	}
	if _, err := system.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('empty-owner','empty-owner','empty-owner')`); err != nil {
		t.Fatal(err)
	}
	if err := r.ResetContactQueue(t.Context(), "empty-owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := system.Write().Exec(`DELETE FROM users WHERE id='empty-owner'`); err != nil {
		t.Fatal("queue metadata blocked user removal", err)
	}
	var count int
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_contact_queue_schedule WHERE user_id='empty-owner'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("owner queue metadata survived deletion", count, err)
	}
}
