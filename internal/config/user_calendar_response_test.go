package config

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserCalendarResponseReservationDurableIsolationScopeAndReplacement(t *testing.T) {
	system, _, accounts, _ := newCalendarControlFixture(t)
	seedCalendarSyncEvents(t, accounts)
	alice, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := accounts.ReserveCalendarResponse(t.Context(), alice, "event", alice.Event().ETag, "accepted")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := accounts.SnapshotCalendarEvent(t.Context(), "bob", "same-event")
	if err != nil {
		t.Fatal(err)
	}
	other, err := accounts.ReserveCalendarResponse(t.Context(), bob, "event", bob.Event().ETag, "declined")
	if err != nil {
		t.Fatal(err)
	}
	for _, response := range []string{"accepted", "declined"} {
		if _, err := accounts.ReserveCalendarResponse(t.Context(), alice, "event", alice.Event().ETag, response); !errors.Is(err, storage.ErrCalendarResponsePending) {
			t.Fatal("eviction/restart lost pending response", err)
		}
	}
	for _, scope := range []string{"occurrence", "series", "invalid"} {
		if _, err := accounts.ReserveCalendarResponse(t.Context(), alice, scope, alice.Event().ETag, "accepted"); !errors.Is(err, storage.ErrCalendarEventChanged) {
			t.Fatal("unsafe scope accepted", scope, err)
		}
	}
	for _, version := range []string{"", "different-version"} {
		if _, err := accounts.ReserveCalendarResponse(t.Context(), alice, "event", version, "accepted"); !errors.Is(err, storage.ErrCalendarEventChanged) {
			t.Fatal("unverified version reserved", err)
		}
	}
	if err := accounts.ReleaseCalendarResponse(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	replacement, err := accounts.ReserveCalendarResponse(t.Context(), alice, "event", alice.Event().ETag, "accepted")
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.ReleaseCalendarResponse(t.Context(), claim); !errors.Is(err, storage.ErrCalendarEventChanged) {
		t.Fatal("late release removed replacement", err)
	}
	if _, err := accounts.ReserveCalendarResponse(t.Context(), alice, "event", alice.Event().ETag, "declined"); !errors.Is(err, storage.ErrCalendarResponsePending) {
		t.Fatal("replacement no longer guards duplicate", err)
	}
	if err := accounts.ReleaseCalendarResponse(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.ReserveCalendarResponse(t.Context(), bob, "event", bob.Event().ETag, "accepted"); !errors.Is(err, storage.ErrCalendarResponsePending) {
		t.Fatal("Alice release changed Bob reservation", err)
	}
	if err := accounts.ReleaseCalendarResponse(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	// Historical uncertain reservations keep empty claim IDs and remain pending.
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		return db.BeginCalendarResponse(t.Context(), "alice", alice.Source().ID, alice.Event().RemoteID, alice.Event().ETag, "accepted")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.ReserveCalendarResponse(t.Context(), alice, "event", alice.Event().ETag, "accepted"); !errors.Is(err, storage.ErrCalendarResponsePending) {
		t.Fatal("legacy uncertainty adopted", err)
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		var id string
		err := db.Read().QueryRow(`SELECT claim_id FROM calendar_response_requests`).Scan(&id)
		if err == nil && id != "" {
			t.Fatal("legacy reservation rebound")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM calendar_response_requests`).Scan(&n); err != nil || n != 0 {
		t.Fatal("central reservation fallback", n, err)
	}
}

func TestUserCalendarResponseReservationSeriesDerivesPrivateParent(t *testing.T) {
	_, _, accounts, _ := newCalendarControlFixture(t)
	seedCalendarSyncEvents(t, accounts)
	calendarMutationSeriesFixture(t, accounts)
	snapshot, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"series", "occurrence"} {
		version := "master-version"
		if scope == "occurrence" {
			version = snapshot.Event().ETag
		}
		claim, err := accounts.ReserveCalendarResponse(t.Context(), snapshot, scope, version, "tentative")
		if err != nil {
			t.Fatal(err)
		}
		if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
			var remote, savedVersion string
			err := db.Read().QueryRow(`SELECT remote_id,version FROM calendar_response_requests`).Scan(&remote, &savedVersion)
			want := snapshot.Event().RemoteID
			if scope == "series" {
				want = snapshot.Event().SeriesRemoteID
			}
			if err == nil && (remote != want || savedVersion != version) {
				t.Fatal("reservation chose wrong private remote/version", remote, savedVersion)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := accounts.ReleaseCalendarResponse(t.Context(), claim); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUserCalendarResponseReservationWriterWaitRechecksAuthority(t *testing.T) {
	for _, operation := range []string{"reserve", "release"} {
		for _, change := range []string{"event", "source", "config", "grant", "owner", "account"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				system, _, accounts, owners := newCalendarControlFixture(t)
				seedCalendarSyncEvents(t, accounts)
				snapshot, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
				if err != nil {
					t.Fatal(err)
				}
				revoked := atomic.Bool{}
				changed := errors.New("central write grant changed")
				guard := func() error {
					if revoked.Load() {
						return changed
					}
					return nil
				}
				var claim *UserCalendarResponseClaim
				if operation == "release" {
					claim, err = accounts.ReserveCalendarResponse(t.Context(), snapshot, "event", snapshot.Event().ETag, "accepted", guard)
					if err != nil {
						t.Fatal(err)
					}
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
					done := make(chan error, 1)
					go func() {
						if operation == "reserve" {
							_, err := accounts.ReserveCalendarResponse(ctx, snapshot, "event", snapshot.Event().ETag, "accepted", guard)
							done <- err
						} else {
							done <- accounts.ReleaseCalendarResponse(ctx, claim, guard)
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
					switch change {
					case "event":
						_, err = tx.Exec(`UPDATE calendar_events SET attendees_json='[{"email":"changed"}]' WHERE id='same-event'`)
					case "source":
						_, err = tx.Exec(`UPDATE calendar_sources SET remote_id='/changed/' WHERE id='same-source'`)
					case "config":
						_, err = tx.Exec(`UPDATE account_caldav_configs SET username='changed' WHERE account_id=?`, owners["alice"].ID)
					case "grant":
						revoked.Store(true)
					case "owner":
						_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
					case "account":
						_, err = system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, owners["alice"].ID)
					}
					if err != nil {
						return err
					}
					if err := tx.Commit(); err != nil {
						return err
					}
					select {
					case err := <-done:
						want := storage.ErrCalendarEventChanged
						switch change {
						case "config":
							want = ErrAccountServicesChanged
						case "grant":
							want = changed
						case "owner":
							want = storage.ErrUserStoreOwner
						case "account":
							want = storage.ErrAccountRoute
						}
						if !errors.Is(err, want) {
							t.Fatal("late reservation/release missed authority transition", err, want)
						}
					case <-ctx.Done():
						return ctx.Err()
					}
					var n int
					if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_response_requests`).Scan(&n); err != nil {
						return err
					}
					want := 0
					if operation == "release" {
						want = 1
					}
					if n != want {
						t.Fatal("failed boundary changed durable guard", operation, n)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestUserCalendarResponseReservationRejectsTriggerFaults(t *testing.T) {
	for _, mode := range []string{"insert-ignore", "insert-change-claim", "insert-change-event", "delete-ignore", "delete-reinsert"} {
		t.Run(mode, func(t *testing.T) {
			_, _, accounts, _ := newCalendarControlFixture(t)
			seedCalendarSyncEvents(t, accounts)
			snapshot, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
			if err != nil {
				t.Fatal(err)
			}
			releasing := strings.HasPrefix(mode, "delete")
			var claim *UserCalendarResponseClaim
			if releasing {
				claim, err = accounts.ReserveCalendarResponse(t.Context(), snapshot, "event", snapshot.Event().ETag, "accepted")
				if err != nil {
					t.Fatal(err)
				}
			}
			trigger := map[string]string{
				"insert-ignore":       `CREATE TRIGGER fault BEFORE INSERT ON calendar_response_requests BEGIN SELECT RAISE(IGNORE); END`,
				"insert-change-claim": `CREATE TRIGGER fault AFTER INSERT ON calendar_response_requests BEGIN UPDATE calendar_response_requests SET claim_id='changed'; END`,
				"insert-change-event": `CREATE TRIGGER fault AFTER INSERT ON calendar_response_requests BEGIN UPDATE calendar_events SET summary='changed' WHERE id='same-event'; END`,
				"delete-ignore":       `CREATE TRIGGER fault BEFORE DELETE ON calendar_response_requests BEGIN SELECT RAISE(IGNORE); END`,
				"delete-reinsert":     `CREATE TRIGGER fault AFTER DELETE ON calendar_response_requests BEGIN INSERT INTO calendar_response_requests(user_id,source_id,remote_id,version,response,claim_id) VALUES(old.user_id,old.source_id,old.remote_id,old.version,old.response,old.claim_id); END`,
			}[mode]
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error { _, err := db.Write().Exec(trigger); return err }); err != nil {
				t.Fatal(err)
			}
			if releasing {
				err = accounts.ReleaseCalendarResponse(t.Context(), claim)
			} else {
				_, err = accounts.ReserveCalendarResponse(t.Context(), snapshot, "event", snapshot.Event().ETag, "accepted")
			}
			if err == nil {
				t.Fatal("trigger fault reported known successful reservation/release")
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var n int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_response_requests`).Scan(&n); err != nil {
					return err
				}
				want := 0
				if releasing {
					want = 1
				}
				if n != want {
					t.Fatal("partial reservation/release survived", n)
				}
				event, err := db.GetCalendarEvent(t.Context(), "alice", "same-event")
				if err == nil && event.Summary != snapshot.Event().Summary {
					t.Fatal("failed reservation changed event")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarResponseReservationRequiresBoundRepositoryAndOAuthGrant(t *testing.T) {
	_, routing, accounts, _ := newCalendarControlFixture(t)
	seedCalendarSyncEvents(t, accounts)
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET provider='outlook',auth_method='oauth2',provider_account_id='subject'; UPDATE calendar_sources SET provider='outlook',access_role='owner'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]func() error{nil, {nil}, {func() error { return nil }, func() error { return nil }}} {
		if _, err := accounts.ReserveCalendarResponse(t.Context(), snapshot, "event", snapshot.Event().ETag, "accepted", extra...); !errors.Is(err, storage.ErrCalendarEventChanged) {
			t.Fatal("missing/invalid OAuth grant guard accepted", err)
		}
	}
	var calls atomic.Int32
	guard := func() error { calls.Add(1); return nil }
	claim, err := accounts.ReserveCalendarResponse(t.Context(), snapshot, "event", snapshot.Event().ETag, "accepted", guard)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("central grant not checked before/after local mutation", calls.Load())
	}
	other, err := NewUserAccountStore(routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []*UserCalendarResponseClaim{nil, {}, claim} {
		if err := other.ReleaseCalendarResponse(t.Context(), bad, guard); !errors.Is(err, storage.ErrCalendarEventChanged) {
			t.Fatal("foreign/unbound claim accepted", err)
		}
	}
	if err := accounts.ReleaseCalendarResponse(t.Context(), claim); !errors.Is(err, storage.ErrCalendarEventChanged) {
		t.Fatal("OAuth release without grant accepted", err)
	}
	if err := accounts.ReleaseCalendarResponse(t.Context(), claim, guard); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatal("release central grant not checked twice", calls.Load())
	}
}
