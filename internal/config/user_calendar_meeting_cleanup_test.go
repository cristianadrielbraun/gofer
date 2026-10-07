package config

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func ownedCleanupFixture(t *testing.T, provider string, final, completed bool) (*storage.DB, *UserAccountStore, map[string]*UserCalendarMeetingCleanupSnapshot) {
	t.Helper()
	system, accounts, sources := ownedMeetingFixture(t, provider)
	guard := func() error { return nil }
	claims := map[string]*UserCalendarMeetingCleanupSnapshot{}
	for _, owner := range []string{"alice", "bob"} {
		d := readyMeetingDraft(t, accounts, sources[owner])
		if final {
			create, err := accounts.BeginCalendarCreate(t.Context(), sources[owner], "original", "submitted", guard)
			if err != nil {
				t.Fatal(err)
			}
			d, err = accounts.BindCalendarMeetingDraft(t.Context(), d, create, nil, guard)
			if err != nil {
				t.Fatal(err)
			}
			if completed {
				_, err = accounts.PublishCalendarCreate(t.Context(), create, storage.CalendarEvent{RemoteID: d.Teams().RemoteID, ETag: "accepted", Summary: "Accepted meeting", StartAt: calendarMutationTime(10), EndAt: calendarMutationTime(11)}, false, guard)
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
			query := `UPDATE calendar_meet_drafts SET created_at=datetime('now','-2 hours')`
			if provider == "outlook" {
				query = `UPDATE calendar_teams_drafts SET updated_at=datetime('now','-2 hours')`
			}
			_, err := db.Write().Exec(query)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := accounts.SetCalendarSourcesSelected(t.Context(), owner, sources[owner].Service().AccountID(), nil); err != nil {
			t.Fatal(err)
		}
		var err error
		claims[owner], err = accounts.SnapshotCalendarMeetingCleanup(t.Context(), owner, sources[owner].Service().AccountID(), "same-source", ownedMeetingDraftID, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	return system, accounts, claims
}

func TestUserCalendarMeetingCleanupOwnsDeselectedPrincipalAndExactRows(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			_, accounts, claims := ownedCleanupFixture(t, provider, false, false)
			for _, owner := range []string{"alice", "bob", "alice"} {
				c := claims[owner]
				if c.Source().UserID != owner || c.Source().IsSelected || c.Prune() {
					t.Fatal("wrong cleanup scope", c.Source())
				}
				if err := accounts.ValidateCalendarMeetingCleanup(t.Context(), c); err != nil {
					t.Fatal("copy lost over eviction", err)
				}
				if _, err := accounts.SnapshotCalendarSource(t.Context(), owner, "same-source"); err == nil {
					t.Fatal("cleanup relaxed ordinary source authority")
				}
			}
			other, err := NewUserAccountStore(accounts.routing, []byte("0123456789abcdef0123456789abcdef"))
			if err != nil {
				t.Fatal(err)
			}
			for _, bad := range []*UserCalendarMeetingCleanupSnapshot{nil, {}, claims["alice"]} {
				if err := other.ValidateCalendarMeetingCleanup(t.Context(), bad); !errors.Is(err, storage.ErrCalendarCreateConflict) {
					t.Fatal("unbound capability accepted", err)
				}
			}
			c := claims["alice"]
			if _, err := accounts.PublishCalendarMeetingCleanup(t.Context(), c, storage.CalendarMeetingDraftCleaned); !errors.Is(err, storage.ErrCalendarCreateConflict) {
				t.Fatal("write without grant accepted", err)
			}
			if _, err := accounts.PublishCalendarMeetingCleanup(t.Context(), c, storage.CalendarMeetingDraftRemote, func() error { return nil }); !errors.Is(err, storage.ErrCalendarCreateConflict) {
				t.Fatal("cleanup authorized preview creation", err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE calendar_create_requests SET request_hash='unbound-legacy' WHERE request_id LIKE 'meeting:%'`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := accounts.SnapshotCalendarMeetingCleanup(t.Context(), "alice", c.Service().AccountID(), "same-source", ownedMeetingDraftID, false); !errors.Is(err, storage.ErrCalendarCreateConflict) {
				t.Fatal("legacy principal silently adopted", err)
			}
		})
	}
}

func TestUserCalendarMeetingCleanupTransitionsAndAtomicRetention(t *testing.T) {
	for _, mode := range []string{"gmail", "outlook-active", "outlook-pending", "outlook-completed"} {
		t.Run(mode, func(t *testing.T) {
			provider := "outlook"
			if mode == "gmail" {
				provider = "gmail"
			}
			_, accounts, claims := ownedCleanupFixture(t, provider, mode == "outlook-pending" || mode == "outlook-completed", mode == "outlook-completed")
			c := claims["alice"]
			guard := func() error { return nil }
			if provider == "outlook" {
				transition := storage.CalendarMeetingDraftAbandon
				if mode == "outlook-pending" {
					if _, err := accounts.PublishCalendarMeetingCleanup(t.Context(), c, storage.CalendarMeetingDraftSaved, guard); !errors.Is(err, storage.ErrCalendarCreateConflict) {
						t.Fatal("pending native marker accepted as completed create", err)
					}
					transition = storage.CalendarMeetingDraftExpire
				} else if mode == "outlook-completed" {
					if _, err := accounts.PublishCalendarMeetingCleanup(t.Context(), c, storage.CalendarMeetingDraftExpire, guard); !errors.Is(err, storage.ErrCalendarCreateConflict) {
						t.Fatal("accepted create authorized expiration", err)
					}
					transition = storage.CalendarMeetingDraftSaved
				}
				var err error
				c, err = accounts.PublishCalendarMeetingCleanup(t.Context(), c, transition, guard)
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode != "outlook-completed" {
				before := c.Google()
				var err error
				c, err = accounts.PublishCalendarMeetingCleanup(t.Context(), c, storage.CalendarMeetingDraftCleaned, guard)
				if err != nil {
					t.Fatal(err)
				}
				if provider == "gmail" && c.Google() != before {
					t.Fatal("cleanup lost retained conference")
				}
			}
			if err := accounts.ValidateCalendarMeetingCleanup(t.Context(), claims["alice"]); !errors.Is(err, storage.ErrCalendarCreateConflict) {
				t.Fatal("consumed snapshot remained current", err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				query := `UPDATE calendar_meet_drafts SET created_at=datetime('now','-8 days')`
				if provider == "outlook" {
					query = `UPDATE calendar_teams_drafts SET updated_at=datetime('now','-8 days')`
				}
				_, err := db.Write().Exec(query)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			prune, err := accounts.SnapshotCalendarMeetingCleanup(t.Context(), "alice", c.Service().AccountID(), "same-source", ownedMeetingDraftID, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := accounts.PruneCalendarMeetingCleanup(t.Context(), prune); err != nil {
				t.Fatal(err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var reservations int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_create_requests WHERE request_id LIKE 'meeting:%'`).Scan(&reservations); err != nil {
					return err
				}
				if reservations != 0 {
					return fmt.Errorf("terminal meeting reservation leaked: %d", reservations)
				}
				if mode == "outlook-pending" || mode == "outlook-completed" {
					if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_create_requests WHERE request_id='original'`).Scan(&reservations); err != nil {
						return err
					}
					if reservations != 1 {
						return fmt.Errorf("original create was pruned: %d", reservations)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := accounts.ValidateCalendarMeetingCleanup(t.Context(), claims["bob"]); err != nil {
				t.Fatal("retention crossed owners", err)
			}
		})
	}
}

func TestUserCalendarMeetingCleanupWriterWaitRechecksAuthority(t *testing.T) {
	for _, change := range []string{"source", "draft", "nonce", "grant", "owner", "account", "principal"} {
		t.Run(change, func(t *testing.T) {
			system, accounts, claims := ownedCleanupFixture(t, "gmail", false, false)
			c := claims["alice"]
			var grant atomic.Bool
			grant.Store(true)
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
					_, err := accounts.PublishCalendarMeetingCleanup(ctx, c, storage.CalendarMeetingDraftCleaned, func() error {
						if !grant.Load() {
							return storage.ErrCalendarCreateConflict
						}
						return nil
					})
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
				case "source":
					_, err = tx.Exec(`UPDATE calendar_sources SET remote_id='replaced' WHERE id='same-source'`)
				case "draft":
					_, err = tx.Exec(`UPDATE calendar_meet_drafts SET used_by='event:changed'`)
				case "nonce":
					_, err = tx.Exec(`UPDATE calendar_create_requests SET request_hash='reinserted-proof' WHERE request_id LIKE 'meeting:%'`)
				case "grant":
					grant.Store(false)
				case "owner":
					_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
				case "account":
					_, err = system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, c.Service().AccountID())
				case "principal":
					_, err = tx.Exec(`UPDATE accounts SET provider_account_id='new-subject'`)
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
						return errors.New("changed cleanup authority escaped writer wait")
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				var pending bool
				if err := db.Read().QueryRow(`SELECT cleanup_pending FROM calendar_meet_drafts`).Scan(&pending); err != nil {
					return err
				}
				if !pending {
					return errors.New("failed publication marked cleanup complete")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarMeetingCleanupRollsBackTriggerCorruption(t *testing.T) {
	for _, fault := range []string{"ignore", "source", "draft", "nonce"} {
		t.Run(fault, func(t *testing.T) {
			_, accounts, claims := ownedCleanupFixture(t, "gmail", false, false)
			c := claims["alice"]
			query := map[string]string{
				"ignore": `CREATE TRIGGER faulty BEFORE UPDATE ON calendar_meet_drafts BEGIN SELECT RAISE(IGNORE); END`,
				"source": `CREATE TRIGGER faulty AFTER UPDATE ON calendar_meet_drafts BEGIN UPDATE calendar_sources SET remote_id='replaced' WHERE id=NEW.source_id; END`,
				"draft":  `CREATE TRIGGER faulty AFTER UPDATE ON calendar_meet_drafts BEGIN UPDATE calendar_meet_drafts SET used_by='event:replaced' WHERE draft_id=NEW.draft_id; END`,
				"nonce":  `CREATE TRIGGER faulty AFTER UPDATE ON calendar_meet_drafts BEGIN UPDATE calendar_create_requests SET request_hash='replaced'; END`,
			}[fault]
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error { _, err := db.Write().Exec(query); return err }); err != nil {
				t.Fatal(err)
			}
			if _, err := accounts.PublishCalendarMeetingCleanup(t.Context(), c, storage.CalendarMeetingDraftCleaned, func() error { return nil }); err == nil {
				t.Fatal("false commit escaped", fault)
			}
			if err := accounts.ValidateCalendarMeetingCleanup(t.Context(), c); err != nil {
				t.Fatal("partial cleanup escaped rollback", err)
			}
		})
	}
}

func TestUserCalendarMeetingCleanupPruneRollsBackBothRowsAndOriginalTarget(t *testing.T) {
	for _, fault := range []string{"draft-ignore", "reservation-ignore", "reservation-abort", "source", "original", "reinsert"} {
		t.Run(fault, func(t *testing.T) {
			_, accounts, claims := ownedCleanupFixture(t, "outlook", true, true)
			c, err := accounts.PublishCalendarMeetingCleanup(t.Context(), claims["alice"], storage.CalendarMeetingDraftSaved, func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE calendar_teams_drafts SET updated_at=datetime('now','-8 days')`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			prune, err := accounts.SnapshotCalendarMeetingCleanup(t.Context(), "alice", c.Service().AccountID(), "same-source", ownedMeetingDraftID, true)
			if err != nil {
				t.Fatal(err)
			}
			trigger := map[string]string{
				"draft-ignore":       `CREATE TRIGGER faulty BEFORE DELETE ON calendar_teams_drafts BEGIN SELECT RAISE(IGNORE); END`,
				"reservation-ignore": `CREATE TRIGGER faulty BEFORE DELETE ON calendar_create_requests WHEN OLD.request_id LIKE 'meeting:%' BEGIN SELECT RAISE(IGNORE); END`,
				"reservation-abort":  `CREATE TRIGGER faulty BEFORE DELETE ON calendar_create_requests WHEN OLD.request_id LIKE 'meeting:%' BEGIN SELECT RAISE(ABORT,'retention interrupted'); END`,
				"source":             `CREATE TRIGGER faulty AFTER DELETE ON calendar_teams_drafts BEGIN UPDATE calendar_sources SET remote_id='replaced' WHERE id=OLD.source_id; END`,
				"original":           `CREATE TRIGGER faulty AFTER DELETE ON calendar_teams_drafts BEGIN UPDATE calendar_create_requests SET remote_id='replaced' WHERE request_id='original'; END`,
				"reinsert":           `CREATE TRIGGER faulty AFTER DELETE ON calendar_create_requests WHEN OLD.request_id LIKE 'meeting:%' BEGIN INSERT INTO calendar_create_requests(user_id,request_id,source_id,request_hash) VALUES(OLD.user_id,OLD.request_id,OLD.source_id,'new-reservation'); END`,
			}[fault]
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error { _, err := db.Write().Exec(trigger); return err }); err != nil {
				t.Fatal(err)
			}
			if err := accounts.PruneCalendarMeetingCleanup(t.Context(), prune); err == nil {
				t.Fatal("partial or corrupt retention committed")
			}
			if err := accounts.ValidateCalendarMeetingCleanup(t.Context(), prune); err != nil {
				t.Fatal("retention failed to roll back exact rows and original target", err)
			}
			if err := accounts.ValidateCalendarMeetingCleanup(t.Context(), claims["bob"]); err != nil {
				t.Fatal("retention crossed owners", err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				_, err := db.Write().Exec(`DROP TRIGGER faulty`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := accounts.PruneCalendarMeetingCleanup(t.Context(), prune); err != nil {
				t.Fatal("rollback did not leave retryable retention", err)
			}
		})
	}
}

func TestUserCalendarMeetingCleanupSavingTargetRechecksWriterWait(t *testing.T) {
	_, accounts, claims := ownedCleanupFixture(t, "outlook", true, false)
	c := claims["alice"]
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
			_, err := accounts.PublishCalendarMeetingCleanup(ctx, c, storage.CalendarMeetingDraftExpire, func() error { return nil })
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
		if _, err := tx.Exec(`UPDATE calendar_create_requests SET event_id='late-accepted',remote_id=? WHERE request_id='original'`, c.Teams().RemoteID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		select {
		case err := <-done:
			if err == nil {
				return errors.New("late completed create was expired")
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		var state string
		if err := db.Read().QueryRow(`SELECT state FROM calendar_teams_drafts`).Scan(&state); err != nil {
			return err
		}
		if state != "saving" {
			return fmt.Errorf("late accepted native save changed state: %s", state)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current, err := accounts.SnapshotCalendarMeetingCleanup(ctx, "alice", c.Service().AccountID(), "same-source", ownedMeetingDraftID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.PublishCalendarMeetingCleanup(ctx, current, storage.CalendarMeetingDraftSaved, func() error { return nil }); err != nil {
		t.Fatal("late acceptance cannot recover locally", err)
	}
}

func TestUserCalendarMeetingCleanupRejectsSavingWithoutRemoteIdentity(t *testing.T) {
	_, accounts, claims := ownedCleanupFixture(t, "outlook", true, false)
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE calendar_teams_drafts SET remote_id=''`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.SnapshotCalendarMeetingCleanup(t.Context(), "alice", claims["alice"].Service().AccountID(), "same-source", ownedMeetingDraftID, false); !errors.Is(err, storage.ErrCalendarCreateConflict) {
		t.Fatal("a corrupt saving row authorized native cleanup", err)
	}
}

func TestUserCalendarMeetingCleanupCursorFairnessSurvivesEvictionAndRefusesMutation(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			_, accounts, sources := ownedMeetingFixture(t, provider)
			for _, owner := range []string{"alice", "bob"} {
				if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
					query := `WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<23) INSERT INTO calendar_meet_drafts(user_id,source_id,draft_id,remote_id,created_at) SELECT ?,'same-source',printf('draft-%02d',i),printf('private-%02d',i),datetime('now','-2 hours') FROM n`
					if provider == "outlook" {
						query = `WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<23) INSERT INTO calendar_teams_drafts(user_id,source_id,draft_id,updated_at) SELECT ?,'same-source',printf('draft-%02d',i),datetime('now','-2 hours') FROM n`
					}
					_, err := db.Write().Exec(query, owner)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			// Deliberately leave every failed, unbound legacy candidate unchanged.
			// Alternating owners forces the one-store pool to close/reopen each pass.
			for i := 0; i < 49; i++ {
				for _, owner := range []string{"alice", "bob"} {
					c, err := accounts.NextCalendarMeetingCleanup(t.Context(), owner, sources[owner].Service().AccountID())
					if err != nil || c == nil {
						t.Fatal("next cleanup candidate", c, err)
					}
					if c.DraftID != fmt.Sprintf("draft-%02d", i%24) || c.SourceID != "same-source" || c.Provider != provider {
						t.Fatal("failed work starved later candidates", i, owner, c)
					}
					if _, err := accounts.SnapshotCalendarMeetingCleanup(t.Context(), owner, sources[owner].Service().AccountID(), c.SourceID, c.DraftID, c.Prune); err == nil {
						t.Fatal("scheduling cursor adopted an unbound native principal")
					}
				}
			}
			if _, err := accounts.NextCalendarMeetingCleanup(t.Context(), "alice", sources["bob"].Service().AccountID()); err == nil {
				t.Fatal("foreign account advanced cleanup cursor")
			}
			// A cursor write must recheck lifecycle after waiting for the writer.
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var calls int
				if _, err := db.NextAccountCalendarMeetingCleanup(t.Context(), "alice", sources["alice"].Service().AccountID(), func(_ *sql.Tx, _ string) error {
					calls++
					if calls > 1 {
						return storage.ErrAccountRoute
					}
					return nil
				}); !errors.Is(err, storage.ErrAccountRoute) {
					return fmt.Errorf("late cursor guard failed: %v", err)
				}
				var raw string
				if err := db.Read().QueryRow(`SELECT value FROM app_settings WHERE user_id='alice' AND key=?`, "calendar_meeting_cleanup_cursor:"+sources["alice"].Service().AccountID()).Scan(&raw); err != nil {
					return err
				}
				if !strings.Contains(raw, `"DraftID":"draft-00"`) {
					return fmt.Errorf("cursor escaped rollback: %s", raw)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
