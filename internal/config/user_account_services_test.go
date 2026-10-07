package config

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newServiceAccountFixture(t *testing.T) (*storage.DB, *storage.AccountRouting, *UserAccountStore, map[string]*models.Account) {
	t.Helper()
	system, routing, accounts := newUserAccountTestStore(t)
	owners := make(map[string]*models.Account)
	for _, owner := range []string{"alice", "bob"} {
		req := secureAccountStoreTestRequest(owner + "@example.com")
		req.Password = owner + "-mail-password"
		var err error
		owners[owner], err = accounts.CreateAccount(t.Context(), owner, req)
		if err != nil {
			t.Fatal(err)
		}
		err = accounts.WithAccountForUser(t.Context(), owner, owners[owner].ID, func(local *AccountStore, _ *storage.DB) error {
			cfg := models.ContactSyncConfig{Provider: "carddav", Enabled: true, BaseURL: "https://" + owner + ".test/dav", Username: owner,
				AddressBooks: []models.ContactAddressBook{{ID: owner + "-book", Name: owner + " book", URL: "https://" + owner + ".test/book", Default: true}}}
			if err := local.SaveContactSyncConfig(t.Context(), owner, owners[owner].ID, cfg, owner+"-contact-password"); err != nil {
				return err
			}
			return local.SaveCalDAVConfig(t.Context(), owner, owners[owner].ID, "https://"+owner+".test/cal", owner, owner+"-calendar-password", false)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return system, routing, accounts, owners
}

func serviceSnapshotFor(t *testing.T, accounts *UserAccountStore, owner, id string) *AccountServiceSnapshot {
	t.Helper()
	snapshot, err := accounts.SnapshotServices(t.Context(), owner, id)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestUserAccountServicesCopiesOwnerSettingsAndSecrets(t *testing.T) {
	system, _, accounts, owners := newServiceAccountFixture(t)
	for _, owner := range []string{"alice", "bob"} {
		snapshot := serviceSnapshotFor(t, accounts, owner, owners[owner].ID)
		if snapshot.OwnerID() != owner || snapshot.AccountID() != owners[owner].ID || snapshot.Identity().EmailAddress != owner+"@example.com" {
			t.Fatal("wrong owner identity")
		}
		cfg := snapshot.ContactConfig()
		if !cfg.Enabled || !cfg.HasPassword || cfg.Provider != "carddav" || len(cfg.AddressBooks) != 1 || cfg.AddressBooks[0].ID != owner+"-book" {
			t.Fatalf("bad config %+v", cfg)
		}
		cfg.AddressBooks[0].Name = "caller mutation"
		if snapshot.ContactConfig().AddressBooks[0].Name != owner+" book" {
			t.Fatal("snapshot slice escaped")
		}
		for _, check := range []struct {
			getter func() (string, error)
			want   string
		}{
			{snapshot.MailboxPassword, owner + "-mail-password"},
			{func() (string, error) { return snapshot.ContactSyncPassword("") }, owner + "-contact-password"},
			{func() (string, error) { return snapshot.ContactSyncPassword("explicit") }, "explicit"},
			{func() (string, error) { return snapshot.CalDAVPassword("", false) }, owner + "-calendar-password"},
			{func() (string, error) { return snapshot.CalDAVPassword("ignored", true) }, owner + "-mail-password"},
		} {
			got, err := check.getter()
			if err != nil || got != check.want {
				t.Fatal("password lookup failed", err)
			}
		}
	}
	if _, err := accounts.SnapshotServices(t.Context(), "bob", owners["alice"].ID); !errors.Is(err, storage.ErrAccountRoute) {
		t.Fatal("foreign snapshot accepted", err)
	}
	for _, table := range []string{"accounts", "account_contact_sync_configs", "account_contact_address_books", "account_caldav_configs"} {
		var count int
		if err := system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("central %s copies=%d err=%v", table, count, err)
		}
	}
}

func TestUserAccountServicesContactTogglePreservesConfigurationAndDefaults(t *testing.T) {
	system, _, accounts, owners := newServiceAccountFixture(t)
	id := owners["alice"].ID
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE account_contact_address_books SET last_sync_token='saved-book-cursor' WHERE account_id=?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := serviceSnapshotFor(t, accounts, "alice", id)
	for _, enabled := range []bool{false, true} {
		current := serviceSnapshotFor(t, accounts, "alice", id)
		if err := accounts.SetContactServicesEnabled(t.Context(), current, enabled); err != nil {
			t.Fatal(err)
		}
		after := serviceSnapshotFor(t, accounts, "alice", id)
		want, got := before.ContactConfig(), after.ContactConfig()
		want.Enabled, want.UpdatedAt = enabled, got.UpdatedAt
		if !reflect.DeepEqual(want, got) || !reflect.DeepEqual(before.contactPassword, after.contactPassword) || before.calendar != after.calendar {
			t.Fatal("toggle replaced settings, credentials, cursors or calendar state")
		}
	}
	if cfg := serviceSnapshotFor(t, accounts, "bob", owners["bob"].ID).ContactConfig(); !cfg.Enabled || cfg.AddressBooks[0].ID != "bob-book" {
		t.Fatal("toggle changed another owner")
	}
	for _, provider := range []string{"imap", "gmail", "outlook"} {
		if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
			if _, err := db.Write().Exec(`DELETE FROM account_contact_address_books WHERE account_id=?`, id); err != nil {
				return err
			}
			if _, err := db.Write().Exec(`DELETE FROM account_contact_sync_configs WHERE account_id=?`, id); err != nil {
				return err
			}
			_, err := db.Write().Exec(`UPDATE accounts SET provider=? WHERE id=?`, provider, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, enabled := range []bool{false, true} {
			snapshot := serviceSnapshotFor(t, accounts, "alice", id)
			err := accounts.SetContactServicesEnabled(t.Context(), snapshot, enabled)
			if provider == "imap" {
				if !errors.Is(err, ErrContactServicesNotConfigured) {
					t.Fatal("unconfigured DAV toggle accepted", err)
				}
			} else if err != nil || serviceSnapshotFor(t, accounts, "alice", id).ContactConfig().Enabled != enabled {
				t.Fatal("builtin default toggle failed", provider, err)
			}
		}
	}
	var count int
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM account_contact_sync_configs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("central contact fallback", count, err)
	}
}

func TestUserAccountServicesSaveRetainsBookIDsAndPasswords(t *testing.T) {
	_, _, accounts, owners := newServiceAccountFixture(t)
	snapshot := serviceSnapshotFor(t, accounts, "alice", owners["alice"].ID)
	cfg := snapshot.ContactConfig()
	cfg.AddressBooks[0].ID = ""
	cfg.AddressBooks[0].Name = "renamed"
	cfg.UserID = "bob"
	cfg.AccountID = owners["bob"].ID
	if err := accounts.SaveContactServices(t.Context(), snapshot, cfg, ""); err != nil {
		t.Fatal(err)
	}
	after := serviceSnapshotFor(t, accounts, "alice", owners["alice"].ID)
	if got := after.ContactConfig(); got.UserID != "alice" || got.AccountID != owners["alice"].ID || got.AddressBooks[0].ID != "alice-book" || got.AddressBooks[0].Name != "renamed" {
		t.Fatalf("bad saved config %+v", got)
	}
	if password, err := after.ContactSyncPassword(""); err != nil || password != "alice-contact-password" {
		t.Fatal("contact password lost", err)
	}
	if err := accounts.SaveCalDAVServices(t.Context(), after, "https://new.test/cal", "new-user", "", false); err != nil {
		t.Fatal(err)
	}
	after = serviceSnapshotFor(t, accounts, "alice", owners["alice"].ID)
	if password, err := after.CalDAVPassword("", false); err != nil || password != "alice-calendar-password" {
		t.Fatal("calendar password lost", err)
	}
	if err := accounts.SaveCalDAVServices(t.Context(), after, "https://new.test/cal", "ignored", "ignored", true); err != nil {
		t.Fatal(err)
	}
	after = serviceSnapshotFor(t, accounts, "alice", owners["alice"].ID)
	_, username, useAccount := after.CalDAVSettings()
	if username != "" || !useAccount || len(after.calendarPassword) != 0 {
		t.Fatal("dedicated credentials remained after choosing mailbox credentials")
	}
}

func TestUserAccountServicesRejectStaleMailboxIdentity(t *testing.T) {
	cases := []struct{ name, sql string }{
		{"provider", "UPDATE accounts SET provider='gmail' WHERE id=?"},
		{"subject", "UPDATE accounts SET provider_account_id='replacement' WHERE id=?"},
		{"email", "UPDATE accounts SET email_address='replacement@example.com' WHERE id=?"},
		{"username", "UPDATE accounts SET username='replacement' WHERE id=?"},
		{"auth", "UPDATE accounts SET auth_method='oauth2' WHERE id=?"},
		{"imap", "UPDATE accounts SET imap_host='replacement.test' WHERE id=?"},
		{"smtp", "UPDATE accounts SET smtp_host='replacement.test' WHERE id=?"},
		{"password", "UPDATE accounts SET encrypted_password=x'123456' WHERE id=?"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, accounts, owners := newServiceAccountFixture(t)
			id := owners["alice"].ID
			snapshot := serviceSnapshotFor(t, accounts, "alice", id)
			if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error { _, err := db.Write().Exec(tc.sql, id); return err }); err != nil {
				t.Fatal(err)
			}
			cfg := snapshot.ContactConfig()
			cfg.BaseURL = "https://stale.test/dav"
			if err := accounts.SaveContactServices(t.Context(), snapshot, cfg, "stale"); !errors.Is(err, ErrAccountServicesChanged) {
				t.Fatal("stale contact result published", err)
			}
			if err := accounts.SetContactServicesEnabled(t.Context(), snapshot, false); !errors.Is(err, ErrAccountServicesChanged) {
				t.Fatal("stale contact toggle published", err)
			}
			if err := accounts.SaveCalDAVServices(t.Context(), snapshot, "https://stale.test/cal", "stale", "stale", false); !errors.Is(err, ErrAccountServicesChanged) {
				t.Fatal("stale calendar result published", err)
			}
			current := serviceSnapshotFor(t, accounts, "alice", id)
			if current.contacts != snapshot.contacts || current.calendar != snapshot.calendar {
				t.Fatal("stale publication changed service state")
			}
		})
	}
}

func TestUserAccountServicesRejectStaleConfigurationAndCursors(t *testing.T) {
	cases := []struct {
		name, sql string
		contacts  bool
	}{
		{"contact-endpoint", "UPDATE account_contact_sync_configs SET base_url='https://changed.test' WHERE account_id=?", true},
		{"contact-disabled", "UPDATE account_contact_sync_configs SET enabled=0 WHERE account_id=?", true},
		{"contact-cursor", "UPDATE account_contact_sync_configs SET last_sync_token='new-cursor' WHERE account_id=?", true},
		{"contact-password", "UPDATE account_contact_sync_configs SET encrypted_password=x'1234' WHERE account_id=?", true},
		{"book-cursor", "UPDATE account_contact_address_books SET last_sync_token='new-book-cursor' WHERE account_id=?", true},
		{"book-name", "UPDATE account_contact_address_books SET name='changed' WHERE account_id=?", true},
		{"book-removed", "DELETE FROM account_contact_address_books WHERE account_id=?", true},
		{"caldav-endpoint", "UPDATE account_caldav_configs SET base_url='https://changed.test' WHERE account_id=?", false},
		{"caldav-credentials", "UPDATE account_caldav_configs SET use_account_credentials=1 WHERE account_id=?", false},
		{"caldav-password", "UPDATE account_caldav_configs SET encrypted_password=x'1234' WHERE account_id=?", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, accounts, owners := newServiceAccountFixture(t)
			id := owners["alice"].ID
			snapshot := serviceSnapshotFor(t, accounts, "alice", id)
			if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error { _, err := db.Write().Exec(tc.sql, id); return err }); err != nil {
				t.Fatal(err)
			}
			var err error
			if tc.contacts {
				cfg := snapshot.ContactConfig()
				cfg.BaseURL = "https://stale.test"
				err = accounts.SaveContactServices(t.Context(), snapshot, cfg, "stale")
			} else {
				err = accounts.SaveCalDAVServices(t.Context(), snapshot, "https://stale.test", "stale", "stale", false)
			}
			if !errors.Is(err, ErrAccountServicesChanged) {
				t.Fatal("stale result accepted", err)
			}
			if tc.contacts {
				if err := accounts.SetContactServicesEnabled(t.Context(), snapshot, false); !errors.Is(err, ErrAccountServicesChanged) {
					t.Fatal("stale contact toggle accepted", err)
				}
			}
		})
	}
}

