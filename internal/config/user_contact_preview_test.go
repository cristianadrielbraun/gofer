package config

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func seedOwnedPreview(t *testing.T, accounts *UserAccountStore, owner, account string) {
	t.Helper()
	if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.SaveContact(t.Context(), owner, models.Contact{ID: "preview-recipient", Email: owner + "-recipient@example.com", SaveTargets: []string{"local", "book:" + owner + "-book"}})
		if err != nil {
			return err
		}
		_, err = db.SaveContactProfile(t.Context(), owner, models.ContactProfile{ID: "preview-donor", DisplayName: owner + " donor", PrimaryEmail: owner + "-donor@example.com",
			Cards:  []models.ContactCard{{ID: "preview-card", Kind: "provider", Provider: "carddav", AccountID: account, AddressBookID: owner + "-book", RemoteID: "https://" + owner + ".test/book/contact.vcf", Etag: owner + "-tag", RawPayload: owner + "-raw"}},
			Fields: []models.ContactField{{ID: "preview-email", Kind: "email", Value: owner + "-donor@example.com", Source: "synced:" + account, CardID: "preview-card"}, {ID: "preview-phone", Kind: "phone", Value: owner + "-phone", Source: "synced:" + account}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func previewValues(snapshot *UserContactPreviewSnapshot) []storage.ContactSetupCandidateValue {
	var values []storage.ContactSetupCandidateValue
	for _, selection := range snapshot.Selections() {
		values = append(values, storage.ContactSetupCandidateValue{AccountID: selection.AccountID, RemoteID: selection.RemoteID, Contact: models.Contact{Email: "untrusted@example.com"}})
	}
	return values
}

func TestUserContactPreviewOwnerBindingsSurviveCacheEviction(t *testing.T) {
	system, routing, accounts, owners := newServiceAccountFixture(t)
	snapshots := map[string]*UserContactPreviewSnapshot{}
	for _, owner := range []string{"alice", "bob"} {
		seedOwnedPreview(t, accounts, owner, owners[owner].ID)
		var err error
		snapshots[owner], err = accounts.SnapshotContactPreview(t.Context(), owner, "preview-recipient", map[string]string{owners[owner].ID: "stored:preview-donor"})
		if err != nil {
			t.Fatal(err)
		}
		selections := snapshots[owner].Selections()
		selections[0].RemoteID = "injected"
		services := snapshots[owner].Services()
		services[0] = nil
		copy := snapshots[owner].StoredContact(owners[owner].ID)
		copy.Email = "injected@example.com"
	}
	other, err := NewUserAccountStore(routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []*UserContactPreviewSnapshot{nil, {}, snapshots["alice"]} {
		if err := other.ValidateContactPreview(t.Context(), snapshot); !errors.Is(err, storage.ErrContactEditChanged) {
			t.Fatal("foreign repository validated snapshot", err)
		}
		if result, err := other.PublishContactPreview(t.Context(), snapshot, nil); !errors.Is(err, storage.ErrContactEditChanged) || result.Contact.ID != "" {
			t.Fatal("foreign repository published snapshot", err)
		}
	}
	for _, owner := range []string{"alice", "bob"} {
		snapshot := snapshots[owner]
		if err := accounts.ValidateContactPreview(t.Context(), snapshot); err != nil {
			t.Fatal(err)
		}
		result, err := accounts.PublishContactPreview(t.Context(), snapshot, previewValues(snapshot))
		if err != nil || result.Contact.ID != "preview-recipient" || result.Contact.GoferSyncEnabled {
			t.Fatal("wrong owner preview", owner, result, err)
		}
		if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
			var phone, email, raw, profile string
			if err := db.Read().QueryRow(`SELECT value FROM contact_fields WHERE profile_id='preview-recipient' AND kind='phone' AND source=?`, "synced:"+owners[owner].ID).Scan(&phone); err != nil {
				return err
			}
			if err := db.Read().QueryRow(`SELECT value FROM contact_fields WHERE profile_id='preview-recipient' AND kind='email' AND source=?`, "synced:"+owners[owner].ID).Scan(&email); err != nil {
				return err
			}
			if err := db.Read().QueryRow(`SELECT profile_id,raw_payload FROM contact_cards WHERE id='preview-card'`).Scan(&profile, &raw); err != nil {
				return err
			}
			if phone != owner+"-phone" || email != owner+"-donor@example.com" || raw != owner+"-raw" || profile != "preview-recipient" {
				t.Fatal("cache eviction crossed owners or lost metadata", owner, phone, email, raw, profile)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"contact_profiles", "contact_fields", "contact_cards", "contact_sync_operations"} {
		var count int
		if err := system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("central preview fallback", table, count, err)
		}
	}
}

func TestUserContactPreviewRechecksServicesAfterWriterWait(t *testing.T) {
	for _, change := range []string{"disabled", "account-deleting", "configuration", "cursor"} {
		t.Run(change, func(t *testing.T) {
			system, routing, accounts, owners := newServiceAccountFixture(t)
			seedOwnedPreview(t, accounts, "alice", owners["alice"].ID)
			snapshot, err := accounts.SnapshotContactPreview(t.Context(), "alice", "preview-recipient", map[string]string{owners["alice"].ID: "stored:preview-donor"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			type outcome struct {
				result storage.ContactPreviewResult
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
				go func() {
					result, err := accounts.PublishContactPreview(ctx, snapshot, previewValues(snapshot))
					done <- outcome{result, err}
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
				switch change {
				case "disabled":
					_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
				case "account-deleting":
					err = routing.RequestAccountDeletion(ctx, "alice", owners["alice"].ID)
				case "configuration":
					_, err = tx.Exec(`UPDATE account_contact_sync_configs SET username='changed' WHERE account_id=?`, owners["alice"].ID)
				case "cursor":
					_, err = tx.Exec(`UPDATE account_contact_address_books SET last_sync_token='changed' WHERE account_id=?`, owners["alice"].ID)
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
				if out.err == nil || out.result.Contact.ID != "" || len(out.result.Fields) != 0 {
					t.Fatal("late preview escaped", change, out)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if change == "disabled" {
				if _, err := system.Write().Exec(`UPDATE users SET status='active' WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
				var profile string
				var fields int
				if err := db.Read().QueryRow(`SELECT profile_id FROM contact_cards WHERE id='preview-card'`).Scan(&profile); err != nil {
					return err
				}
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_fields WHERE profile_id='preview-recipient' AND source=?`, "synced:"+owners["alice"].ID).Scan(&fields); err != nil {
					return err
				}
				if profile != "preview-donor" || fields != 0 {
					t.Fatal("rejected preview partially published", profile, fields)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
