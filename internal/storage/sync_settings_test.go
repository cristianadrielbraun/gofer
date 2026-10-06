package storage

import (
	"errors"
	"sync"
	"testing"
)

func TestUserSyncSettingsAtomicOwnershipAndConcurrentMerge(t *testing.T) {
	_, _, r := newAccountRoutingTest(t, 2)
	first := createRoutingTestAccount(t, r, "alice")
	second := createRoutingTestAccount(t, r, "alice")
	foreign := createRoutingTestAccount(t, r, "bob")
	err := r.WithUser(t.Context(), "alice", func(db *DB) error {
		for _, id := range []string{first.AccountID, second.AccountID} {
			if _, err := db.Write().Exec(`INSERT INTO folders(id,account_id,name,role,selectable,remote_id) VALUES(?,?,'Inbox','inbox',1,'INBOX')`, id+"-inbox", id); err != nil {
				return err
			}
		}
		if err := db.SetSetting(t.Context(), "alice", "idle_folders", "none"); err != nil {
			return err
		}
		if err := db.SaveUserSyncSettings(t.Context(), "alice", 5, nil); err != nil {
			return err
		}
		err := db.SaveUserSyncSettings(t.Context(), "alice", 10, map[string][]string{first.AccountID: {first.AccountID + "-inbox"}, foreign.AccountID: {"none"}})
		if !errors.Is(err, ErrAccountRoute) {
			t.Fatalf("foreign account accepted: %v", err)
		}
		if db.GetSyncInterval(t.Context(), "alice") != 5 {
			t.Fatal("invalid selection partially saved interval")
		}
		if _, err := db.Write().Exec(`CREATE TRIGGER fail_interval BEFORE INSERT ON app_settings WHEN NEW.key='sync_interval_minutes' BEGIN SELECT RAISE(ABORT,'synthetic interval failure'); END`); err != nil {
			return err
		}
		if err := db.SaveUserSyncSettings(t.Context(), "alice", 10, map[string][]string{first.AccountID: {first.AccountID + "-inbox"}}); err == nil {
			t.Fatal("injected failure was ignored")
		}
		if val, err := db.GetSetting(t.Context(), "alice", "idle_folders"); err != nil || val != "none" {
			t.Fatalf("folder selections escaped rollback: %q %v", val, err)
		}
		if _, err := db.Write().Exec("DROP TRIGGER fail_interval"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{first.AccountID, second.AccountID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- r.WithUser(t.Context(), "alice", func(db *DB) error {
				return db.SaveUserSyncSettings(t.Context(), "alice", 15, map[string][]string{id: {id + "-inbox"}})
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
		for _, id := range []string{first.AccountID, second.AccountID} {
			if _, err := db.GetFoldersForAccount(t.Context(), id); err != nil {
				return err
			}
			if !db.GetIdleFolderIDsForAccount(t.Context(), "alice", id)[id+"-inbox"] {
				t.Fatal("concurrent settings update lost another account")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