func TestUserAccountServicesSnapshotDoesNotHoldCacheDuringProviderWork(t *testing.T) {
	_, _, accounts, owners := newServiceAccountFixture(t)
	snapshot := serviceSnapshotFor(t, accounts, "alice", owners["alice"].ID)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		<-release
		done <- accounts.SaveContactServices(t.Context(), snapshot, snapshot.ContactConfig(), "")
	}()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := accounts.SnapshotServices(ctx, "bob", owners["bob"].ID); err != nil {
		close(release)
		<-done
		t.Fatal("copied snapshot pinned user cache", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUserAccountServicesIndependentServiceAndStatusUpdates(t *testing.T) {
	_, _, accounts, owners := newServiceAccountFixture(t)
	id := owners["alice"].ID
	snapshot := serviceSnapshotFor(t, accounts, "alice", id)
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(local *AccountStore, _ *storage.DB) error {
		if err := local.MarkContactSyncError(t.Context(), "alice", id, "transient"); err != nil {
			return err
		}
		return local.SaveCalDAVConfig(t.Context(), "alice", id, "https://other.test/cal", "other", "other", false)
	}); err != nil {
		t.Fatal(err)
	}
	cfg := snapshot.ContactConfig()
	cfg.BaseURL = "https://new.test/dav"
	if err := accounts.SaveContactServices(t.Context(), snapshot, cfg, ""); err != nil {
		t.Fatal("unrelated calendar/status change rejected contacts", err)
	}
	fresh := serviceSnapshotFor(t, accounts, "alice", id)
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(local *AccountStore, _ *storage.DB) error {
		return local.SaveContactSyncConfig(t.Context(), "alice", id, cfg, "contact-replaced")
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveCalDAVServices(t.Context(), fresh, "https://final.test/cal", "final", "", false); err != nil {
		t.Fatal("unrelated contact change rejected calendar", err)
	}
}

func TestUserAccountServicesConcurrentSaveHasSingleWinner(t *testing.T) {
	_, _, accounts, owners := newServiceAccountFixture(t)
	snapshot := serviceSnapshotFor(t, accounts, "alice", owners["alice"].ID)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, name := range []string{"first", "second"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			<-start
			cfg := snapshot.ContactConfig()
			cfg.Username = name
			results <- accounts.SaveContactServices(t.Context(), snapshot, cfg, name)
		}(name)
	}
	close(start)
	wg.Wait()
	close(results)
	successes, stale := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrAccountServicesChanged) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("successes=%d stale=%d", successes, stale)
	}
}

