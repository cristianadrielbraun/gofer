package notifications

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/handler"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newOwnedMeetingCreateFixture(t *testing.T, provider, mode string) (*userStorageFixture, *ownedMeetingPreviewAPI, *ownedCalendarCreateAPI, map[string]int) {
	return newOwnedMeetingCreateFixtureServices(t, provider, mode, nil)
}
func newOwnedMeetingCreateFixtureServices(t *testing.T, provider, mode string, options *handler.UserCalendarSyncOptions) (*userStorageFixture, *ownedMeetingPreviewAPI, *ownedCalendarCreateAPI, map[string]int) {
	t.Helper()
	preview := &ownedMeetingPreviewAPI{t: t, provider: provider, remote: map[string]map[string]any{}, posts: map[string]int{}, deletes: map[string]int{}}
	final := &ownedCalendarCreateAPI{t: t, provider: provider, remote: map[string]map[string]any{}, dav: map[string]string{}, etags: map[string]string{}, writes: map[string]int{}, creates: map[string]int{}, calls: map[string]int{}}
	if mode == "lost-save" && provider == "gmail" {
		final.mode = "lost-ack"
	}
	saves := map[string]int{}
	f, _, _ := newOwnedCalendarFixtureServices(t, provider, func(base *ownedCalendarDiscoveryAPI) http.Handler {
		previews, finals := preview.serve(base), final.serve(base)
		return ownedPreviewSyncHandler(preview, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
			if provider == "gmail" {
				collection := "/calendars/primary/events"
				if r.Method == "GET" && r.URL.Path == collection+"/"+strings.ReplaceAll(ownedCalendarCreateID, "-", "") {
					finals.ServeHTTP(w, r)
					return
				}
				if r.Method == "POST" && r.URL.Path == collection {
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(raw))
					var payload map[string]any
					if err := json.Unmarshal(raw, &payload); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if payload["id"] == strings.ReplaceAll(ownedCalendarCreateID, "-", "") {
						conference, _ := json.Marshal(payload["conferenceData"])
						if string(conference) != string(compactMeetingJSON(t, ownedPreviewMeet)) || strings.Contains(string(conference), "createRequest") {
							t.Error("final create replaced prepared Meet conference", string(conference))
						}
						finals.ServeHTTP(w, r)
						return
					}
				}
			} else if r.Method == "PATCH" && r.URL.Path == "/me/calendars/primary/events/temporary-native" {
				if r.Header.Get("If-Match") != `W/"temp-v1"` {
					t.Error("unconditional Teams save", r.Header.Get("If-Match"))
					w.WriteHeader(412)
					return
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				preview.mu.Lock()
				defer preview.mu.Unlock()
				if preview.remote[owner] == nil {
					t.Error("no prepared Teams event")
					w.WriteHeader(404)
					return
				}
				saves[owner]++
				if saves[owner] > 1 {
					t.Error("accepted Teams save repeated")
				}
				for key, value := range payload {
					preview.remote[owner][key] = value
				}
				preview.remote[owner]["@odata.etag"], preview.remote[owner]["changeKey"] = `W/"saved-v2"`, "saved-v2"
				if mode == "lost-save" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				_ = json.NewEncoder(w).Encode(preview.remote[owner])
				return
			}
			previews.ServeHTTP(w, r)
		}))
	}, options)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.accountStore.SetCalendarServiceEnabled(t.Context(), owner, f.accounts[owner].ID, true); err != nil {
			t.Fatal(err)
		}
		if err := f.accountStore.SetCalendarSourcesSelected(t.Context(), owner, f.accounts[owner].ID, []string{"same-source"}); err != nil {
			t.Fatal(err)
		}
	}
	return f, preview, final, saves
}
func compactMeetingJSON(t *testing.T, s string) []byte {
	t.Helper()
	var data any
	if err := json.Unmarshal([]byte(s), &data); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestUserCalendarMeetingDraftHTTPFinalCreatePreservesPreparedLinkAndRecovery(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, mode := range []string{"confirmed", "lost-save", "publication-failure"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				f, preview, final, saves := newOwnedMeetingCreateFixture(t, provider, mode)
				for _, owner := range []string{"alice", "bob"} {
					prepared := f.request(owner, "POST", ownedPreviewPath(provider), ownedPreviewForm())
					if prepared.Code != 200 {
						t.Fatal("preview", prepared.Code, prepared.Body.String())
					}
					form := ownedCalendarCreateForm()
					if provider == "gmail" {
						form.Set("google_meet_meeting", "true")
						form.Set("google_meet_draft_id", ownedPreviewID)
					} else {
						form.Set("teams_meeting", "true")
						form.Set("teams_draft_id", ownedPreviewID)
					}
					if mode == "publication-failure" {
						if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
							_, err := db.Write().Exec(`CREATE TRIGGER fail_meeting_create BEFORE INSERT ON calendar_events BEGIN SELECT RAISE(ABORT,'test cache failure'); END`)
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					result := f.request(owner, "POST", "/api/calendar/events", form.Encode())
					if mode != "confirmed" {
						expected := 502
						if mode == "publication-failure" {
							expected = 503
						}
						if result.Code != expected || !strings.Contains(result.Body.String(), `"uncertain":true`) {
							t.Fatal("unknown/cache failure", result.Code, result.Body.String())
						}
						if mode == "publication-failure" {
							if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error { _, err := db.Write().Exec(`DROP TRIGGER fail_meeting_create`); return err }); err != nil {
								t.Fatal(err)
							}
						}
						result = f.request(owner, "POST", "/api/calendar/events", form.Encode())
					}
					if result.Code != 201 || strings.Contains(result.Body.String(), `"google_meet_unconfirmed":true`) || strings.Contains(result.Body.String(), `"teams_unconfirmed":true`) {
						t.Fatal("prepared final create", result.Code, result.Body.String())
					}
					if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
						var meeting string
						if err := db.Read().QueryRow(`SELECT online_meeting_json FROM calendar_events`).Scan(&meeting); err != nil {
							return err
						}
						want := "https://meet.google.com/abc-defg-hij"
						if provider == "outlook" {
							want = ownedPreviewTeams
						}
						if !strings.Contains(meeting, want) {
							t.Fatal("prepared join link changed", meeting)
						}
						if provider == "outlook" {
							var state, used, remote string
							if err := db.Read().QueryRow(`SELECT state,used_by,remote_id FROM calendar_teams_drafts`).Scan(&state, &used, &remote); err != nil {
								return err
							}
							if state != "saved" || used != "create:"+ownedCalendarCreateID || remote != "temporary-native" {
								t.Fatal("accepted Teams target not saved", state, used, remote)
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if _, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET expires_at=datetime('now','-1 hour') WHERE account_id=?`, f.accounts[owner].ID); err != nil {
						t.Fatal(err)
					}
					if err := f.routing.DeferProviderRetry(t.Context(), owner, f.accounts[owner].ID, time.Now().Add(time.Hour)); err != nil {
						t.Fatal(err)
					}
					replay := f.request(owner, "POST", "/api/calendar/events", form.Encode())
					if replay.Code != 201 || !strings.Contains(replay.Body.String(), `"replayed":true`) {
						t.Fatal("completed meeting replay", replay.Code, replay.Body.String())
					}
					preview.mu.Lock()
					posts, saveCount := preview.posts[owner], saves[owner]
					preview.mu.Unlock()
					if posts != 1 || provider == "outlook" && saveCount != 1 {
						t.Fatal("prepared meeting recreated/resaved", posts, saveCount)
					}
					if provider == "gmail" {
						final.mu.Lock()
						creates := final.creates[owner]
						final.mu.Unlock()
						if creates != 1 {
							t.Fatal("Google final duplicate", creates)
						}
					}
				}
			})
		}
	}
}
