package config

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func seedOwnedContactSetup(t *testing.T, accounts *UserAccountStore, owner string) {
	t.Helper()
	if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.SaveContact(t.Context(), owner, models.Contact{ID: "same-setup-id", Email: owner + "-setup@example.com", Phone: owner + "-phone", SaveTargets: []string{"local", "book:" + owner + "-book"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserContactSetupOwnerBindingStoredTargetsAndCacheEviction(t *testing.T) {
	system, routing, accounts, _ := newServiceAccountFixture(t)
	snapshots := map[string]*UserContactEditSnapshot{}
	for _, owner := range []string{"alice", "bob"} {
		seedOwnedContactSetup(t, accounts, owner)
		var err error
		snapshots[owner], err = accounts.SnapshotContactSetup(t.Context(), owner, "same-setup-id")
		if err != nil {
			t.Fatal(err)
		}
		public := snapshots[owner].Contact()
		public.SaveTargets[1] = "book:foreign"
	}
	other, err := NewUserAccountStore(routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []*UserContactEditSnapshot{nil, {}, snapshots["alice"]} {
		if result, err := other.ConfirmContactSetup(t.Context(), snapshot, nil); !errors.Is(err, storage.ErrContactEditChanged) || result.Contact.ID != "" {
			t.Fatal("foreign repository accepted setup", err)
		}
	}
	edit, err := accounts.SnapshotContactEdit(t.Context(), "alice", models.Contact{ID: "same-setup-id", Email: "alice-setup@example.com", SaveTargets: []string{"local"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.ConfirmContactSetup(t.Context(), edit, nil); !errors.Is(err, storage.ErrContactEditChanged) {
		t.Fatal("editor proposal used to choose confirmation destinations", err)
	}
	for _, owner := range []string{"alice", "bob"} {
		result, err := accounts.ConfirmContactSetup(t.Context(), snapshots[owner], nil)
		if err != nil || !result.Contact.GoferSyncEnabled || result.Contact.Phone != owner+"-phone" || result.OperationID == "" {
			t.Fatal("confirmation crossed owner or lost copied snapshot", owner, result, err)
		}
		if len(result.Contact.SaveTargets) != 2 || result.Contact.SaveTargets[1] != "book:"+owner+"-book" {
			t.Fatal("caller mutation changed stored targets", result.Contact.SaveTargets)
		}
		if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
			var count int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations WHERE user_id=?`, owner).Scan(&count); err != nil {
				return err
			}
			if count != 1 {
				t.Fatal("owner queue", owner, count)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"contact_profiles", "contact_sync_operations", "contact_sync_memberships", "contact_activity_events"} {
		var count int
		if err := system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("central fallback", table, count, err)
		}
	}
}

func TestUserContactSetupRechecksLifecycleAndServicesAfterWriterWait(t *testing.T) {
	for _, change := range []string{"disabled", "deletion_pending", "account_deleting", "configuration", "identity", "cursor"} {
		t.Run(change, func(t *testing.T) {
			system, routing, accounts, owners := newServiceAccountFixture(t)
			seedOwnedContactSetup(t, accounts, "alice")
			snapshot, err := accounts.SnapshotContactSetup(t.Context(), "alice", "same-setup-id")
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
				go func() { result, err := accounts.ConfirmContactSetup(ctx, snapshot, nil); done <- outcome{result, err} }()
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
					_, err = tx.Exec(`UPDATE account_contact_sync_configs SET username='newer' WHERE account_id=?`, owners["alice"].ID)
				case "identity":
					_, err = tx.Exec(`UPDATE accounts SET username='newer' WHERE id=?`, owners["alice"].ID)
				case "cursor":
					_, err = tx.Exec(`UPDATE account_contact_address_books SET last_sync_token='newer' WHERE account_id=?`, owners["alice"].ID)
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
					t.Fatal("late setup commit escaped", change, out)
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
				contact, err := db.GetContact(ctx, "alice", "same-setup-id")
				if err != nil {
					return err
				}
				if contact == nil || contact.GoferSyncEnabled || contact.Phone != "alice-phone" {
					t.Fatal("rejected confirmation changed contact", contact)
				}
				var count int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&count); err != nil {
					return err
				}
				if count != 0 {
					t.Fatal("rejected confirmation queued work")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
