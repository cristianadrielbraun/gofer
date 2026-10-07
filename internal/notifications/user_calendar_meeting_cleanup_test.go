package notifications

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/handler"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func wakeOwnedMeetingCleanup(t *testing.T, f *userStorageFixture) {
	t.Helper()
	if r := f.request("alice", http.MethodPost, "/api/accounts/"+f.accounts["alice"].ID+"/calendar/sources", ""); r.Code != 200 {
		t.Fatal("deselection wake", r.Code, r.Body.String())
	}
}

func waitOwnedMeetingCleanup(t *testing.T, f *userStorageFixture, check func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var next int64
		err := f.system.Read().QueryRowContext(ctx, `SELECT next_due_ms FROM gofer_account_service_schedule WHERE account_id=? AND service='calendar'`, f.accounts["alice"].ID).Scan(&next)
		if err == nil && next > time.Now().Add(30*time.Minute).UnixMilli() && check() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("owned cleanup did not finish", ctx.Err(), err, next)
		case <-tick.C:
		}
	}
}

func TestUserCalendarMeetingCleanupRegisteredWorkerNativeSafetyAndPublication(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, mode := range []string{"confirmed", "rejected", "changed-source", "changed-grant", "guests"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				f, api := newOwnedMeetingPreviewFixtureServices(t, provider, "cleanup-fails", &handler.UserCalendarSyncOptions{ScanInterval: 20 * time.Millisecond, RecoveryInterval: time.Hour})
				for _, owner := range []string{"alice", "bob"} {
					if r := f.request(owner, http.MethodPost, ownedPreviewPath(provider), ownedPreviewForm()); r.Code != 200 {
						t.Fatal("native prepare", owner, r.Code, r.Body.String())
					}
				}
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					query := `UPDATE calendar_meet_drafts SET created_at=datetime('now','-2 hours')`
					if provider == "outlook" {
						query = `UPDATE calendar_teams_drafts SET updated_at=datetime('now','-2 hours')`
					}
					_, err := db.Write().Exec(query)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				api.mu.Lock()
				api.mode = "confirmed"
				api.failDelete = map[string]bool{"bob": true, "alice": mode == "rejected"}
				beforeDelete, beforeRead := api.deletes["alice"], api.reads["alice"]
				if mode == "guests" {
					api.remote["alice"]["attendees"] = []map[string]any{{"email": "guest@example.com", "emailAddress": map[string]string{"address": "guest@example.com"}}}
				}
				api.beforeDelete = func(r *http.Request) {
					if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer alice-") {
						return
					}
					if mode == "changed-source" {
						if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
							_, err := db.Write().Exec(`UPDATE calendar_sources SET remote_id='replaced' WHERE id='same-source'`)
							return err
						}); err != nil {
							t.Error(err)
						}
					}
					if mode == "changed-grant" {
						if _, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET revision=revision+1 WHERE account_id=?`, f.accounts["alice"].ID); err != nil {
							t.Error(err)
						}
					}
				}
				api.mu.Unlock()
				wakeOwnedMeetingCleanup(t, f)
				waitOwnedMeetingCleanup(t, f, func() bool {
					api.mu.Lock()
					defer api.mu.Unlock()
					if mode == "guests" {
						return api.reads["alice"] > beforeRead
					}
					return api.deletes["alice"] > beforeDelete
				})
				for _, owner := range []string{"alice", "bob"} {
					if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
						if provider == "gmail" {
							var pending bool
							var conference string
							if err := db.Read().QueryRow(`SELECT cleanup_pending,conference_json FROM calendar_meet_drafts WHERE draft_id=?`, ownedPreviewID).Scan(&pending, &conference); err != nil {
								return err
							}
							if pending != (owner != "alice" || mode != "confirmed") || !strings.Contains(conference, "private-signature") {
								return fmt.Errorf("wrong cleanup publication for %s: pending=%t conference=%s", owner, pending, conference)
							}
						} else {
							var state string
							if err := db.Read().QueryRow(`SELECT state FROM calendar_teams_drafts WHERE draft_id=?`, ownedPreviewID).Scan(&state); err != nil {
								return err
							}
							want := "active"
							if owner == "alice" {
								want = "abandoned"
								if mode == "confirmed" {
									want = "cleaned"
								}
							}
							if state != want {
								return fmt.Errorf("%s state=%s, want=%s", owner, state, want)
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				api.mu.Lock()
				aliceGone := api.remote["alice"] == nil
				if aliceGone != (mode == "confirmed" || mode == "changed-source" || mode == "changed-grant") || api.remote["bob"] == nil {
					t.Error("native resource deletion crossed scope or removed guests", aliceGone, mode)
				}
				if mode == "guests" && api.deletes["alice"] != beforeDelete {
					t.Error("unsafe guest cleanup dispatched a delete")
				}
				api.mu.Unlock()
			})
		}
	}
}

func TestUserCalendarMeetingCleanupRegisteredWorkerRootJoinAndStoreProgress(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newOwnedMeetingPreviewFixtureServices(t, provider, "cleanup-fails", &handler.UserCalendarSyncOptions{ScanInterval: 20 * time.Millisecond, RecoveryInterval: time.Hour})
			if r := f.request("alice", http.MethodPost, ownedPreviewPath(provider), ownedPreviewForm()); r.Code != 200 {
				t.Fatal(r.Code, r.Body.String())
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				query := `UPDATE calendar_meet_drafts SET created_at=datetime('now','-2 hours')`
				if provider == "outlook" {
					query = `UPDATE calendar_teams_drafts SET updated_at=datetime('now','-2 hours')`
				}
				_, err := db.Write().Exec(query)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			api.mu.Lock()
			api.mode = "confirmed"
			api.beforeDelete = func(r *http.Request) {
				if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer alice-") {
					return
				}
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					close(canceled)
				}
			}
			api.mu.Unlock()
			wakeOwnedMeetingCleanup(t, f)
			select {
			case <-entered:
			case <-time.After(8 * time.Second):
				t.Fatal("cleanup did not dispatch")
			}
			progress := make(chan error, 1)
			go func() {
				for _, owner := range []string{"alice", "bob"} {
					if r := f.request(owner, http.MethodGet, "/api/calendar/events/new", ""); r.Code != 200 {
						progress <- fmt.Errorf("cached %s form: %d", owner, r.Code)
						return
					}
				}
				if r := f.request("bob", http.MethodPost, ownedPreviewPath(provider), ownedPreviewForm()); r.Code != 200 {
					progress <- fmt.Errorf("Bob native preview: %d %s", r.Code, r.Body.String())
					return
				}
				progress <- nil
			}()
			select {
			case err := <-progress:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("native cleanup pinned store/other owner")
			}
			f.stopIMAP()
			done := make(chan struct{})
			go func() { f.imap.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(8 * time.Second):
				t.Fatal("cleanup worker did not join shutdown")
			}
			select {
			case <-canceled:
			case <-time.After(8 * time.Second):
				t.Fatal("native cleanup not canceled")
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var state string
				query := `SELECT CAST(cleanup_pending AS TEXT) FROM calendar_meet_drafts WHERE draft_id=?`
				want := "1"
				if provider == "outlook" {
					query = `SELECT state FROM calendar_teams_drafts WHERE draft_id=?`
					want = "abandoned"
				}
				if err := db.Read().QueryRow(query, ownedPreviewID).Scan(&state); err != nil {
					return err
				}
				if state != want {
					return fmt.Errorf("shutdown falsely published cleanup: %s", state)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			if api.remote["alice"] == nil {
				t.Error("canceled fixture deleted native event")
			}
			api.mu.Unlock()
		})
	}
}

func TestUserCalendarMeetingCleanupRegisteredWorkerPreservesAcceptedAndUncertainSaves(t *testing.T) {
	for _, mode := range []string{"completed", "pending"} {
		t.Run(mode, func(t *testing.T) {
			f, api, _, saves := newOwnedMeetingCreateFixtureServices(t, "outlook", "confirmed", &handler.UserCalendarSyncOptions{ScanInterval: 20 * time.Millisecond, RecoveryInterval: time.Hour})
			if r := f.request("alice", http.MethodPost, ownedPreviewPath("outlook"), ownedPreviewForm()); r.Code != 200 {
				t.Fatal("prepare", r.Code, r.Body.String())
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				query := `CREATE TRIGGER fail_cleanup_marker BEFORE UPDATE OF state ON calendar_teams_drafts WHEN NEW.state='saved' BEGIN SELECT RAISE(ABORT,'test marker failure'); END`
				if mode == "pending" {
					query = `CREATE TRIGGER fail_cleanup_marker BEFORE INSERT ON calendar_events BEGIN SELECT RAISE(ABORT,'test accepted cache failure'); END`
				}
				_, err := db.Write().Exec(query)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			form := ownedCalendarCreateForm()
			form.Set("teams_meeting", "true")
			form.Set("teams_draft_id", ownedPreviewID)
			saved := f.request("alice", http.MethodPost, "/api/calendar/events", form.Encode())
			want := 201
			if mode == "pending" {
				want = 503
			}
			if saved.Code != want || mode == "pending" && !strings.Contains(saved.Body.String(), `"uncertain":true`) {
				t.Fatal("native save", saved.Code, saved.Body.String())
			}
			// Stop ordinary selected-source sync before allowing any cache retry.
			wakeOwnedMeetingCleanup(t, f)
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				if _, err := db.Write().Exec(`DROP TRIGGER fail_cleanup_marker`); err != nil {
					return err
				}
				_, err := db.Write().Exec(`UPDATE calendar_teams_drafts SET updated_at=datetime('now','-2 hours')`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if mode == "completed" {
				if _, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET expires_at=? WHERE account_id=?`, time.Now().Add(-time.Hour), f.accounts["alice"].ID); err != nil {
					t.Fatal(err)
				}
			}
			api.mu.Lock()
			beforeReads, beforeTokens, beforeDeletes := api.reads["alice"], api.tokens["alice"], api.deletes["alice"]
			api.mu.Unlock()
			wakeOwnedMeetingCleanup(t, f)
			waitOwnedMeetingCleanup(t, f, func() bool {
				if mode == "pending" {
					api.mu.Lock()
					defer api.mu.Unlock()
					return api.reads["alice"] > beforeReads
				}
				var state string
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					return db.Read().QueryRow(`SELECT state FROM calendar_teams_drafts`).Scan(&state)
				}); err != nil {
					t.Error(err)
					return false
				}
				return state == "saved"
			})
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var state, remote, used string
				if err := db.Read().QueryRow(`SELECT state,remote_id,used_by FROM calendar_teams_drafts`).Scan(&state, &remote, &used); err != nil {
					return err
				}
				want := "saving"
				if mode == "completed" {
					want = "saved"
				}
				if state != want || remote != "temporary-native" || used != "create:"+ownedCalendarCreateID {
					return fmt.Errorf("lost accepted/uncertain original target: %s/%s/%s", state, remote, used)
				}
				var eventID, remoteID string
				if err := db.Read().QueryRow(`SELECT event_id,remote_id FROM calendar_create_requests WHERE request_id=?`, ownedCalendarCreateID).Scan(&eventID, &remoteID); err != nil {
					return err
				}
				if (eventID != "" && remoteID == "temporary-native") != (mode == "completed") {
					return fmt.Errorf("cleanup invented original completion: %s/%s", eventID, remoteID)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			if api.remote["alice"] == nil || saves["alice"] != 1 || api.deletes["alice"] != beforeDeletes {
				t.Error("cleanup deleted or resent accepted native save")
			}
			if mode == "completed" && (api.reads["alice"] != beforeReads || api.tokens["alice"] != beforeTokens) {
				t.Error("local completed recovery made native/OAuth requests", api.reads["alice"]-beforeReads, api.tokens["alice"]-beforeTokens)
			}
			api.mu.Unlock()
		})
	}
}

