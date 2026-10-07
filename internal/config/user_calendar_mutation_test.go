package config

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func calendarMutationResult(snapshot *UserCalendarEventSnapshot) storage.CalendarEvent {
	event := snapshot.Event()
	event.ID, event.UserID, event.SourceID = "forged-event", "bob", "second-source"
	event.ETag, event.Summary, event.Description, event.Location = "new-version", "confirmed change", "confirmed description", "confirmed location"
	event.StartAt, event.EndAt = calendarMutationTime(10), calendarMutationTime(11)
	event.ProviderUpdatedAt = calendarMutationTime(9)
	return event
}

func calendarMutationTime(hour int) *time.Time {
	at := time.Date(2026, 10, 3, hour, 0, 0, 123, time.FixedZone("provider", 2*60*60))
	return &at
}

func calendarMutationSeriesFixture(t *testing.T, accounts *UserAccountStore) {
	t.Helper()
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE calendar_events SET series_remote_id='master',ical_uid='uid',etag='old-version' WHERE id IN ('same-event','outside-event');
UPDATE calendar_events SET series_remote_id='master',ical_uid='different-uid',etag='old-version' WHERE id='missing-event';
INSERT INTO calendar_events(id,user_id,source_id,remote_id,series_remote_id,ical_uid,etag,summary,all_day,start_at,end_at)
SELECT 'newer-event',user_id,source_id,'newer','master','uid','newer-version','already advanced',all_day,start_at,end_at FROM calendar_events WHERE id='same-event'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserCalendarPublicationOwnsIDsAndCompleteResultAcrossEviction(t *testing.T) {
	for _, kind := range []storage.CalendarEventPublicationKind{storage.CalendarPublishUpdate, storage.CalendarPublishOnlineUpdate, storage.CalendarPublishSeriesConversion, storage.CalendarPublishDelete, storage.CalendarPublishResponse} {
		t.Run(map[storage.CalendarEventPublicationKind]string{storage.CalendarPublishUpdate: "update", storage.CalendarPublishOnlineUpdate: "online", storage.CalendarPublishSeriesConversion: "conversion", storage.CalendarPublishDelete: "delete", storage.CalendarPublishResponse: "response"}[kind], func(t *testing.T) {
			system, _, accounts, _ := newCalendarControlFixture(t)
			seedCalendarSyncEvents(t, accounts)
			var guards []func() error
			if kind == storage.CalendarPublishOnlineUpdate {
				if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE accounts SET provider='gmail',auth_method='oauth2',provider_account_id='subject'; UPDATE calendar_sources SET provider='gmail'`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				guards = []func() error{func() error { return nil }}
			}
			snapshot, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
			if err != nil {
				t.Fatal(err)
			}
			event := calendarMutationResult(snapshot)
			if kind == storage.CalendarPublishOnlineUpdate {
				if err := accounts.PublishCalendarEvent(t.Context(), snapshot, kind, event); !errors.Is(err, storage.ErrCalendarEventChanged) {
					t.Fatal("OAuth publication accepted without its central grant guard", err)
				}
			}
			if kind == storage.CalendarPublishOnlineUpdate {
				event.ICalUID, event.OrganizerName, event.OrganizerEmail = "confirmed-uid", "organizer", "alice@example.com"
				event.ResponseStatus, event.AttendeesJSON, event.OnlineMeetingJSON = "organizer", `[{"email":"guest@example.com"}]`, `{"conference":"confirmed"}`
			}
			if kind == storage.CalendarPublishSeriesConversion {
				event.RecurrenceJSON = `["RRULE:FREQ=DAILY"]`
			}
			if kind == storage.CalendarPublishResponse {
				event.ResponseStatus, event.AttendeesJSON, event.OnlineMeetingJSON = "accepted", `[{"email":"alice@example.com","status":"accepted"}]`, `{"conference":"readback"}`
				event.OrganizerName, event.OrganizerEmail, event.HTMLLink = "host", "host@example.com", "https://provider.test/confirmed"
			}
			if err := accounts.WithUser(t.Context(), "bob", func(*AccountStore, *storage.DB) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err := accounts.PublishCalendarEvent(t.Context(), snapshot, kind, event, guards...); err != nil {
				t.Fatal(err)
			}
			if err := accounts.PublishCalendarEvent(t.Context(), snapshot, kind, event, guards...); !errors.Is(err, storage.ErrCalendarEventChanged) {
				t.Fatal("publication replay accepted", err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var id, owner, source, summary, description, etag, attendees, meeting string
				var deleted int
				if err := db.Read().QueryRow(`SELECT id,user_id,source_id,summary,description,etag,attendees_json,online_meeting_json,is_deleted FROM calendar_events WHERE id='same-event'`).Scan(&id, &owner, &source, &summary, &description, &etag, &attendees, &meeting, &deleted); err != nil {
					return err
				}
				if id != "same-event" || owner != "alice" || source != "same-source" {
					t.Fatal("provider chose local row identity", id, owner, source)
				}
				if kind == storage.CalendarPublishDelete {
					if deleted != 1 || summary != "alice old" || etag != "old-version" {
						t.Fatal("delete changed unrelated fields")
					}
				} else {
					if summary != event.Summary || description != event.Description || etag != event.ETag {
						t.Fatal("confirmed editable fields not published")
					}
					if (kind == storage.CalendarPublishSeriesConversion) != (deleted == 1) {
						t.Fatal("conversion tombstone missing or ordinary update hidden")
					}
				}
				if kind == storage.CalendarPublishOnlineUpdate || kind == storage.CalendarPublishResponse {
					if attendees != event.AttendeesJSON || meeting != event.OnlineMeetingJSON {
						t.Fatal("new version attached to stale metadata")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := accounts.WithUser(t.Context(), "bob", func(_ *AccountStore, db *storage.DB) error {
				event, err := db.GetCalendarEvent(t.Context(), "bob", "same-event")
				if err == nil && (event.Summary != "bob old" || event.ETag != "old-version") {
					t.Fatal("colliding Bob event changed")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := system.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events`).Scan(&count); err != nil || count != 0 {
				t.Fatal("central fallback", count, err)
			}
		})
	}
}

func TestUserCalendarPublicationSeriesAndDAVOccurrenceAtomicScope(t *testing.T) {
	for _, kind := range []storage.CalendarEventPublicationKind{storage.CalendarPublishOccurrenceUpdate, storage.CalendarPublishOccurrenceDelete, storage.CalendarPublishSeriesUpdate, storage.CalendarPublishSeriesDelete} {
		t.Run(map[storage.CalendarEventPublicationKind]string{storage.CalendarPublishOccurrenceUpdate: "occurrence-update", storage.CalendarPublishOccurrenceDelete: "occurrence-delete", storage.CalendarPublishSeriesUpdate: "series-update", storage.CalendarPublishSeriesDelete: "series-delete"}[kind], func(t *testing.T) {
			_, _, accounts, _ := newCalendarControlFixture(t)
			seedCalendarSyncEvents(t, accounts)
			calendarMutationSeriesFixture(t, accounts)
			snapshot, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
			if err != nil {
				t.Fatal(err)
			}
			event := calendarMutationResult(snapshot)
			series := kind == storage.CalendarPublishSeriesUpdate || kind == storage.CalendarPublishSeriesDelete
			if series {
				event.RemoteID, event.SeriesRemoteID, event.RecurrenceJSON = "master", "", `["RRULE:FREQ=DAILY"]`
			}
			if err := accounts.PublishCalendarEvent(t.Context(), snapshot, kind, event); err != nil {
				t.Fatal(err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				for _, id := range []string{"same-event", "outside-event", "missing-event", "newer-event"} {
					var etag, summary, status string
					var deleted int
					if err := db.Read().QueryRow(`SELECT etag,summary,status,is_deleted FROM calendar_events WHERE id=?`, id).Scan(&etag, &summary, &status, &deleted); err != nil {
						return err
					}
					if series {
						if deleted != 1 || (kind == storage.CalendarPublishSeriesDelete && status != "cancelled") {
							t.Fatal("live series member outside refreshed window", id, deleted, status)
						}
						if etag == event.ETag {
							t.Fatal("master version attached to occurrence", id)
						}
					} else {
						want := "old-version"
						if id == "same-event" || id == "outside-event" {
							want = event.ETag
						}
						if id == "newer-event" {
							want = "newer-version"
						}
						if etag != want {
							t.Fatal("DAV sibling version scope", id, etag, want)
						}
						if id != "same-event" && deleted != 0 {
							t.Fatal("occurrence write hid sibling", id)
						}
						if id == "same-event" && (deleted == 1) != (kind == storage.CalendarPublishOccurrenceDelete) {
							t.Fatal("occurrence deletion state", deleted)
						}
					}
					if id == "outside-event" && summary != "alice outside" {
						t.Fatal("sibling content changed")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarPublicationWriterWaitRechecksEventConfigLifecycleAndGrant(t *testing.T) {
	for _, change := range []string{"event-same-version", "source", "service", "account", "owner", "grant"} {
		t.Run(change, func(t *testing.T) {
			system, _, accounts, owners := newCalendarControlFixture(t)
			seedCalendarSyncEvents(t, accounts)
			snapshot, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
			if err != nil {
				t.Fatal(err)
			}
			var replaced atomic.Bool
			grantError := errors.New("grant replaced")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
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
					done <- accounts.PublishCalendarEvent(ctx, snapshot, storage.CalendarPublishUpdate, calendarMutationResult(snapshot), func() error {
						if replaced.Load() {
							return grantError
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
				case "event-same-version":
					_, err = tx.Exec(`UPDATE calendar_events SET attendees_json='[{"email":"late@example.com"}]' WHERE id='same-event'`)
				case "source":
					_, err = tx.Exec(`UPDATE calendar_sources SET remote_id='/replaced/' WHERE id='same-source'`)
				case "service":
					_, err = tx.Exec(`UPDATE account_caldav_configs SET base_url='https://replaced.test/' WHERE account_id=?`, owners["alice"].ID)
				case "account":
					_, err = system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, owners["alice"].ID)
				case "owner":
					_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
				case "grant":
					replaced.Store(true)
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
					case "service":
						want = ErrAccountServicesChanged
					case "account":
						want = storage.ErrAccountRoute
					case "owner":
						want = storage.ErrUserStoreOwner
					case "grant":
						want = grantError
					}
					if !errors.Is(err, want) {
						t.Fatal("late writer-wait transition accepted", change, err, want)
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				var summary, etag string
				if err := db.Read().QueryRow(`SELECT summary,etag FROM calendar_events WHERE id='same-event'`).Scan(&summary, &etag); err != nil {
					return err
				}
				if summary != "alice old" || etag != "old-version" {
					t.Fatal("stale publication escaped")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarPublicationRejectsPartialAndMutatedCommits(t *testing.T) {
	for _, fault := range []string{"ignore-selected", "ignore-series-member", "ignore-sibling-version", "mutate-details", "mutate-created", "change-source"} {
		t.Run(fault, func(t *testing.T) {
			_, _, accounts, _ := newCalendarControlFixture(t)
			seedCalendarSyncEvents(t, accounts)
			kind := storage.CalendarPublishUpdate
			if fault == "ignore-series-member" || fault == "ignore-sibling-version" {
				calendarMutationSeriesFixture(t, accounts)
			}
			if fault == "ignore-series-member" {
				kind = storage.CalendarPublishSeriesDelete
			}
			if fault == "ignore-sibling-version" {
				kind = storage.CalendarPublishOccurrenceUpdate
			}
			snapshot, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
			if err != nil {
				t.Fatal(err)
			}
			event := calendarMutationResult(snapshot)
			if kind == storage.CalendarPublishSeriesDelete {
				event.RemoteID, event.SeriesRemoteID, event.RecurrenceJSON = "master", "", `["RRULE:FREQ=DAILY"]`
			}
			trigger := map[string]string{
				"ignore-selected":        `CREATE TRIGGER faulty BEFORE UPDATE ON calendar_events WHEN OLD.id='same-event' BEGIN SELECT RAISE(IGNORE);END`,
				"ignore-series-member":   `CREATE TRIGGER faulty BEFORE UPDATE ON calendar_events WHEN OLD.id='outside-event' BEGIN SELECT RAISE(IGNORE);END`,
				"ignore-sibling-version": `CREATE TRIGGER faulty BEFORE UPDATE OF etag ON calendar_events WHEN OLD.id='outside-event' BEGIN SELECT RAISE(IGNORE);END`,
				"mutate-details":         `CREATE TRIGGER faulty AFTER UPDATE ON calendar_events WHEN NEW.id='same-event' BEGIN UPDATE calendar_events SET attendees_json='[{"email":"corrupt@example.com"}]' WHERE id=NEW.id;END`,
				"mutate-created":         `CREATE TRIGGER faulty AFTER UPDATE ON calendar_events WHEN NEW.id='same-event' BEGIN UPDATE calendar_events SET created_at='2000-01-01' WHERE id=NEW.id;END`,
				"change-source":          `CREATE TRIGGER faulty AFTER UPDATE ON calendar_events WHEN NEW.id='same-event' BEGIN UPDATE calendar_sources SET remote_id='/changed-by-trigger/' WHERE id=NEW.source_id;END`,
			}[fault]
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error { _, err := db.Write().Exec(trigger); return err }); err != nil {
				t.Fatal(err)
			}
			if err := accounts.PublishCalendarEvent(t.Context(), snapshot, kind, event); err == nil {
				t.Fatal("false known-commit result", fault)
			}
			if err := accounts.ValidateCalendarEvent(t.Context(), snapshot); err != nil {
				t.Fatal("failed compound publication did not roll back", err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var tombstones int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events WHERE is_deleted=1`).Scan(&tombstones); err != nil {
					return err
				}
				if tombstones != 0 {
					t.Fatal("partial series tombstones committed")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarPublicationCopiesTimesBeforeWriterWaitAndRejectsUnbound(t *testing.T) {
	_, routing, accounts, _ := newCalendarControlFixture(t)
	seedCalendarSyncEvents(t, accounts)
	snapshot, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewUserAccountStore(routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []*UserCalendarEventSnapshot{nil, {}, snapshot} {
		if err := other.PublishCalendarEvent(t.Context(), invalid, storage.CalendarPublishUpdate, calendarMutationResult(snapshot)); !errors.Is(err, storage.ErrCalendarEventChanged) {
			t.Fatal("unbound publication accepted", err)
		}
	}
	event := calendarMutationResult(snapshot)
	start, end, updated := *event.StartAt, *event.EndAt, *event.ProviderUpdatedAt
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
		tx, err := db.Write().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		before := db.Write().Stats().WaitCount
		done := make(chan error, 1)
		go func() { done <- accounts.PublishCalendarEvent(ctx, snapshot, storage.CalendarPublishUpdate, event) }()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for db.Write().Stats().WaitCount == before {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
			}
		}
		*event.StartAt, *event.EndAt, *event.ProviderUpdatedAt = start.Add(time.Hour), end.Add(time.Hour), updated.Add(time.Hour)
		if err := tx.Rollback(); err != nil {
			return err
		}
		select {
		case err := <-done:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		stored, err := db.GetCalendarEvent(ctx, "alice", "same-event")
		if err != nil {
			return err
		}
		if !stored.StartAt.Equal(start) || !stored.EndAt.Equal(end) || !stored.ProviderUpdatedAt.Equal(updated) {
			t.Fatal("caller timestamp mutation retargeted pending result")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.PublishCalendarEvent(t.Context(), snapshot, storage.CalendarEventPublicationKind(255), event); !errors.Is(err, storage.ErrCalendarEventChanged) {
		t.Fatal("invalid mode accepted", err)
	}
	if err := accounts.PublishCalendarEvent(t.Context(), snapshot, storage.CalendarPublishUpdate, event, nil); !errors.Is(err, storage.ErrCalendarEventChanged) {
		t.Fatal("nil central callback accepted", err)
	}
}

func TestCalendarMutationInvalidResultDoesNotWaitForWriter(t *testing.T) {
	_, _, accounts, _ := newCalendarControlFixture(t)
	seedCalendarSyncEvents(t, accounts)
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		existing, err := db.GetCalendarEvent(t.Context(), "alice", "same-event")
		if err != nil {
			return err
		}
		tx, err := db.Write().BeginTx(t.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		before := db.Write().Stats().WaitCount
		calls := []struct {
			name    string
			run     func(context.Context) error
			message string
		}{
			{"update", func(ctx context.Context) error {
				return db.CompleteCalendarUpdate(ctx, "alice", "same-event", "same-source", "old-version", storage.CalendarEvent{})
			}, "provider did not confirm a versioned event"},
			{"response", func(ctx context.Context) error {
				return db.CompleteCalendarResponse(ctx, existing, storage.CalendarEvent{})
			}, storage.ErrCalendarUpdateConflict.Error()},
			{"delete", func(ctx context.Context) error {
				return db.CompleteCalendarDelete(ctx, "alice", "same-event", "same-source", " ")
			}, "calendar deletion requires an event version"},
			{"series", func(ctx context.Context) error {
				return db.CompleteCalendarSeriesUpdate(ctx, "alice", "same-event", "same-source", "old-version", storage.CalendarEvent{})
			}, "provider did not confirm a versioned series master"},
		}
		for _, call := range calls {
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			err := call.run(ctx)
			cancel()
			if err == nil || err.Error() != call.message {
				t.Fatal("invalid result waited for writer or changed error", call.name, err)
			}
		}
		if db.Write().Stats().WaitCount != before {
			t.Fatal("invalid result attempted to acquire busy writer")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
