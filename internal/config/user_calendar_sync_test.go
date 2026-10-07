package config

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func calendarSyncWindow() (time.Time, time.Time) {
	start := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	return start, start.Add(48 * time.Hour)
}

func seedCalendarSyncEvents(t *testing.T, accounts *UserAccountStore) {
	t.Helper()
	start, end := calendarSyncWindow()
	for _, owner := range []string{"alice", "bob"} {
		if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
			a, b := start.Add(time.Hour), start.Add(2*time.Hour)
			c, d := end.Add(24*time.Hour), end.Add(25*time.Hour)
			return db.ReplaceCalendarEvents(t.Context(), owner, "same-source", []storage.CalendarEvent{
				{ID: "same-event", RemoteID: "kept", Summary: owner + " old", StartAt: &a, EndAt: &b, ETag: "old-version"},
				{ID: "missing-event", RemoteID: "missing", Summary: owner + " missing", AllDay: true, StartDate: "2026-10-03", EndDate: "2026-10-04"},
				{ID: "outside-event", RemoteID: "outside", Summary: owner + " outside", StartAt: &c, EndAt: &d},
			}, start, end)
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func startCalendarSyncFor(t *testing.T, accounts *UserAccountStore, owner, id string) *UserCalendarSyncClaim {
	t.Helper()
	start, end := calendarSyncWindow()
	c, err := accounts.StartCalendarSync(t.Context(), owner, id, "same-source", start, end)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func calendarSyncInputs() []storage.CalendarEvent {
	start, _ := calendarSyncWindow()
	a, b := start.Add(3*time.Hour), start.Add(4*time.Hour)
	return []storage.CalendarEvent{
		{ID: "outside-event", UserID: "bob", SourceID: "second-source", RemoteID: "kept", Summary: "changed", ETag: "new-version", StartAt: &a, EndAt: &b, AttendeesJSON: `[{"email":"guest@example.com"}]`, OnlineMeetingJSON: `{"url":"https://meeting.test"}`},
		{ID: "same-event", UserID: "bob", SourceID: "second-source", RemoteID: "new", Summary: "new", AllDay: true, StartDate: "2026-10-03", EndDate: "2026-10-04"},
	}
}

func TestUserCalendarSyncOwnedAtomicWindowAndEviction(t *testing.T) {
	system, _, accounts, owners := newCalendarControlFixture(t)
	seedCalendarSyncEvents(t, accounts)
	c := startCalendarSyncFor(t, accounts, "alice", owners["alice"].ID)
	copy := c.Source()
	copy.RemoteID = "forged"
	if c.Source().RemoteID == "forged" || c.Attempt() != 1 || c.Service().OwnerID() != "alice" {
		t.Fatal("mutable or unowned claim")
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE calendar_sources SET is_hidden=1 WHERE id='same-source'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// MaxOpen=1. Opening Bob evicts Alice; a claim must remain usable afterward.
	if err := accounts.WithUser(t.Context(), "bob", func(*AccountStore, *storage.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := accounts.ValidateCalendarSync(t.Context(), c); err != nil {
		t.Fatal("eviction/display invalidated claim", err)
	}
	next := time.Now().Add(5 * time.Minute)
	if err := accounts.PublishCalendarSync(t.Context(), c, calendarSyncInputs(), next); err != nil {
		t.Fatal(err)
	}
	if err := accounts.PublishCalendarSync(t.Context(), c, calendarSyncInputs(), next); !errors.Is(err, storage.ErrCalendarSyncChanged) {
		t.Fatal("claim replay", err)
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
			event, err := db.GetCalendarEvent(t.Context(), owner, "same-event")
			if err != nil {
				return err
			}
			if owner == "bob" {
				if event.Summary != "bob old" {
					t.Fatal("cross-owner publication", event)
				}
				return nil
			}
			if event.RemoteID != "kept" || event.Summary != "changed" || event.ETag != "new-version" || !event.SourceHidden || event.AttendeesJSON != calendarSyncInputs()[0].AttendeesJSON {
				t.Fatal("event identity or details lost", event)
			}
			var id, storedOwner, source string
			if err := db.Read().QueryRow(`SELECT id,user_id,source_id FROM calendar_events WHERE remote_id='new'`).Scan(&id, &storedOwner, &source); err != nil {
				return err
			}
			if id == "same-event" || id == "outside-event" || storedOwner != "alice" || source != "same-source" {
				t.Fatal("provider chose local identity", id, storedOwner, source)
			}
			var missing, outside int
			if err := db.Read().QueryRow(`SELECT is_deleted FROM calendar_events WHERE id='missing-event'`).Scan(&missing); err != nil {
				return err
			}
			if err := db.Read().QueryRow(`SELECT is_deleted FROM calendar_events WHERE id='outside-event'`).Scan(&outside); err != nil {
				return err
			}
			if missing != 1 || outside != 0 {
				t.Fatal("window reconciliation", missing, outside)
			}
			var state string
			var scheduled sql.NullString
			if err := db.Read().QueryRow(`SELECT state,next_attempt_at FROM calendar_sync_state WHERE source_id='same-source'`).Scan(&state, &scheduled); err != nil {
				return err
			}
			if state != "ok" || !scheduled.Valid {
				t.Fatal("state not published", state, scheduled)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events`).Scan(&count); err != nil || count != 0 {
		t.Fatal("central fallback", count, err)
	}
}

func TestUserCalendarSyncSupersededInvalidAndFailure(t *testing.T) {
	_, routing, accounts, owners := newCalendarControlFixture(t)
	seedCalendarSyncEvents(t, accounts)
	c := startCalendarSyncFor(t, accounts, "alice", owners["alice"].ID)
	newer := startCalendarSyncFor(t, accounts, "alice", owners["alice"].ID)
	if newer.Attempt() != c.Attempt()+1 {
		t.Fatal("attempt identity not advanced")
	}
	next := time.Now().Add(time.Minute)
	if err := accounts.FailCalendarSync(t.Context(), c, "old failure", next); !errors.Is(err, storage.ErrCalendarSyncChanged) {
		t.Fatal("old claim finalized newer work", err)
	}
	if err := accounts.PublishCalendarSync(t.Context(), c, calendarSyncInputs(), next); !errors.Is(err, storage.ErrCalendarSyncChanged) {
		t.Fatal("old claim published", err)
	}
	if err := accounts.FailCalendarSync(t.Context(), newer, "provider unavailable", next); err != nil {
		t.Fatal(err)
	}
	other, err := NewUserAccountStore(routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []*UserCalendarSyncClaim{nil, {}, newer} {
		if err := other.ValidateCalendarSync(t.Context(), invalid); err == nil {
			t.Fatal("invalid claim accepted")
		}
		if err := other.PublishCalendarSync(t.Context(), invalid, nil, next); err == nil {
			t.Fatal("invalid publication accepted")
		}
		if err := other.FailCalendarSync(t.Context(), invalid, "failure", next); err == nil {
			t.Fatal("invalid failure accepted")
		}
	}
	start, end := calendarSyncWindow()
	for _, input := range []struct {
		owner, account, source string
		start, end             time.Time
	}{
		{"alice", owners["bob"].ID, "same-source", start, end},
		{"alice", owners["alice"].ID, "second-source", start, end},
		{"alice", owners["alice"].ID, "same-source", end, start},
	} {
		if claim, err := accounts.StartCalendarSync(t.Context(), input.owner, input.account, input.source, input.start, input.end); err == nil || claim != nil {
			t.Fatal("invalid start accepted", input, err)
		}
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		var state, message, summary string
		if err := db.Read().QueryRow(`SELECT state,last_error FROM calendar_sync_state WHERE source_id='same-source'`).Scan(&state, &message); err != nil {
			return err
		}
		if err := db.Read().QueryRow(`SELECT summary FROM calendar_events WHERE id='same-event'`).Scan(&summary); err != nil {
			return err
		}
		if state != "failed" || message != "provider unavailable" || summary != "alice old" {
			t.Fatal("failure changed cache", state, message, summary)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserCalendarSyncWriterWaitRejectsChangedAuthorityAndCache(t *testing.T) {
	for _, change := range []string{"owner", "account", "connection", "caldav", "source", "selection", "event", "attempt", "authorization"} {
		t.Run(change, func(t *testing.T) {
			system, _, accounts, owners := newCalendarControlFixture(t)
			seedCalendarSyncEvents(t, accounts)
			c := startCalendarSyncFor(t, accounts, "alice", owners["alice"].ID)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
				tx, err := db.Write().BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				before := db.Write().Stats().WaitCount
				var revoked atomic.Bool
				done := make(chan error, 1)
				go func() {
					done <- accounts.PublishCalendarSync(ctx, c, calendarSyncInputs(), time.Now().Add(time.Minute), func() error {
						if revoked.Load() {
							return errors.New("authorization replaced")
						}
						return nil
					})
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
					_, err = system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, owners["alice"].ID)
				case "connection":
					_, err = tx.Exec(`UPDATE accounts SET username='edited' WHERE id=?`, owners["alice"].ID)
				case "caldav":
					_, err = tx.Exec(`UPDATE account_caldav_configs SET base_url='https://edited.test' WHERE account_id=?`, owners["alice"].ID)
				case "source":
					_, err = tx.Exec(`UPDATE calendar_sources SET remote_id='/edited/' WHERE id='same-source'`)
				case "selection":
					_, err = tx.Exec(`UPDATE calendar_sources SET is_selected=0 WHERE id='same-source'`)
				case "event":
					_, err = tx.Exec(`UPDATE calendar_events SET etag='incoming-response' WHERE id='same-event'`)
				case "attempt":
					_, err = tx.Exec(`UPDATE calendar_sync_state SET attempt_count=attempt_count+1 WHERE source_id='same-source'`)
				case "authorization":
					revoked.Store(true)
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
						t.Fatal("late publication accepted change")
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				var summary string
				if err := db.Read().QueryRow(`SELECT summary FROM calendar_events WHERE id='same-event'`).Scan(&summary); err != nil {
					return err
				}
				if summary != "alice old" {
					t.Fatal("late publication leaked", summary)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarSyncAtomicFaultsAndRetry(t *testing.T) {
	for _, fault := range []string{"insert", "insert-ignore", "missing", "missing-ignore", "state", "state-ignore"} {
		t.Run(fault, func(t *testing.T) {
			_, _, accounts, owners := newCalendarControlFixture(t)
			seedCalendarSyncEvents(t, accounts)
			c := startCalendarSyncFor(t, accounts, "alice", owners["alice"].ID)
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				raise := "SELECT RAISE(ABORT,'synthetic event fault');"
				if fault == "insert-ignore" || fault == "missing-ignore" || fault == "state-ignore" {
					raise = "SELECT RAISE(IGNORE);"
				}
				query := ""
				switch fault {
				case "insert", "insert-ignore":
					query = "CREATE TRIGGER fail_sync BEFORE INSERT ON calendar_events WHEN NEW.remote_id='new' BEGIN " + raise + " END"
				case "missing", "missing-ignore":
					query = "CREATE TRIGGER fail_sync BEFORE UPDATE ON calendar_events WHEN NEW.id='missing-event' AND NEW.is_deleted=1 BEGIN " + raise + " END"
				case "state", "state-ignore":
					query = "CREATE TRIGGER fail_sync BEFORE UPDATE ON calendar_sync_state WHEN NEW.state='ok' BEGIN " + raise + " END"
				}
				_, err := db.Write().Exec(query)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			next := time.Now().Add(time.Minute)
			if err := accounts.PublishCalendarSync(t.Context(), c, calendarSyncInputs(), next); err == nil {
				t.Fatal("fault accepted")
			}
			if err := accounts.ValidateCalendarSync(t.Context(), c); err != nil {
				t.Fatal("failed transaction changed baseline", err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var summary string
				var missing, newEvents int
				if err := db.Read().QueryRow(`SELECT summary FROM calendar_events WHERE id='same-event'`).Scan(&summary); err != nil {
					return err
				}
				if err := db.Read().QueryRow(`SELECT is_deleted FROM calendar_events WHERE id='missing-event'`).Scan(&missing); err != nil {
					return err
				}
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events WHERE remote_id='new'`).Scan(&newEvents); err != nil {
					return err
				}
				if summary != "alice old" || missing != 0 || newEvents != 0 {
					t.Fatal("partial cache escaped rollback", summary, missing, newEvents)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				_, err := db.Write().Exec(`DROP TRIGGER fail_sync`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := accounts.PublishCalendarSync(t.Context(), c, calendarSyncInputs(), next); err != nil {
				t.Fatal("retry", err)
			}
		})
	}
}

func TestUserCalendarSyncCancellationWhileWaitingForWriter(t *testing.T) {
	for _, operation := range []string{"start", "publish", "fail"} {
		t.Run(operation, func(t *testing.T) {
			_, _, accounts, owners := newCalendarControlFixture(t)
			seedCalendarSyncEvents(t, accounts)
			c := startCalendarSyncFor(t, accounts, "alice", owners["alice"].ID)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				tx, err := db.Write().BeginTx(t.Context(), nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				before := db.Write().Stats().WaitCount
				done := make(chan error, 1)
				go func() {
					var err error
					switch operation {
					case "start":
						start, end := calendarSyncWindow()
						var result *UserCalendarSyncClaim
						result, err = accounts.StartCalendarSync(ctx, "alice", owners["alice"].ID, "same-source", start, end)
						if result != nil {
							t.Error("canceled start returned claim")
						}
					case "publish":
						err = accounts.PublishCalendarSync(ctx, c, calendarSyncInputs(), time.Now().Add(time.Minute))
					case "fail":
						err = accounts.FailCalendarSync(ctx, c, "provider error", time.Now().Add(time.Minute))
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
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatal("canceled operation", err)
					}
				case <-time.After(time.Second):
					t.Fatal("cancellation did not join writer waiter")
				}
				if err := tx.Rollback(); err != nil {
					return err
				}
				var summary, state string
				var attempt int
				if err := db.Read().QueryRow(`SELECT summary FROM calendar_events WHERE id='same-event'`).Scan(&summary); err != nil {
					return err
				}
				if err := db.Read().QueryRow(`SELECT state,attempt_count FROM calendar_sync_state WHERE source_id='same-source'`).Scan(&state, &attempt); err != nil {
					return err
				}
				if summary != "alice old" || state != "syncing" || attempt != c.Attempt() {
					t.Fatal("canceled operation leaked state", summary, state, attempt)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarSyncStartRollsBackIgnoredAttempt(t *testing.T) {
	_, _, accounts, owners := newCalendarControlFixture(t)
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER ignore_start BEFORE UPDATE ON calendar_sync_state WHEN NEW.state='syncing' BEGIN SELECT RAISE(IGNORE); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	start, end := calendarSyncWindow()
	if result, err := accounts.StartCalendarSync(t.Context(), "alice", owners["alice"].ID, "same-source", start, end); err == nil || result != nil {
		t.Fatal("ignored start returned claim", err)
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		var attempt int
		if err := db.Read().QueryRow(`SELECT attempt_count FROM calendar_sync_state WHERE source_id='same-source'`).Scan(&attempt); err != nil {
			return err
		}
		if attempt != 0 {
			t.Fatal("failed start leaked attempt", attempt)
		}
		_, err := db.Write().Exec(`DROP TRIGGER ignore_start`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if c := startCalendarSyncFor(t, accounts, "alice", owners["alice"].ID); c.Attempt() != 1 {
		t.Fatal("start retry did not retain first attempt")
	}
}
