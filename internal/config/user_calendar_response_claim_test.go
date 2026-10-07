package config

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserCalendarResponseClaimPublicationNonceAndEviction(t *testing.T) {
	for _, change := range []string{"none", "replacement", "trigger-replacement", "wrong-response", "series"} {
		t.Run(change, func(t *testing.T) {
			_, _, accounts, _ := newCalendarControlFixture(t)
			seedCalendarSyncEvents(t, accounts)
			if change == "series" {
				calendarMutationSeriesFixture(t, accounts)
			}
			snapshot, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
			if err != nil {
				t.Fatal(err)
			}
			scope, version := "event", snapshot.Event().ETag
			if change == "series" {
				scope, version = "series", "master-version"
			}
			claim, err := accounts.ReserveCalendarResponse(t.Context(), snapshot, scope, version, "accepted")
			if err != nil {
				t.Fatal(err)
			}
			if claim.Event() != snapshot {
				t.Fatal("claim did not retain its original private event")
			}
			// MaxOpen=1: accessing Bob evicts Alice and forces validation through
			// the persisted reservation rather than an in-memory object alone.
			if err := accounts.WithUser(t.Context(), "bob", func(*AccountStore, *storage.DB) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err := accounts.ValidateCalendarResponse(t.Context(), claim); err != nil {
				t.Fatal("claim did not survive store eviction", err)
			}
			if change == "replacement" || change == "trigger-replacement" {
				query := `UPDATE calendar_response_requests SET claim_id='replacement'`
				if change == "trigger-replacement" {
					query = `CREATE TRIGGER replace_response AFTER UPDATE ON calendar_events BEGIN UPDATE calendar_response_requests SET claim_id='replacement'; END`
				}
				if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error { _, err := db.Write().Exec(query); return err }); err != nil {
					t.Fatal(err)
				}
			}
			result := calendarMutationResult(snapshot)
			result.ResponseStatus = "accepted"
			if change == "wrong-response" {
				result.ResponseStatus = "declined"
			}
			err = accounts.PublishCalendarResponse(t.Context(), claim, result)
			if change == "none" {
				if err != nil {
					t.Fatal("confirmed response could not publish", err)
				}
			} else if !errors.Is(err, storage.ErrCalendarEventChanged) {
				t.Fatal("wrong or superseded response published", change, err)
			}
			if change != "none" && change != "replacement" {
				if err := accounts.ValidateCalendarResponse(t.Context(), claim); err != nil {
					t.Fatal("failed publication did not roll back event and nonce", err)
				}
			}
			if change == "replacement" {
				if err := accounts.ValidateCalendarResponse(t.Context(), claim); !errors.Is(err, storage.ErrCalendarEventChanged) {
					t.Fatal("stale nonce authorized dispatch", err)
				}
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				event, err := db.GetCalendarEvent(t.Context(), "alice", "same-event")
				if err == nil && change == "none" && (event.ResponseStatus != "accepted" || event.ETag != result.ETag || event.UserID != "alice" || event.SourceID != "same-source") {
					t.Fatal("response used provider-controlled local identity", event)
				}
				if err == nil && change != "none" && event.ETag != snapshot.Event().ETag {
					t.Fatal("failed nonce publication changed cache", event)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarResponseClaimWriterWaitRejectsReplacedNonce(t *testing.T) {
	_, _, accounts, _ := newCalendarControlFixture(t)
	seedCalendarSyncEvents(t, accounts)
	snapshot, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := accounts.ReserveCalendarResponse(t.Context(), snapshot, "event", snapshot.Event().ETag, "accepted")
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
		done := make(chan error, 1)
		go func() {
			result := calendarMutationResult(snapshot)
			result.ResponseStatus = "accepted"
			done <- accounts.PublishCalendarResponse(ctx, claim, result)
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
		if _, err := tx.Exec(`UPDATE calendar_response_requests SET claim_id='replacement'`); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		select {
		case err := <-done:
			if !errors.Is(err, storage.ErrCalendarEventChanged) {
				t.Fatal("writer wait adopted replacement nonce", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		var summary, nonce string
		if err := db.Read().QueryRow(`SELECT e.summary,r.claim_id FROM calendar_events e JOIN calendar_response_requests r ON r.source_id=e.source_id AND r.remote_id=e.remote_id WHERE e.id='same-event'`).Scan(&summary, &nonce); err != nil {
			return err
		}
		if summary != snapshot.Event().Summary || nonce != "replacement" {
			t.Fatal("failed publication mutated original cache or replacement barrier", summary, nonce)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserCalendarResponseClaimReleaseKeepsDurableReply(t *testing.T) {
	f := newOwnedReplyFixture(t)
	_ = f.queue(t, "alice")
	if err := f.accounts.ReleaseCalendarResponse(t.Context(), f.claims["alice"]); !errors.Is(err, storage.ErrCalendarResponsePending) {
		t.Fatal("queued reply reservation released", err)
	}
	if err := f.accounts.ValidateCalendarResponse(t.Context(), f.claims["alice"]); err != nil {
		t.Fatal("queued reply lost nonce", err)
	}
	if _, err := f.accounts.ReserveCalendarResponse(t.Context(), f.claims["alice"].Event(), "event", f.claims["alice"].Event().Event().ETag, "declined"); !errors.Is(err, storage.ErrCalendarResponsePending) {
		t.Fatal("queued reply no longer guards duplicate response", err)
	}
}
