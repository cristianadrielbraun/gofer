package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserCalendarResponseReservationRealGraphSequenceAndNoAmbiguousReplay(t *testing.T) {
	for _, mode := range []string{"confirmed", "acknowledged-readback-failed", "definite-rejection-after-refresh", "reconnected-before-reservation"} {
		t.Run(mode, func(t *testing.T) {
			var gets, posts, refreshes atomic.Int32
			accepted := atomic.Bool{}
			f := ownedCalendarWorkerFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/token" {
					refreshes.Add(1)
					if err := r.ParseForm(); err != nil {
						t.Error(err)
					}
					if r.Form.Get("refresh_token") != "alice-refresh" || r.Form.Get("scope") != "https://graph.microsoft.com/Calendars.ReadWrite" {
						t.Error("foreign/non-calendar refresh", r.Form)
					}
					_, _ = io.WriteString(w, `{"access_token":"alice-fresh","refresh_token":"rotated","token_type":"Bearer","expires_in":3600,"scope":"https://graph.microsoft.com/Calendars.ReadWrite"}`)
					return
				}
				token := r.Header.Get("Authorization")
				if token != "Bearer alice-access" && token != "Bearer alice-fresh" {
					t.Error("foreign grant", token)
					http.Error(w, "wrong identity", 403)
					return
				}
				if r.Method == http.MethodPost && r.URL.Path == "/me/calendars/primary/events/kept/accept" {
					posts.Add(1)
					if r.Header.Get("If-Match") != `W/"old-version"` || r.Header.Get("Prefer") != `IdType="ImmutableId"` {
						t.Error("response action changed version/identity headers", r.Header)
					}
					payload, _ := io.ReadAll(r.Body)
					if string(payload) != `{"sendResponse":true}` {
						t.Error("response payload changed", string(payload))
					}
					if mode == "definite-rejection-after-refresh" {
						if token == "Bearer alice-access" {
							http.Error(w, "expired", 401)
						} else {
							http.Error(w, "definitely rejected", 403)
						}
						return
					}
					accepted.Store(true)
					w.WriteHeader(202)
					return
				}
				if r.Method != http.MethodGet || r.URL.Path != "/me/calendars/primary/events/kept" {
					t.Error("wrong response endpoint", r.Method, r.URL.Path)
					http.Error(w, "unexpected", 400)
					return
				}
				gets.Add(1)
				if accepted.Load() && mode == "acknowledged-readback-failed" {
					http.Error(w, "readback unavailable", 500)
					return
				}
				version, response := "old-version", "notResponded"
				if accepted.Load() {
					version, response = "new-version", "accepted"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "kept", "changeKey": version, "@odata.etag": `W/"` + version + `"`, "type": "singleInstance", "isOrganizer": false, "subject": "Invitation", "responseStatus": map[string]string{"response": response}, "organizer": map[string]any{"emailAddress": map[string]string{"address": "host@example.com"}}, "attendees": []any{map[string]any{"emailAddress": map[string]string{"address": "alice@example.com"}, "status": map[string]string{"response": response}}}, "start": map[string]string{"dateTime": "2026-10-03T07:00:00", "timeZone": "UTC"}, "end": map[string]string{"dateTime": "2026-10-03T08:00:00", "timeZone": "UTC"}})
			}), "alice", "bob")
			draft := calendarProviderDraft(t, false)
			for _, owner := range []string{"alice", "bob"} {
				if err := f.h.userAccounts.WithUser(t.Context(), owner, func(_ *config.AccountStore, db *storage.DB) error {
					if _, err := db.Write().Exec(`UPDATE calendar_sources SET access_role='owner'`); err != nil {
						return err
					}
					return db.ReplaceCalendarEvents(t.Context(), owner, "same-source", []storage.CalendarEvent{{ID: "same-event", RemoteID: "kept", ETag: "old-version", Summary: owner + " invitation", ResponseStatus: "needsAction", StartAt: draft.StartAt, EndAt: draft.EndAt}}, draft.StartAt.Add(-time.Hour), draft.EndAt.Add(time.Hour))
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
			target, err := readCalendarResponseTarget(ctx, snapshot.Source(), snapshot.Event(), "event", "wrong-caller-token")
			if err != nil {
				t.Fatal("real owned preflight", err)
			}
			target.Token = "wrong-caller-token"
			if mode == "reconnected-before-reservation" {
				wait, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				if err := f.h.userAccounts.WithUser(wait, "alice", func(_ *config.AccountStore, db *storage.DB) error {
					tx, err := db.Write().BeginTx(wait, nil)
					if err != nil {
						return err
					}
					defer tx.Rollback()
					before := db.Write().Stats().WaitCount
					done := make(chan error, 1)
					go func() { _, err := p.reserveResponse(wait, target, "accepted"); done <- err }()
					tick := time.NewTicker(time.Millisecond)
					defer tick.Stop()
					for db.Write().Stats().WaitCount == before {
						select {
						case <-wait.Done():
							return wait.Err()
						case <-tick.C:
						}
					}
					if _, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET revision=revision+1 WHERE account_id=?`, f.accounts["alice"].ID); err != nil {
						return err
					}
					if err := tx.Rollback(); err != nil {
						return err
					}
					select {
					case err := <-done:
						if !errors.Is(err, mailauth.ErrMailboxAuthorizationChanged) {
							t.Fatal("reconnected grant reserved response", err)
						}
					case <-wait.Done():
						return wait.Err()
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if posts.Load() != 0 {
					t.Fatal("provider wrote before reservation")
				}
				return
			}
			claim, err := p.reserveResponse(ctx, target, "accepted")
			if err != nil {
				t.Fatal(err)
			}
			result, sendErr := sendCalendarResponse(ctx, snapshot.Source(), snapshot.Event(), target, "accepted")
			pending := true
			if mode == "definite-rejection-after-refresh" {
				if sendErr == nil || calendarCreateUncertain(sendErr) {
					t.Fatal("definite rejection misclassified", sendErr)
				}
				if err := p.releaseResponse(ctx, claim); err != nil {
					t.Fatal("own token refresh prevented definite-rejection release", err)
				}
				pending = false
				if posts.Load() != 2 || refreshes.Load() != 1 || gets.Load() != 1 {
					t.Fatal("401/rejection sequence replayed", posts.Load(), refreshes.Load(), gets.Load())
				}
			} else {
				if sendErr != nil {
					t.Fatal(sendErr)
				}
				if (mode == "acknowledged-readback-failed") != result.Pending {
					t.Fatal("acknowledgement treated as confirmed response", result)
				}
				if posts.Load() != 1 || gets.Load() != 2 || refreshes.Load() != 0 {
					t.Fatal("ambiguous response sequence replayed", posts.Load(), gets.Load(), refreshes.Load())
				}
				if !result.Pending {
					if err := p.publishEvent(ctx, storage.CalendarPublishResponse, calendarStorageEvent("forged", "forged", result.Event)); err != nil {
						t.Fatal("confirmed response not published", err)
					}
				}
			}
			// Evict/reopen Alice. Its old-version reservation must survive even when the
			// provider accepted but readback failed; Bob's colliding identity stays empty.
			if err := f.h.userAccounts.WithUser(t.Context(), "bob", func(_ *config.AccountStore, db *storage.DB) error {
				var n int
				err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_response_requests`).Scan(&n)
				if err == nil && n != 0 {
					t.Fatal("foreign response reserved", n)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				var n int
				err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_response_requests WHERE version='old-version' AND claim_id<>''`).Scan(&n)
				want := 0
				if pending {
					want = 1
				}
				if err == nil && n != want {
					t.Fatal("uncertain response guard lost or definite failure retained", n, want)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if mode == "acknowledged-readback-failed" {
				fresh, err := f.h.userAccounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
				if err != nil {
					t.Fatal(err)
				}
				p.event = fresh
				if _, err := p.reserveResponse(ctx, target, "declined"); !errors.Is(err, storage.ErrCalendarResponsePending) {
					t.Fatal("unconfirmed response can resend after eviction", err)
				}
			}
		})
	}
}
