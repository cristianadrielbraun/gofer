package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserCalendarPublicationProviderReadPatchAndGuardedCache(t *testing.T) {
	for _, mode := range []string{"confirmed", "cooldown", "reconnected-before-writer"} {
		t.Run(mode, func(t *testing.T) {
			var gets, patches atomic.Int32
			current := map[string]any{
				"id": "kept", "changeKey": "old-version", "@odata.etag": `W/"old-version"`, "type": "singleInstance", "subject": "Before",
				"body": map[string]any{"contentType": "html", "content": ""}, "location": map[string]any{"displayName": ""},
				"start":     map[string]any{"dateTime": "2026-10-03T07:00:00", "timeZone": "UTC"},
				"end":       map[string]any{"dateTime": "2026-10-03T08:00:00", "timeZone": "UTC"},
				"attendees": []any{}, "recurrence": nil,
			}
			f := ownedCalendarWorkerFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer alice-access" || r.URL.Path != "/me/calendars/primary/events/kept" {
					t.Error("wrong owned provider identity or endpoint", r.URL.Path)
					http.Error(w, "unexpected identity", 403)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					gets.Add(1)
				} else if r.Method == http.MethodPatch {
					patches.Add(1)
					if r.Header.Get("If-Match") != `W/"old-version"` {
						t.Error("provider version was not preserved")
					}
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					for key, value := range payload {
						current[key] = value
					}
					current["changeKey"], current["@odata.etag"] = "new-version", `W/"new-version"`
				} else {
					t.Error("unexpected provider method", r.Method)
					w.WriteHeader(405)
					return
				}
				_ = json.NewEncoder(w).Encode(current)
			}), "alice", "bob")
			draft := calendarProviderDraft(t, false)
			for _, owner := range []string{"alice", "bob"} {
				if err := f.h.userAccounts.WithUser(t.Context(), owner, func(_ *config.AccountStore, db *storage.DB) error {
					return db.ReplaceCalendarEvents(t.Context(), owner, "same-source", []storage.CalendarEvent{{ID: "same-event", RemoteID: "kept", ETag: "old-version", Summary: owner + " cached", StartAt: draft.StartAt, EndAt: draft.EndAt}}, draft.StartAt.Add(-time.Hour), draft.EndAt.Add(time.Hour))
				}); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := f.h.userAccounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
			if err != nil {
				t.Fatal(err)
			}
			p := &userCalendarRequest{h: f.h, event: snapshot}
			ctx, err := p.actionContext(t.Context(), true)
			if err != nil {
				t.Fatal(err)
			}
			remote, err := updateOutlookCalendarEvent(ctx, "wrong-caller-token", "primary", snapshot.Event(), draft)
			if err != nil {
				t.Fatal("real provider update adapter", err)
			}
			if gets.Load() != 1 || patches.Load() != 1 {
				t.Fatal("provider sequence replayed", gets.Load(), patches.Load())
			}
			result := calendarStorageEvent("forged-owner", "forged-source", remote)
			if mode == "cooldown" {
				if err := f.h.userStorage.DeferProviderRetry(ctx, "alice", f.accounts["alice"].ID, time.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "reconnected-before-writer" {
				wait, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				err = f.h.userAccounts.WithUser(wait, "alice", func(_ *config.AccountStore, db *storage.DB) error {
					tx, err := db.Write().BeginTx(wait, nil)
					if err != nil {
						return err
					}
					defer tx.Rollback()
					before := db.Write().Stats().WaitCount
					done := make(chan error, 1)
					go func() { done <- p.publishEvent(wait, storage.CalendarPublishUpdate, result) }()
					tick := time.NewTicker(time.Millisecond)
					defer tick.Stop()
					for db.Write().Stats().WaitCount == before {
						select {
						case <-wait.Done():
							return wait.Err()
						case <-tick.C:
						}
					}
					// Central revision replacement models a reconnect after the
					// provider accepted the PATCH but before local publication.
					if _, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET revision=revision+1 WHERE account_id=?`, f.accounts["alice"].ID); err != nil {
						return err
					}
					if err := tx.Rollback(); err != nil {
						return err
					}
					select {
					case err := <-done:
						if !errors.Is(err, mailauth.ErrMailboxAuthorizationChanged) {
							t.Fatal("late write grant replaced", err)
						}
					case <-wait.Done():
						return wait.Err()
					}
					return nil
				})
			} else {
				err = p.publishEvent(ctx, storage.CalendarPublishUpdate, result)
			}
			if err != nil {
				t.Fatal("guarded cache publication", err)
			}
			for _, owner := range []string{"alice", "bob"} {
				if err := f.h.userAccounts.WithUser(t.Context(), owner, func(_ *config.AccountStore, db *storage.DB) error {
					event, err := db.GetCalendarEvent(t.Context(), owner, "same-event")
					if err != nil {
						return err
					}
					if owner == "bob" || mode == "reconnected-before-writer" {
						if event.Summary != owner+" cached" || event.ETag != "old-version" {
							t.Fatal("late/foreign cache result published", owner, event)
						}
					} else if event.Summary != draft.Summary || event.ETag != "new-version" || event.UserID != "alice" || event.SourceID != "same-source" {
						t.Fatal("accepted result lost or retargeted", event)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if gets.Load() != 1 || patches.Load() != 1 {
				t.Fatal("cache publication retried provider", gets.Load(), patches.Load())
			}
		})
	}
}
