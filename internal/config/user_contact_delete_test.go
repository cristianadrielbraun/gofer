package config

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserContactDeletePrivateSourcesSurviveOwnerCacheEviction(t *testing.T) {
	system, routing, accounts, owners := newServiceAccountFixture(t)
	snapshots := map[string]*UserContactDeleteSnapshot{}
	for _, owner := range []string{"alice", "bob"} {
		seedOwnedPreview(t, accounts, owner, owners[owner].ID)
		var err error
		snapshots[owner], err = accounts.SnapshotContactDelete(t.Context(), owner, "preview-donor")
		if err != nil {
			t.Fatal(err)
		}
		cards := snapshots[owner].Sources()
		cards[0].AccountID = owners["bob"].ID
		cards[0].RemoteID = "https://foreign.test/card.vcf"
		services := snapshots[owner].Services()
		services[0] = nil
		card, service, err := snapshots[owner].Source("preview-card")
		if err != nil || card.AccountID != owners[owner].ID || card.RemoteID != "https://"+owner+".test/book/contact.vcf" || service.OwnerID() != owner {
			t.Fatal("public card/slice changed authority", card, err)
		}
	}
	other, err := NewUserAccountStore(routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []*UserContactDeleteSnapshot{nil, {}, snapshots["alice"]} {
		if err := other.ValidateContactDelete(t.Context(), snapshot); !errors.Is(err, storage.ErrContactEditChanged) {
			t.Fatal("foreign facade validated deletion", err)
		}
		if next, err := other.AcknowledgeContactDeleteSource(t.Context(), snapshot, "preview-card"); !errors.Is(err, storage.ErrContactEditChanged) || next != nil {
			t.Fatal("foreign facade acknowledged deletion", err)
		}
		if id, err := other.FinishContactDelete(t.Context(), snapshot); !errors.Is(err, storage.ErrContactEditChanged) || id != "" {
			t.Fatal("foreign facade finalized deletion", err)
		}
	}
	for _, owner := range []string{"alice", "bob"} {
		snapshot := snapshots[owner]
		if err := accounts.ValidateContactDelete(t.Context(), snapshot); err != nil {
			t.Fatal(err)
		}
		next, err := accounts.AcknowledgeContactDeleteSource(t.Context(), snapshot, "preview-card")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := next.Source("preview-card"); !errors.Is(err, storage.ErrContactEditChanged) {
			t.Fatal("acknowledged source still authorizes provider call", err)
		}
		if id, err := accounts.FinishContactDelete(t.Context(), next); err != nil || id != "preview-donor" {
			t.Fatal("owned deletion", owner, id, err)
		}
		if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
			var deleted int
			if err := db.Read().QueryRow(`SELECT is_deleted FROM contact_profiles WHERE id='preview-donor'`).Scan(&deleted); err != nil {
				return err
			}
			if deleted != 1 {
				t.Fatal("owner deletion missed", owner)
			}
			contact, err := db.GetContact(t.Context(), owner, "preview-recipient")
			if err == nil && (contact == nil || contact.Email != owner+"-recipient@example.com") {
				t.Fatal("deletion crossed profile/owner", owner, contact)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"contact_profiles", "contact_cards", "contact_activity_events"} {
		var count int
		if err := system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("delete central fallback", table, count, err)
		}
	}
}

func TestUserContactDeleteRevalidatesAfterWriterWait(t *testing.T) {
	for _, change := range []string{"disabled", "deleting", "settings", "profile", "policy"} {
		t.Run(change, func(t *testing.T) {
			system, _, accounts, owners := newServiceAccountFixture(t)
			seedOwnedPreview(t, accounts, "alice", owners["alice"].ID)
			snapshot, err := accounts.SnapshotContactDelete(t.Context(), "alice", "preview-donor")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
				tx, err := db.Write().BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				before := db.Write().Stats().WaitCount
				type result struct {
					next *UserContactDeleteSnapshot
					err  error
				}
				done := make(chan result, 1)
				go func() {
					next, err := accounts.AcknowledgeContactDeleteSource(ctx, snapshot, "preview-card")
					done <- result{next, err}
				}()
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for db.Write().Stats().WaitCount == before {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-tick.C:
					}
				}
				var expected error
				switch change {
				case "disabled":
					_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
					expected = storage.ErrUserStoreOwner
				case "deleting":
					_, err = system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, owners["alice"].ID)
					expected = storage.ErrAccountRoute
				case "settings":
					_, err = tx.Exec(`UPDATE account_contact_sync_configs SET username='replacement' WHERE user_id='alice'`)
					expected = ErrAccountServicesChanged
				case "profile":
					_, err = tx.Exec(`UPDATE contact_profiles SET avatar_url='replacement' WHERE id='preview-donor'`)
					expected = storage.ErrContactEditChanged
				case "policy":
					_, err = tx.Exec(`INSERT INTO app_settings(user_id,key,value) VALUES('alice','ui_settings','{"contacts_prevent_recreate_deleted":"false"}')`)
					expected = storage.ErrContactEditChanged
				}
				if err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				select {
				case got := <-done:
					if got.next != nil || !errors.Is(got.err, expected) {
						t.Fatal("writer wait missed changed deletion state", change, got.err)
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				var profile string
				if err := db.Read().QueryRow(`SELECT profile_id FROM contact_cards WHERE id='preview-card'`).Scan(&profile); err != nil {
					return err
				}
				if profile != "preview-donor" {
					t.Fatal("failed ack deleted source")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
