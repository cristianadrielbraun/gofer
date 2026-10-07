package config

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func seedContactEditProfile(t *testing.T, accounts *UserAccountStore, owner string) {
	t.Helper()
	if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.SaveContact(t.Context(), owner, models.Contact{ID: "same-edit-id", Name: owner, Email: "person@example.com", Phone: owner, GoferSyncEnabled: true, SaveTargets: []string{"local", "book:" + owner + "-book"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserContactEditOwnerBindingAndCacheEviction(t *testing.T) {
	system, routing, accounts, _ := newServiceAccountFixture(t)
	snapshots := make(map[string]*UserContactEditSnapshot)
	for _, owner := range []string{"alice", "bob"} {
		seedContactEditProfile(t, accounts, owner)
		var err error
		snapshots[owner], err = accounts.SnapshotContactEdit(t.Context(), owner, models.Contact{ID: "same-edit-id", Email: "person@example.com", Name: owner, Phone: owner + "-updated", GoferSyncEnabled: true, SaveTargets: []string{"local", "book:" + owner + "-book"}})
		if err != nil {
			t.Fatal(err)
		}
		copy := snapshots[owner].Contact()
		copy.SaveTargets[0] = "account:wrong"
		previous := snapshots[owner].Previous()
		previous.SaveTargets[0] = "account:wrong"
	}
	for _, owner := range []string{"alice", "bob"} {
		result, err := accounts.SaveContactEdit(t.Context(), snapshots[owner], false)
		if err != nil || result.Contact.Phone != owner+"-updated" || result.OperationID == "" {
			t.Fatal("wrong owner after cache eviction", owner, result, err)
		}
		if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
			var count int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations WHERE user_id=?`, owner).Scan(&count); err != nil {
				return err
			}
			if count != 1 {
				t.Fatal("owner queue count", owner, count)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	other, err := NewUserAccountStore(routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []*UserContactEditSnapshot{nil, {}, snapshots["alice"]} {
		if result, err := other.SaveContactEdit(t.Context(), snapshot, false); !errors.Is(err, storage.ErrContactEditChanged) || result.Contact.ID != "" {
			t.Fatal("unbound edit accepted", err)
		}
	}
	var central int
	for _, table := range []string{"contact_profiles", "contact_sync_operations", "contact_sync_memberships"} {
		if err := system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&central); err != nil || central != 0 {
			t.Fatal("shared fallback", table, central, err)
		}
	}
}

func TestUserContactEditRechecksLifecycleAndServicesAfterWriterWait(t *testing.T) {
	for _, change := range []string{"disabled", "deletion_pending", "account_deleting", "configuration", "identity"} {
		t.Run(change, func(t *testing.T) {
			system, routing, accounts, owners := newServiceAccountFixture(t)
			seedContactEditProfile(t, accounts, "alice")
			snapshot, err := accounts.SnapshotContactEdit(t.Context(), "alice", models.Contact{ID: "same-edit-id", Email: "person@example.com", Phone: "late", GoferSyncEnabled: true, SaveTargets: []string{"book:alice-book"}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			type outcome struct {
				result storage.ContactEditResult
				err    error
			}
			done := make(chan outcome, 1)
			err = accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
				tx, err := db.Write().BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				before := db.Write().Stats().WaitCount
				go func() { result, err := accounts.SaveContactEdit(ctx, snapshot, false); done <- outcome{result, err} }()
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for db.Write().Stats().WaitCount == before {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-tick.C:
					}
				}
				switch change {
				case "disabled":
					_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
				case "deletion_pending":
					_, err = system.Write().Exec(`UPDATE users SET status='disabled',deletion_pending=1 WHERE id='alice'`)
				case "account_deleting":
					err = routing.RequestAccountDeletion(ctx, "alice", owners["alice"].ID)
				case "configuration":
					_, err = tx.Exec(`UPDATE account_contact_sync_configs SET username='changed' WHERE account_id=?`, owners["alice"].ID)
				case "identity":
					_, err = tx.Exec(`UPDATE accounts SET username='changed' WHERE id=?`, owners["alice"].ID)
				}
				if err != nil {
					return err
				}
				return tx.Commit()
			})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case out := <-done:
				if out.err == nil || out.result.Contact.ID != "" || out.result.OperationID != "" {
					t.Fatal("late edit escaped", change, out)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if change == "disabled" || change == "deletion_pending" {
				if _, err := system.Write().Exec(`UPDATE users SET status='active',deletion_pending=0 WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
				contact, err := db.GetContact(ctx, "alice", "same-edit-id")
				if err != nil {
					return err
				}
				if contact == nil || contact.Phone != "alice" {
					t.Fatal("late fields committed", contact)
				}
				var count int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&count); err != nil {
					return err
				}
				if count != 0 {
					t.Fatal("late queue committed")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
