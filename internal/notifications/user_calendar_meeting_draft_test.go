package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/handler"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const ownedPreviewID = "b7931511-7c6a-4ce8-8d1a-5239ba410589"
const ownedPreviewProperty = "String {534725f9-4d8e-4616-a2ea-3b020983e0c8} Name GoferTeamsDraft"
const ownedPreviewMeet = `{"conferenceId":"abc-defg-hij","signature":"private-signature","entryPoints":[{"entryPointType":"video","uri":"https://meet.google.com/abc-defg-hij"}]}`
const ownedPreviewTeams = "https://teams.microsoft.com/l/meetup-join/test/0"

type ownedMeetingPreviewAPI struct {
	t              *testing.T
	provider, mode string
	mu             sync.Mutex
	remote         map[string]map[string]any
	posts, deletes map[string]int
	tokens         map[string]int
	reads          map[string]int
	failDelete     map[string]bool
	beforePost     func(*http.Request)
	beforeDelete   func(*http.Request)
}

func (a *ownedMeetingPreviewAPI) serve(base http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			owner := strings.Split(r.FormValue("refresh_token"), "-")[0]
			a.mu.Lock()
			if a.tokens == nil {
				a.tokens = make(map[string]int)
			}
			a.tokens[owner]++
			a.mu.Unlock()
			base.ServeHTTP(w, r)
			return
		}
		owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
		if owner != "alice" && owner != "bob" {
			a.t.Error("wrong owned write credentials", r.Header.Get("Authorization"))
			w.WriteHeader(401)
			return
		}
		collection := "/calendars/primary/events"
		remoteID := storage.CalendarMeetDraftRemoteID(owner, "same-source", ownedPreviewID)
		if a.provider == "outlook" {
			collection = "/me" + collection
			remoteID = "temporary-native"
			if r.Header.Get("Prefer") != `IdType="ImmutableId"` {
				a.t.Error("unstable Graph resource identity")
			}
		}
		if r.Method == "GET" && ((a.provider == "gmail" && r.URL.Path == "/users/me/calendarList/primary") || (a.provider == "outlook" && r.URL.Path == "/me/calendars/primary")) {
			if a.provider == "gmail" {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "primary", "accessRole": "owner", "conferenceProperties": map[string]any{"allowedConferenceSolutionTypes": []string{"hangoutsMeet"}}})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "primary", "canEdit": true, "allowedOnlineMeetingProviders": []string{"teamsForBusiness"}})
			}
			return
		}
		if r.URL.Path != collection && r.URL.Path != collection+"/"+remoteID {
			a.t.Error("wrong original collection/resource", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		if r.Method == "POST" {
			if a.beforePost != nil {
				a.beforePost(r)
			}
			var payload map[string]any
			if json.NewDecoder(r.Body).Decode(&payload) != nil {
				a.t.Error("invalid native payload")
				w.WriteHeader(400)
				return
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			a.posts[owner]++
			if a.remote[owner] != nil {
				a.t.Error("duplicate native preview create")
				w.WriteHeader(409)
				return
			}
			if payload["attendees"] != nil {
				a.t.Error("temporary preview invited guests")
			}
			if a.provider == "gmail" {
				if payload["id"] != remoteID || payload["visibility"] != "private" || payload["transparency"] != "transparent" || r.URL.Query().Get("sendUpdates") != "none" || r.URL.Query().Get("conferenceDataVersion") != "1" {
					a.t.Error("unsafe temporary Google create")
				}
				payload["etag"] = `"temp-v1"`
				payload["conferenceData"] = json.RawMessage(ownedPreviewMeet)
			} else {
				if payload["transactionId"] != ownedPreviewID || payload["sensitivity"] != "private" || payload["showAs"] != "free" || payload["isReminderOn"] != false {
					a.t.Error("unsafe temporary Graph create")
				}
				payload["id"], payload["iCalUId"], payload["@odata.etag"], payload["changeKey"], payload["isOrganizer"] = "temporary-native", "private-uid", `W/"temp-v1"`, "temp-v1", true
				payload["onlineMeeting"] = map[string]string{"joinUrl": ownedPreviewTeams}
				payload["body"] = map[string]string{"contentType": "html", "content": "Join Teams"}
			}
			a.remote[owner] = payload
			if a.mode == "lost-create" && owner == "alice" {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					a.t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			_ = json.NewEncoder(w).Encode(payload)
			return
		}
		a.mu.Lock()
		beforeDelete := a.beforeDelete
		a.mu.Unlock()
		if r.Method == "DELETE" && beforeDelete != nil {
			beforeDelete(r)
			if r.Context().Err() != nil {
				return
			}
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if r.Method == "GET" {
			if a.reads == nil {
				a.reads = make(map[string]int)
			}
			a.reads[owner]++
		}
		if r.Method == "DELETE" {
			safe := a.provider == "gmail" && r.Header.Get("If-Match") == `"temp-v1"` && r.URL.Query().Get("sendUpdates") == "none"
			if a.provider == "outlook" {
				safe = r.Header.Get("If-Match") == `W/"temp-v1"` && r.URL.RawQuery == ""
			}
			if !safe {
				a.t.Error("unsafe preview cleanup")
			}
			a.deletes[owner]++
			if a.mode == "cleanup-fails" || a.failDelete[owner] {
				w.WriteHeader(503)
				return
			}
			delete(a.remote, owner)
			w.WriteHeader(204)
			return
		}
		if r.Method != "GET" {
			a.t.Error("unexpected preview method", r.Method)
			w.WriteHeader(400)
			return
		}
		if r.URL.Path == collection {
			if a.provider != "outlook" || !strings.Contains(r.URL.Query().Get("$filter"), "draft:"+ownedPreviewID) {
				a.t.Error("unscoped recovery lookup")
			}
			events := []any{}
			if a.remote[owner] != nil {
				events = append(events, a.remote[owner])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": events})
			return
		}
		if a.remote[owner] == nil {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(a.remote[owner])
	})
}
func newOwnedMeetingPreviewFixture(t *testing.T, provider, mode string) (*userStorageFixture, *ownedMeetingPreviewAPI) {
	return newOwnedMeetingPreviewFixtureServices(t, provider, mode, nil)
}
func newOwnedMeetingPreviewFixtureServices(t *testing.T, provider, mode string, options *handler.UserCalendarSyncOptions) (*userStorageFixture, *ownedMeetingPreviewAPI) {
	t.Helper()
	api := &ownedMeetingPreviewAPI{t: t, provider: provider, mode: mode, remote: map[string]map[string]any{}, posts: map[string]int{}, deletes: map[string]int{}}
	f, _, _ := newOwnedCalendarFixtureServices(t, provider, func(base *ownedCalendarDiscoveryAPI) http.Handler {
		return ownedPreviewSyncHandler(api, api.serve(base))
	}, options)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.accountStore.SetCalendarServiceEnabled(t.Context(), owner, f.accounts[owner].ID, true); err != nil {
			t.Fatal(err)
		}
		if err := f.accountStore.SetCalendarSourcesSelected(t.Context(), owner, f.accounts[owner].ID, []string{"same-source"}); err != nil {
			t.Fatal(err)
		}
	}
	return f, api
}

// The real background sync can run during preparation. Return actual native
// preview resources; the adapters must exclude their private markers themselves.
func ownedPreviewSyncHandler(api *ownedMeetingPreviewAPI, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		window := r.Method == "GET" && ((api.provider == "gmail" && r.URL.Path == "/calendars/primary/events" && r.URL.Query().Get("timeMin") != "") || (api.provider == "outlook" && r.URL.Path == "/me/calendars/primary/calendarView"))
		if !window {
			next.ServeHTTP(w, r)
			return
		}
		owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
		if owner != "alice" && owner != "bob" {
			api.t.Error("wrong native sync owner", owner)
			w.WriteHeader(401)
			return
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		events := []any{}
		if api.remote[owner] != nil {
			events = append(events, api.remote[owner])
		}
		key := "items"
		if api.provider == "outlook" {
			key = "value"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{key: events})
	})
}
func ownedPreviewPath(provider string) string {
	if provider == "gmail" {
		return "/api/calendar/google-meet/drafts"
	}
	return "/api/calendar/teams/drafts"
}
func ownedPreviewForm() string {
	return url.Values{"source_id": {"same-source"}, "draft_id": {ownedPreviewID}}.Encode()
}

func TestUserCalendarMeetingDraftHTTPNativePreparationRecoveryAndIsolation(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		modes := []string{"confirmed", "lost-create", "changed-after-create"}
		if provider == "gmail" {
			modes = append(modes, "cleanup-fails")
		}
		for _, mode := range modes {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				f, api := newOwnedMeetingPreviewFixture(t, provider, mode)
				if mode == "changed-after-create" {
					api.beforePost = func(r *http.Request) {
						if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer alice-") {
							if _, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET revision=revision+1 WHERE account_id=?`, f.accounts["alice"].ID); err != nil {
								t.Error(err)
							}
						}
					}
				}
				if mode == "lost-create" || mode == "changed-after-create" {
					r := f.request("alice", "POST", ownedPreviewPath(provider), ownedPreviewForm())
					if r.Code != 502 {
						t.Fatal("uncertain preview response", r.Code, r.Body.String())
					}
				}
				if mode != "changed-after-create" {
					for _, owner := range []string{"alice", "bob", "alice"} {
						r := f.request(owner, "POST", ownedPreviewPath(provider), ownedPreviewForm())
						join := "https://meet.google.com/abc-defg-hij"
						if provider == "outlook" {
							join = ownedPreviewTeams
						}
						if r.Code != 200 || !strings.Contains(r.Body.String(), join) || strings.Contains(r.Body.String(), "private-signature") {
							t.Fatal("native preview/recovery", owner, r.Code, r.Body.String())
						}
					}
				} else {
					r := f.request("bob", "POST", ownedPreviewPath(provider), ownedPreviewForm())
					if r.Code != 200 {
						t.Fatal("foreign owner stalled", r.Code, r.Body.String())
					}
				}
				api.mu.Lock()
				postsA, postsB := api.posts["alice"], api.posts["bob"]
				api.mu.Unlock()
				if postsA != 1 || postsB != 1 {
					t.Fatal("duplicate or cross-owner preview", postsA, postsB)
				}
				for _, owner := range []string{"alice", "bob"} {
					if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
						var cached int
						if err := db.Read().QueryRow(`SELECT count(*) FROM calendar_events`).Scan(&cached); err != nil {
							return err
						}
						if cached != 0 {
							t.Fatal("preview became visible appointment")
						}
						var conference string
						query := `SELECT conference_json FROM calendar_meet_drafts`
						if provider == "outlook" {
							query = `SELECT meeting_json FROM calendar_teams_drafts`
						}
						if err := db.Read().QueryRow(query).Scan(&conference); err != nil {
							return err
						}
						if mode == "changed-after-create" && owner == "alice" {
							if conference != "" {
								t.Fatal("stale grant published conference")
							}
						} else if conference == "" {
							t.Fatal("ready conference not retained")
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				for _, table := range []string{"calendar_create_requests", "calendar_meet_drafts", "calendar_teams_drafts"} {
					var n int
					if err := f.system.Read().QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
						t.Fatal("central fallback", table, n, err)
					}
				}
			})
		}
	}
}
func TestUserCalendarMeetingDraftHTTPDiscardAndStrictForms(t *testing.T) {
	f, api := newOwnedMeetingPreviewFixture(t, "outlook", "")
	if r := f.request("alice", "POST", "/api/calendar/teams/drafts/discard", ownedPreviewForm()); r.Code != 204 {
		t.Fatal(r.Code, r.Body.String())
	}
	if r := f.request("alice", "POST", ownedPreviewPath("outlook"), ownedPreviewForm()); r.Code != 409 {
		t.Fatal("late prepare escaped tombstone", r.Code, r.Body.String())
	}
	for _, bad := range []string{ownedPreviewForm() + "&account_id=foreign", ownedPreviewForm() + "&draft_id=" + ownedPreviewID, ownedPreviewForm() + "&event_id=foreign", strings.Repeat("x", 4097)} {
		if r := f.request("alice", "POST", ownedPreviewPath("outlook"), bad); r.Code != 400 {
			t.Fatal("bad form accepted", r.Code, r.Body.String())
		}
	}
	if r := f.request("alice", "POST", ownedPreviewPath("outlook")+"?source_id=foreign", ownedPreviewForm()); r.Code != 400 {
		t.Fatal("query accepted", r.Code, r.Body.String())
	}
	if r := f.request("bob", "POST", ownedPreviewPath("outlook"), ownedPreviewForm()); r.Code != 200 {
		t.Fatal("discard crossed owner", r.Code, r.Body.String())
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.posts["alice"] != 0 || api.posts["bob"] != 1 {
		t.Fatal("discard dispatch", api.posts)
	}
}
func TestUserCalendarMeetingDraftHTTPBrowserDetachShutdownAndProgress(t *testing.T) {
	for _, mode := range []string{"browser", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			f, api := newOwnedMeetingPreviewFixture(t, "outlook", "")
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			api.beforePost = func(r *http.Request) {
				if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer alice-") {
					close(started)
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := httptest.NewRequest("POST", ownedPreviewPath("outlook"), strings.NewReader(ownedPreviewForm())).WithContext(ctx)
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { f.http.ServeHTTP(rec, r); close(done) }()
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("native preview did not dispatch")
			}
			if other := f.request("bob", "POST", ownedPreviewPath("outlook"), ownedPreviewForm()); other.Code != 200 {
				t.Fatal("held store across native wait", other.Code, other.Body.String())
			}
			if mode == "browser" {
				cancel()
			} else {
				f.stopIMAP()
			}
			unblock()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("preview did not drain")
			}
			if mode == "browser" && rec.Code != 200 {
				t.Fatal("browser lost retained preview", rec.Code, rec.Body.String())
			}
			if mode == "shutdown" && rec.Code == 200 {
				t.Fatal("shutdown published preview")
			}
			if mode == "shutdown" {
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					var meeting string
					if err := db.Read().QueryRow(`SELECT meeting_json FROM calendar_teams_drafts`).Scan(&meeting); err != nil {
						return err
					}
					if meeting != "" {
						t.Fatal("shutdown retained conference")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