func TestUserAccountServicesRollbackRetainsConfigurationAndAllowsRetry(t *testing.T) {
	_, _, accounts, owners := newServiceAccountFixture(t)
	id := owners["alice"].ID
	snapshot := serviceSnapshotFor(t, accounts, "alice", id)
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER reject_service_book BEFORE INSERT ON account_contact_address_books BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cfg := snapshot.ContactConfig()
	cfg.BaseURL = "https://new.test/dav"
	cfg.AddressBooks[0].Name = "new name"
	if err := accounts.SaveContactServices(t.Context(), snapshot, cfg, "new-password"); err == nil {
		t.Fatal("failed book insert accepted")
	}
	after := serviceSnapshotFor(t, accounts, "alice", id)
	if after.contacts != snapshot.contacts || !reflect.DeepEqual(after.ContactConfig(), snapshot.ContactConfig()) {
		t.Fatal("partial config escaped rollback")
	}
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`DROP TRIGGER reject_service_book`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveContactServices(t.Context(), snapshot, cfg, "new-password"); err != nil {
		t.Fatal("retry failed", err)
	}
}

func TestUserAccountServicesRejectUnavailableOwnersAndUnboundSnapshots(t *testing.T) {
	for _, state := range []string{"disabled", "deletion_pending", "account_deleting", "account_deleted"} {
		t.Run(state, func(t *testing.T) {
			system, routing, accounts, owners := newServiceAccountFixture(t)
			id := owners["alice"].ID
			snapshot := serviceSnapshotFor(t, accounts, "alice", id)
			var err error
			switch state {
			case "disabled":
				_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
			case "deletion_pending":
				_, err = system.Write().Exec(`UPDATE users SET status='disabled',deletion_pending=1 WHERE id='alice'`)
			case "account_deleting":
				err = routing.BeginAccountDeletion(t.Context(), "alice", id)
			case "account_deleted":
				err = accounts.DeleteAccount(t.Context(), "alice", id, func(context.Context, string) error { return nil })
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := accounts.SaveContactServices(t.Context(), snapshot, snapshot.ContactConfig(), ""); err == nil {
				t.Fatal("unavailable owner contact save accepted")
			}
			if err := accounts.SaveCalDAVServices(t.Context(), snapshot, "https://stale.test", "stale", "", false); err == nil {
				t.Fatal("unavailable owner calendar save accepted")
			}
			if _, err := accounts.SnapshotServices(t.Context(), "bob", owners["bob"].ID); err != nil {
				t.Fatal("other owner affected", err)
			}
		})
	}
	_, routing, accounts, owners := newServiceAccountFixture(t)
	snapshot := serviceSnapshotFor(t, accounts, "alice", owners["alice"].ID)
	other, err := NewUserAccountStore(routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []*AccountServiceSnapshot{nil, {}, snapshot} {
		if err := other.SaveContactServices(t.Context(), candidate, models.ContactSyncConfig{}, ""); !errors.Is(err, storage.ErrAccountRoute) {
			t.Fatal("unbound snapshot accepted", err)
		}
	}
}

func TestUserAccountServicesBuiltinDefaultsAndCalDAVProviderGuard(t *testing.T) {
	_, _, accounts, owners := newServiceAccountFixture(t)
	id := owners["alice"].ID
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET provider='outlook',auth_method='oauth2',provider_account_id='graph-subject' WHERE id=?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := serviceSnapshotFor(t, accounts, "alice", id)
	if cfg := snapshot.ContactConfig(); cfg.Provider != "outlook" || !cfg.Enabled {
		t.Fatal("builtin contact default lost")
	}
	if err := accounts.SaveCalDAVServices(t.Context(), snapshot, "https://invalid.test", "wrong", "wrong", false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("CalDAV config accepted for Graph provider", err)
	}
}

func TestUserAccountServicesPasswordFallbackAndReadOnlySnapshots(t *testing.T) {
	_, _, accounts, owners := newServiceAccountFixture(t)
	id := owners["alice"].ID
	for _, encrypted := range []string{"NULL", "x'1234'"} {
		if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
			_, err := db.Write().Exec("UPDATE account_contact_sync_configs SET encrypted_password="+encrypted+" WHERE account_id=?", id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		snapshot := serviceSnapshotFor(t, accounts, "alice", id)
		if password, err := snapshot.ContactSyncPassword(""); err != nil || password != "alice-mail-password" {
			t.Fatal("legacy mailbox password fallback lost", err)
		}
	}
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE account_caldav_configs SET encrypted_password=x'1234' WHERE account_id=?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := serviceSnapshotFor(t, accounts, "alice", id)
	if password, err := snapshot.CalDAVPassword("", false); err == nil || password != "" {
		t.Fatal("corrupt separate CalDAV credential accepted")
	}
	if password, err := snapshot.CalDAVPassword("replacement", false); err != nil || password != "replacement" {
		t.Fatal("submitted CalDAV replacement rejected", err)
	}
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER reject_service_snapshot_write BEFORE UPDATE ON account_contact_sync_configs BEGIN SELECT RAISE(ABORT,'snapshot must be read-only'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.SnapshotServices(t.Context(), "alice", id); err != nil {
		t.Fatal("snapshot attempted write", err)
	}
}

func TestUserAccountServicesCalDAVRollbackAllowsSameSnapshotRetry(t *testing.T) {
	_, _, accounts, owners := newServiceAccountFixture(t)
	id := owners["alice"].ID
	snapshot := serviceSnapshotFor(t, accounts, "alice", id)
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER reject_service_calendar BEFORE UPDATE ON account_caldav_configs BEGIN SELECT RAISE(ABORT,'synthetic calendar failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveCalDAVServices(t.Context(), snapshot, "https://new.test/calendar", "new", "new-secret", false); err == nil {
		t.Fatal("calendar SQL failure accepted")
	}
	if after := serviceSnapshotFor(t, accounts, "alice", id); after.calendar != snapshot.calendar {
		t.Fatal("partial calendar configuration escaped")
	}
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`DROP TRIGGER reject_service_calendar`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveCalDAVServices(t.Context(), snapshot, "https://new.test/calendar", "new", "new-secret", false); err != nil {
		t.Fatal(err)
	}
	if password, err := serviceSnapshotFor(t, accounts, "alice", id).CalDAVPassword("", false); err != nil || password != "new-secret" {
		t.Fatal("successful retry lost password", err)
	}
}

func TestUserAccountServicesBuiltinStoredDisableIsPreserved(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, stored := range []string{"absent", "disabled", "enabled"} {
			t.Run(provider+"/"+stored, func(t *testing.T) {
				_, _, accounts, owners := newServiceAccountFixture(t)
				id := owners["alice"].ID
				if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
					if _, err := db.Write().Exec(`UPDATE accounts SET provider=?,auth_method='oauth2',provider_account_id='provider-subject' WHERE id=?`, provider, id); err != nil {
						return err
					}
					query := `DELETE FROM account_contact_sync_configs WHERE account_id=?`
					if stored == "disabled" {
						query = `UPDATE account_contact_sync_configs SET enabled=0 WHERE account_id=?`
					}
					if stored == "enabled" {
						query = `UPDATE account_contact_sync_configs SET enabled=1 WHERE account_id=?`
					}
					_, err := db.Write().Exec(query, id)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				snapshot := serviceSnapshotFor(t, accounts, "alice", id)
				cfg := snapshot.ContactConfig()
				if cfg.Provider != provider || cfg.Enabled != (stored != "disabled") {
					t.Fatalf("provider=%s stored=%s enabled=%v", cfg.Provider, stored, cfg.Enabled)
				}
			})
		}
	}
}

func TestUserAccountServicesRecheckLifecycleAfterWriterWait(t *testing.T) {
	for _, kind := range []string{"contacts", "calendar", "contact-toggle"} {
		for _, change := range []string{"disabled", "account_deleting"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				system, routing, accounts, owners := newServiceAccountFixture(t)
				id := owners["alice"].ID
				snapshot := serviceSnapshotFor(t, accounts, "alice", id)
				done := make(chan error, 1)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				err := accounts.WithAccountForUser(ctx, "alice", id, func(_ *AccountStore, db *storage.DB) error {
					tx, err := db.Write().BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					defer tx.Rollback()
					before := db.Write().Stats().WaitCount
					go func() {
						if kind == "contacts" {
							cfg := snapshot.ContactConfig()
							cfg.Username = "late"
							done <- accounts.SaveContactServices(ctx, snapshot, cfg, "late")
						} else if kind == "contact-toggle" {
							done <- accounts.SetContactServicesEnabled(ctx, snapshot, false)
						} else {
							done <- accounts.SaveCalDAVServices(ctx, snapshot, "https://late.test", "late", "late", false)
						}
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
					// The facade passed its first central authorization check and is now
					// waiting for the one local writer connection held by this transaction.
					if change == "disabled" {
						_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
					} else {
						err = routing.RequestAccountDeletion(ctx, "alice", id)
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
				case err := <-done:
					if err == nil {
						t.Fatal("late lifecycle publication accepted")
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if change == "disabled" {
					if _, err := system.Write().Exec(`UPDATE users SET status='active' WHERE id='alice'`); err != nil {
						t.Fatal(err)
					}
				}
				if change == "account_deleting" {
					// Deletion intent deliberately remains durable. Inspect the same local
					// file through an owner lease, without routing its unavailable account.
					err = accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
						var contactUsername, calendarURL string
						if err := db.Read().QueryRow(`SELECT username FROM account_contact_sync_configs WHERE account_id=?`, id).Scan(&contactUsername); err != nil {
							return err
						}
						if err := db.Read().QueryRow(`SELECT base_url FROM account_caldav_configs WHERE account_id=?`, id).Scan(&calendarURL); err != nil {
							return err
						}
						if contactUsername != "alice" || calendarURL != "https://alice.test/cal" {
							t.Fatal("late settings escaped")
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
				} else if after := serviceSnapshotFor(t, accounts, "alice", id); after.contacts != snapshot.contacts || after.calendar != snapshot.calendar {
					t.Fatal("disabled owner settings changed")
				}
			})
		}
	}
}

func TestUserAccountServicesValidateCopiedSetupIsReadOnlyAndBound(t *testing.T) {
	_, _, accounts, owners := newServiceAccountFixture(t)
	id := owners["alice"].ID
	snapshot := serviceSnapshotFor(t, accounts, "alice", id)
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER reject_service_validate_write BEFORE UPDATE ON account_contact_sync_configs BEGIN SELECT RAISE(ABORT,'validation must be read-only'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.ValidateContactServices(t.Context(), snapshot); err != nil {
		t.Fatal("unchanged read validation failed", err)
	}
	if err := accounts.ValidateContactServices(t.Context(), nil); !errors.Is(err, storage.ErrAccountRoute) {
		t.Fatal("unbound snapshot validated", err)
	}
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		if _, err := db.Write().Exec(`DROP TRIGGER reject_service_validate_write`); err != nil {
			return err
		}
		_, err := db.Write().Exec(`UPDATE account_contact_sync_configs SET username='changed' WHERE account_id=?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.ValidateContactServices(t.Context(), snapshot); !errors.Is(err, ErrAccountServicesChanged) {
		t.Fatal("stale setup validated", err)
	}
}
