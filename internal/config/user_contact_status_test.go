package config

import (
	"errors"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserContactPullStatusBuiltinCreationAndLegacyNormalization(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "legacy"}[legacy], func(t *testing.T) {
			_, _, accounts, owners := newServiceAccountFixture(t)
			id := owners["alice"].ID
			if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
				if _, err := db.Write().Exec(`UPDATE accounts SET provider='gmail' WHERE id=?`, id); err != nil {
					return err
				}
				if !legacy {
					_, err := db.Write().Exec(`DELETE FROM account_contact_sync_configs WHERE account_id=?`, id)
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			snapshot := serviceSnapshotFor(t, accounts, "alice", id)
			next, err := accounts.StartContactPull(t.Context(), snapshot)
			if err != nil || next == nil || next.contacts == snapshot.contacts || next.ContactConfig().Provider != "gmail" {
				t.Fatal("start did not capture normalized config", next, err)
			}
			if _, err := accounts.PublishInboundContacts(t.Context(), snapshot, nil); !errors.Is(err, ErrAccountServicesChanged) {
				t.Fatal("old config snapshot accepted", err)
			}
			results, err := accounts.PublishInboundContacts(t.Context(), next, []storage.InboundContact{{Contact: models.Contact{Email: "person@example.com"}, RemoteID: "people/1"}})
			if err != nil || len(results) != 1 {
				t.Fatal("started snapshot could not publish", results, err)
			}
			if err := accounts.FinishContactPull(t.Context(), next, 1, nil); err != nil {
				t.Fatal(err)
			}
			if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
				var provider string
				var enabled, count, started, success int
				err := db.Read().QueryRow(`SELECT provider,enabled,last_import_count,last_started_at IS NOT NULL,last_success_at IS NOT NULL FROM account_contact_sync_configs WHERE account_id=?`, id).Scan(&provider, &enabled, &count, &started, &success)
				if provider != "gmail" || enabled != 1 || count != 1 || started != 1 || success != 1 {
					t.Fatal("bad start/success status", provider, enabled, count, started, success)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserContactPullStatusKeepsCursorsAndRejectsLateSettings(t *testing.T) {
	_, _, accounts, owners := newServiceAccountFixture(t)
	id := owners["alice"].ID
	snapshot := serviceSnapshotFor(t, accounts, "alice", id)
	next, err := accounts.StartContactPull(t.Context(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	_, next, err = accounts.PublishInboundContactBook(t.Context(), next, "alice-book", storage.InboundContactBookPage{SyncToken: "book-cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.FinishContactPull(t.Context(), next, 3, nil); err != nil {
		t.Fatal(err)
	}
	if err := accounts.FinishContactPull(t.Context(), next, 0, errors.New("provider failed")); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		var token, message string
		var count, success int
		if err := db.Read().QueryRow(`SELECT c.last_import_count,c.last_error,c.last_success_at IS NOT NULL,b.last_sync_token FROM account_contact_sync_configs c JOIN account_contact_address_books b ON b.account_id=c.account_id WHERE c.account_id=?`, id).Scan(&count, &message, &success, &token); err != nil {
			return err
		}
		if count != 3 || message != "provider failed" || success != 1 || token != "book-cursor" {
			t.Fatal("status damaged cursor/history", count, message, success, token)
		}
		_, err := db.Write().Exec(`UPDATE account_contact_sync_configs SET username='replacement',last_error='new-status' WHERE account_id=?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []error{nil, errors.New("stale failure")} {
		if err := accounts.FinishContactPull(t.Context(), next, 9, failure); !errors.Is(err, ErrAccountServicesChanged) {
			t.Fatal("late status accepted", err)
		}
	}
	if _, err := accounts.StartContactPull(t.Context(), next); !errors.Is(err, ErrAccountServicesChanged) {
		t.Fatal("stale start accepted", err)
	}
	if after := serviceSnapshotFor(t, accounts, "alice", id); after.ContactConfig().LastError != "new-status" {
		t.Fatal("new status overwritten")
	}
}

func TestUserContactPullStatusDisabledAndRollbackRetry(t *testing.T) {
	_, _, accounts, owners := newServiceAccountFixture(t)
	id := owners["alice"].ID
	snapshot := serviceSnapshotFor(t, accounts, "alice", id)
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER fail_start BEFORE UPDATE ON account_contact_sync_configs BEGIN SELECT RAISE(ABORT,'status failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if next, err := accounts.StartContactPull(t.Context(), snapshot); err == nil || next != nil {
		t.Fatal("failed status returned snapshot", next, err)
	}
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`DROP TRIGGER fail_start`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	next, err := accounts.StartContactPull(t.Context(), snapshot)
	if err != nil {
		t.Fatal("start retry", err)
	}
	cfg := next.ContactConfig()
	cfg.Enabled = false
	if err := accounts.SaveContactServices(t.Context(), next, cfg, ""); err != nil {
		t.Fatal(err)
	}
	disabled := serviceSnapshotFor(t, accounts, "alice", id)
	if _, err := accounts.StartContactPull(t.Context(), disabled); !errors.Is(err, storage.ErrContactPublication) {
		t.Fatal("disabled sync started", err)
	}
	if err := accounts.FinishContactPull(t.Context(), next, 1, nil); !errors.Is(err, ErrAccountServicesChanged) {
		t.Fatal("disabled sync completed", err)
	}
}