func TestUserCalendarMeetingCleanupRegisteredWorkerAdvancesPastFailedClaims(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newOwnedMeetingPreviewFixtureServices(t, provider, "cleanup-fails", &handler.UserCalendarSyncOptions{ScanInterval: time.Hour, RecoveryInterval: time.Hour})
			if r := f.request("alice", http.MethodPost, ownedPreviewPath(provider), ownedPreviewForm()); r.Code != 200 {
				t.Fatal(r.Code, r.Body.String())
			}
			wakeOwnedMeetingCleanup(t, f)
			waitOwnedMeetingCleanup(t, f, func() bool { return true })
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				query := `UPDATE calendar_meet_drafts SET created_at=datetime('now','-2 hours')`
				insert := `WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<7) INSERT INTO calendar_meet_drafts(user_id,source_id,draft_id,remote_id,created_at) SELECT 'alice','same-source',printf('000-unbound-%02d',i),printf('unbound-%02d',i),datetime('now','-3 hours') FROM n`
				if provider == "outlook" {
					query = `UPDATE calendar_teams_drafts SET updated_at=datetime('now','-2 hours')`
					insert = `WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<7) INSERT INTO calendar_teams_drafts(user_id,source_id,draft_id,updated_at) SELECT 'alice','same-source',printf('000-unbound-%02d',i),datetime('now','-3 hours') FROM n`
				}
				if _, err := db.Write().Exec(`DELETE FROM app_settings WHERE key=?`, "calendar_meeting_cleanup_cursor:"+f.accounts["alice"].ID); err != nil {
					return err
				}
				if _, err := db.Write().Exec(query); err != nil {
					return err
				}
				_, err := db.Write().Exec(insert)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			api.mode = "confirmed"
			beforeDelete, beforeRead := api.deletes["alice"], api.reads["alice"]
			api.mu.Unlock()
			wakeOwnedMeetingCleanup(t, f)
			waitOwnedMeetingCleanup(t, f, func() bool {
				var cursor string
				err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					return db.Read().QueryRow(`SELECT value FROM app_settings WHERE user_id='alice' AND key=?`, "calendar_meeting_cleanup_cursor:"+f.accounts["alice"].ID).Scan(&cursor)
				})
				return err == nil && strings.Contains(cursor, `"DraftID":"000-unbound-07"`)
			})
			api.mu.Lock()
			premature := api.deletes["alice"] != beforeDelete || api.reads["alice"] != beforeRead
			api.mu.Unlock()
			if premature {
				t.Fatal("unbound scheduling candidates authorized native calls")
			}
			wakeOwnedMeetingCleanup(t, f)
			waitOwnedMeetingCleanup(t, f, func() bool {
				api.mu.Lock()
				defer api.mu.Unlock()
				return api.deletes["alice"] > beforeDelete
			})
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var finished bool
				query := `SELECT cleanup_pending=0 FROM calendar_meet_drafts WHERE draft_id=?`
				if provider == "outlook" {
					query = `SELECT state='cleaned' FROM calendar_teams_drafts WHERE draft_id=?`
				}
				if err := db.Read().QueryRow(query, ownedPreviewID).Scan(&finished); err != nil {
					return err
				}
				if !finished {
					return fmt.Errorf("failed legacy claims starved native %s cleanup", provider)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
