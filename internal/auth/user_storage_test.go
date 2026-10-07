package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newOwnedDeletionManager(t *testing.T) (*Manager, *storage.AccountRouting, *Session, string) {
	t.Helper()
	now := time.Now().UTC()
	m := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, m, "administrator", true, now)
	insertActiveUser(t, m, "person", false, now)
	insertPolicyTestTOTP(t, m, "administrator", now)
	session := createPolicyAdministratorSession(t, m)
	stores, err := storage.NewUserStores(m.db, storage.UserStoreOptions{MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := stores.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	r, err := storage.NewAccountRouting(stores)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := r.ReserveAccount(t.Context(), "person")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CompleteAccountCreation(t.Context(), "person", entry.AccountID, func(db *storage.DB) error {
		_, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,email_address) VALUES(?,'person','private@example.com')`, entry.AccountID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Write().Exec(`UPDATE users SET status='disabled' WHERE id='person'`); err != nil {
		t.Fatal(err)
	}
	return m, r, session, entry.AccountID
}

func ownedDeletionOptions(s *Session) PrepareAdministratorUserDeletionOptions {
	return PrepareAdministratorUserDeletionOptions{ActorUserID: s.UserID, ActorSessionID: s.ID, TargetUserID: "person", Confirmation: "person"}
}

func TestOwnedAdministratorDeletionRequiresCleanupReceipt(t *testing.T) {
	m, r, s, id := newOwnedDeletionManager(t)
	if err := m.SetUserStorage(r); err != nil {
		t.Fatal(err)
	}
	if err := m.SetUserStorage(r); err != nil {
		t.Fatal("same coordinator not idempotent", err)
	}
	result, err := m.PrepareAdministratorUserDeletion(t.Context(), ownedDeletionOptions(s))
	if err != nil || result == nil || len(result.AccountIDs) != 1 || result.AccountIDs[0] != id {
		t.Fatal(result, err)
	}
	if state, err := r.AccountStateForUser(t.Context(), "person", id); err != nil || state != storage.AccountDeleting {
		t.Fatal(state, err)
	}
	for _, stage := range []string{"marked", "database removed", "account removed"} {
		switch stage {
		case "database removed":
			if err := r.DrainPendingUserAccount(t.Context(), "person", id); err != nil {
				t.Fatal(err)
			}
			if err := r.RemovePendingUserStore(t.Context(), "person"); err != nil {
				t.Fatal(err)
			}
		case "account removed":
			if err := r.RemovePendingUserAccount(t.Context(), "person", id, func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
		}
		if deleted, err := m.CompleteAdministratorUserDeletion(t.Context(), "person", s.ID); deleted || !errors.Is(err, ErrAdministratorUserDeletionIncomplete) {
			t.Fatal("premature identity removal", stage, deleted, err)
		}
	}
	// Resuming after database removal never downgrades its durable receipt.
	result, err = m.PrepareAdministratorUserDeletion(t.Context(), ownedDeletionOptions(s))
	if err != nil || !result.Resumed || len(result.AccountIDs) != 0 {
		t.Fatal(result, err)
	}
	if removed, err := r.PendingUserStoreRemoved(t.Context(), "person"); err != nil || !removed {
		t.Fatal("retry downgraded receipt", removed, err)
	}
	if err := r.CompletePendingUserCleanup(t.Context(), "person", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if deleted, err := m.CompleteAdministratorUserDeletion(t.Context(), "person", s.ID); err != nil || !deleted {
		t.Fatal(deleted, err)
	}
	if deleted, err := m.CompleteAdministratorUserDeletion(t.Context(), "person", ""); err != nil || deleted {
		t.Fatal("idempotent completion", deleted, err)
	}
	var starts, finishes int
	if err := m.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE event_type=?`, AuthEventUserDeletionStarted).Scan(&starts); err != nil {
		t.Fatal(err)
	}
	if err := m.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE event_type=?`, AuthEventUserDeleted).Scan(&finishes); err != nil || starts != 1 || finishes != 1 {
		t.Fatal("audit counts", starts, finishes, err)
	}
}

func TestOwnedAdministratorDeletionMarkFailureRollsBackIntentAndAudit(t *testing.T) {
	m, r, s, id := newOwnedDeletionManager(t)
	if err := m.SetUserStorage(r); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Write().Exec(`CREATE TRIGGER reject_owned_delete BEFORE UPDATE OF state ON gofer_account_directory WHEN NEW.state='deleting' BEGIN SELECT RAISE(ABORT,'injected directory failure'); END`); err != nil {
		t.Fatal(err)
	}
	if result, err := m.PrepareAdministratorUserDeletion(t.Context(), ownedDeletionOptions(s)); err == nil || result != nil {
		t.Fatal("injected failure accepted", result, err)
	}
	var pending, starts int
	var state string
	if err := m.db.Read().QueryRow(`SELECT deletion_pending FROM users WHERE id='person'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := m.db.Read().QueryRow(`SELECT state FROM gofer_user_store_directory WHERE user_id='person'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := m.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE event_type=?`, AuthEventUserDeletionStarted).Scan(&starts); err != nil {
		t.Fatal(err)
	}
	accountState, err := r.AccountStateForUser(t.Context(), "person", id)
	if err != nil || pending != 0 || starts != 0 || state != "present" || accountState != storage.AccountActive {
		t.Fatal("partial deletion transaction", pending, starts, state, accountState, err)
	}
}

func TestUserStorageRequiresExplicitManagedConfiguration(t *testing.T) {
	m, r, s, id := newOwnedDeletionManager(t)
	if _, err := m.db.Write().Exec(`INSERT INTO accounts(id,user_id,email_address) VALUES('shared-person','person','shared@example.com')`); err != nil {
		t.Fatal(err)
	}
	result, err := m.PrepareAdministratorUserDeletion(t.Context(), ownedDeletionOptions(s))
	if err != nil || result == nil || len(result.AccountIDs) != 1 || result.AccountIDs[0] != "shared-person" {
		t.Fatal("table presence selected owned layout", result, err)
	}
	if state, err := r.AccountStateForUser(t.Context(), "person", id); err != nil || state != storage.AccountActive {
		t.Fatal("shared deletion marked owned directory", state, err)
	}
	if err := m.SetUserStorage(nil); err == nil {
		t.Fatal("nil coordinator accepted")
	}
	for _, mode := range []Mode{ModeOpen, ModePersonal} {
		other := NewManager(&Config{Mode: mode}, m.db)
		if err := other.SetUserStorage(r); err == nil {
			t.Fatal("non-managed layout accepted", mode)
		}
	}
	foreign := newDeterministicManager(t, &fixedClock{now: time.Now().UTC()}, secureTokenGenerator{})
	if err := foreign.SetUserStorage(r); err == nil {
		t.Fatal("foreign system coordinator accepted")
	}
}
