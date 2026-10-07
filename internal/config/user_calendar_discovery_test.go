package config

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func calendarDiscoveryFor(t *testing.T, accounts *UserAccountStore, owner, id string) *UserCalendarDiscoverySnapshot {
	t.Helper()
	snapshot, err := accounts.SnapshotCalendarDiscovery(t.Context(), owner, id)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func discoveredCalendarInputs() []storage.CalendarSource {
	return []storage.CalendarSource{
		{ID: "second-source", UserID: "bob", AccountID: "foreign", Provider: "outlook", RemoteID: "/primary/", Name: "renamed", IsPrimary: true, IsSelected: false},
		{ID: "same-source", UserID: "bob", RemoteID: "/new/", Name: "new calendar", IsSelected: true},
	}
}

func TestUserCalendarDiscoveryCopiesAndAtomicallyPreservesOwnedIdentity(t *testing.T) {
	system, routing, accounts, owners := newCalendarControlFixture(t)
	alice, bob := owners["alice"].ID, owners["bob"].ID
	snapshot := calendarDiscoveryFor(t, accounts, "alice", alice)
	copy := snapshot.Sources()
	copy[0].ID, copy[0].RemoteID, copy[0].Name = "forged", "forged", "forged"
	if snapshot.Sources()[0].Name == "forged" || snapshot.Service().OwnerID() != "alice" || snapshot.Provider() != "caldav" {
		t.Fatal("mutable or unowned discovery")
	}
	if err := routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE calendar_sources SET is_hidden=1 WHERE id='same-source';
 UPDATE calendar_sync_state SET state='syncing',attempt_count=2,last_error='preserve',last_success_at='2026-10-01 00:00:00' WHERE source_id='same-source'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.ValidateCalendarDiscovery(t.Context(), snapshot); err != nil {
		t.Fatal("display/progress invalidated discovery", err)
	}
	dav := &DiscoveredCalDAVSettings{BaseURL: "https://new.test/dav", Username: "new-user", Password: "new-secret"}
	next, err := accounts.PublishCalendarDiscovery(t.Context(), snapshot, discoveredCalendarInputs(), dav)
	if err != nil || next == nil {
		t.Fatal("atomic publication", err)
	}
	baseURL, username, useAccount := next.Service().CalDAVSettings()
	password, err := next.Service().CalDAVPassword("", false)
	if baseURL != dav.BaseURL || username != dav.Username || useAccount || err != nil || password != dav.Password {
		t.Fatal("new service snapshot not captured with catalog", baseURL, username, useAccount, err)
	}
	if err := accounts.ValidateCalendarDiscovery(t.Context(), next); err != nil {
		t.Fatal("new discovery snapshot invalid", err)
	}
	if err := accounts.ValidateCalendarDiscovery(t.Context(), snapshot); !errors.Is(err, ErrAccountServicesChanged) {
		t.Fatal("old service state accepted", err)
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			var name string
			var selected, hidden, stateCount int
			if err := db.Read().QueryRow(`SELECT name,is_selected,is_hidden FROM calendar_sources WHERE id='same-source'`).Scan(&name, &selected, &hidden); err != nil {
				return err
			}
			if owner == "alice" {
				if name != "renamed" || selected != 1 || hidden != 1 {
					t.Fatal("discovery replaced identity/selection/visibility", name, selected, hidden)
				}
				var deleted, secondSelected, attempt int
				var state, lastError string
				if err := db.Read().QueryRow(`SELECT is_deleted,is_selected FROM calendar_sources WHERE id='second-source'`).Scan(&deleted, &secondSelected); err != nil {
					return err
				}
				if err := db.Read().QueryRow(`SELECT state,attempt_count,last_error FROM calendar_sync_state WHERE source_id='same-source'`).Scan(&state, &attempt, &lastError); err != nil {
					return err
				}
				if deleted != 1 || secondSelected != 0 || state != "syncing" || attempt != 2 || lastError != "preserve" {
					t.Fatal("discovery lost tombstones or worker state", deleted, secondSelected, state, attempt, lastError)
				}
				var newID, newOwner, newAccount, newProvider string
				if err := db.Read().QueryRow(`SELECT id,user_id,account_id,provider FROM calendar_sources WHERE remote_id='/new/'`).Scan(&newID, &newOwner, &newAccount, &newProvider); err != nil {
					return err
				}
				if newID == "same-source" || newID == "second-source" || newOwner != owner || newAccount != alice || newProvider != "caldav" {
					t.Fatal("caller chose published local identity", newID, newOwner, newAccount, newProvider)
				}
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_sync_state`).Scan(&stateCount); err != nil || stateCount != 3 {
					t.Fatal("new source missing state", stateCount, err)
				}
			} else if name != "bob primary" || selected != 1 || hidden != 0 {
				t.Fatal("publication crossed owner", name, selected, hidden)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := accounts.SnapshotCalendarDiscovery(t.Context(), "alice", bob); !errors.Is(err, storage.ErrAccountRoute) {
		t.Fatal("foreign account snapshot", err)
	}
	other, err := NewUserAccountStore(routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []*UserCalendarDiscoverySnapshot{nil, {}, next} {
		if err := other.ValidateCalendarDiscovery(t.Context(), value); err == nil {
			t.Fatal("nil/zero/foreign repository accepted")
		}
		if result, err := other.PublishCalendarDiscovery(t.Context(), value, nil, dav); err == nil || result != nil {
			t.Fatal("invalid snapshot published", err)
		}
	}
	var centralSources int
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM calendar_sources`).Scan(&centralSources); err != nil || centralSources != 0 {
		t.Fatal("discovery central fallback", centralSources, err)
	}
}

func TestUserCalendarDiscoveryAtomicFaultsAndRetry(t *testing.T) {
	for _, fault := range []string{"settings", "settings-ignore", "source", "source-ignore", "sync-state", "sync-state-ignore", "missing", "missing-ignore"} {
		t.Run(fault, func(t *testing.T) {
			_, _, accounts, owners := newCalendarControlFixture(t)
			id := owners["alice"].ID
			snapshot := calendarDiscoveryFor(t, accounts, "alice", id)
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				raise := "SELECT RAISE(ABORT,'synthetic discovery fault');"
				if fault == "settings-ignore" || fault == "source-ignore" || fault == "sync-state-ignore" || fault == "missing-ignore" {
					raise = "SELECT RAISE(IGNORE);"
				}
				var query string
				switch fault {
				case "settings", "settings-ignore":
					query = "CREATE TRIGGER fail_discovery BEFORE INSERT ON account_caldav_configs BEGIN " + raise + " END"
				case "source", "source-ignore":
					query = "CREATE TRIGGER fail_discovery BEFORE INSERT ON calendar_sources WHEN NEW.remote_id='/new/' BEGIN " + raise + " END"
				case "sync-state", "sync-state-ignore":
					query = "CREATE TRIGGER fail_discovery BEFORE INSERT ON calendar_sync_state WHEN EXISTS(SELECT 1 FROM calendar_sources WHERE id=NEW.source_id AND remote_id='/new/') BEGIN " + raise + " END"
				case "missing", "missing-ignore":
					query = "CREATE TRIGGER fail_discovery BEFORE UPDATE ON calendar_sources WHEN NEW.id='second-source' AND NEW.is_deleted=1 BEGIN " + raise + " END"
				}
				_, err := db.Write().Exec(query)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			dav := &DiscoveredCalDAVSettings{BaseURL: "https://new.test/dav", Username: "new-user", Password: "new-secret"}
			if result, err := accounts.PublishCalendarDiscovery(t.Context(), snapshot, discoveredCalendarInputs(), dav); err == nil || result != nil {
				t.Fatal("fault returned publication", err)
			}
			current := calendarDiscoveryFor(t, accounts, "alice", id)
			if !reflect.DeepEqual(current.Sources(), snapshot.Sources()) || current.service.calendar != snapshot.service.calendar {
				t.Fatal("partial configuration/catalog escaped rollback")
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				_, err := db.Write().Exec(`DROP TRIGGER fail_discovery`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if result, err := accounts.PublishCalendarDiscovery(t.Context(), snapshot, discoveredCalendarInputs(), dav); err != nil || result == nil {
				t.Fatal("atomic publication retry", err)
			}
		})
	}
}

func TestUserCalendarDiscoveryWriterWaitRejectsChangedAuthority(t *testing.T) {
	for _, change := range []string{"owner", "account", "connection", "caldav", "selection", "source"} {
		t.Run(change, func(t *testing.T) {
			system, _, accounts, owners := newCalendarControlFixture(t)
			id := owners["alice"].ID
			snapshot := calendarDiscoveryFor(t, accounts, "alice", id)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
				tx, err := db.Write().BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				before := db.Write().Stats().WaitCount
				done := make(chan error, 1)
				go func() {
					result, err := accounts.PublishCalendarDiscovery(ctx, snapshot, discoveredCalendarInputs(), &DiscoveredCalDAVSettings{BaseURL: "https://new.test", Password: "new-secret"})
					if result != nil {
						t.Error("changed authority produced snapshot")
					}
					done <- err
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
				case "owner":
					_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
				case "account":
					_, err = system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, id)
				case "connection":
					_, err = tx.Exec(`UPDATE accounts SET username='changed' WHERE id=?`, id)
				case "caldav":
					_, err = tx.Exec(`UPDATE account_caldav_configs SET base_url='https://edited.test' WHERE account_id=?`, id)
				case "selection":
					_, err = tx.Exec(`UPDATE calendar_sources SET is_selected=0 WHERE id='same-source'`)
				case "source":
					_, err = tx.Exec(`UPDATE calendar_sources SET remote_id='/edited/' WHERE id='same-source'`)
				}
				if err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("late discovery ignored authority change")
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				var newSources int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_sources WHERE remote_id='/new/'`).Scan(&newSources); err != nil || newSources != 0 {
					t.Fatal("late provider source published", newSources, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
