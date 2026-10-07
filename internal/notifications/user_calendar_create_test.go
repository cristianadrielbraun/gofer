package notifications

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const ownedCalendarCreateID = "cc77e9f1-81da-4fe3-9566-cd3b7e01700c"

func ownedCalendarCreateForm() url.Values {
	return url.Values{"request_id": {ownedCalendarCreateID}, "source_id": {"same-source"}, "summary": {"Planning"}, "start_date": {"2026-10-07"}, "end_date": {"2026-10-07"}, "start_time": {"10:00"}, "end_time": {"11:00"}, "timezone": {"UTC"}, "description": {"Full notes"}, "location": {"Room 2"}}
}

type ownedCalendarCreateAPI struct {
	t                      *testing.T
	provider, mode         string
	mu                     sync.Mutex
	remote                 map[string]map[string]any
	dav                    map[string]string
	etags                  map[string]string
	writes, creates, calls map[string]int
	revisions              map[string]int
	afterWrite             func(*http.Request)
	beforeWrite            func(*http.Request)
}

func (a *ownedCalendarCreateAPI) serve(base http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			base.ServeHTTP(w, r)
			return
		}
		owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
		collection := "/calendars/primary/events"
		id := strings.ReplaceAll(ownedCalendarCreateID, "-", "")
		if a.provider == "outlook" {
			collection = "/me" + collection
			id = "created-event"
			if r.Header.Get("Prefer") != `IdType="ImmutableId"` {
				a.t.Error("unstable Graph ID")
			}
		}
		if a.provider == "caldav" {
			user, password, ok := r.BasicAuth()
			owner = user
			if !ok || password != owner+"-calendar-secret" {
				a.t.Error("wrong DAV credentials")
				w.WriteHeader(401)
				return
			}
			collection = "/users/" + owner + "/calendars/primary"
			id = ownedCalendarCreateID + ".ics"
		}
		if owner != "alice" && owner != "bob" {
			a.t.Error("unknown create owner", owner)
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != collection && r.URL.Path != collection+"/"+id {
			a.t.Error("wrong create source", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		write := r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH"
		update := r.Method == "PATCH" || r.Method == "PUT" && r.Header.Get("If-Match") != ""
		a.mu.Lock()
		beforeWrite := a.beforeWrite
		a.mu.Unlock()
		if write && beforeWrite != nil {
			beforeWrite(r)
			if r.Context().Err() != nil {
				return
			}
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.revisions == nil {
			a.revisions = make(map[string]int)
		}
		a.calls[owner]++
		if write {
			a.writes[owner]++
			if a.mode == "rejected" {
				w.WriteHeader(403)
				return
			}
			if update {
				expected := a.etags[owner]
				if a.provider == "gmail" {
					expected, _ = a.remote[owner]["etag"].(string)
				}
				if a.provider == "outlook" {
					expected, _ = a.remote[owner]["@odata.etag"].(string)
				}
				if r.Header.Get("If-Match") != expected {
					a.t.Error("wrong conditional update", r.Header.Get("If-Match"))
					w.WriteHeader(412)
					return
				}
				a.revisions[owner]++
				version := fmt.Sprintf("saved-v%d", a.revisions[owner])
				if a.provider == "caldav" {
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						a.t.Error(err)
						w.WriteHeader(400)
						return
					}
					a.dav[owner] = string(raw)
					a.etags[owner] = `"` + version + `"`
				} else {
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						a.t.Error(err)
						w.WriteHeader(400)
						return
					}
					for key, value := range payload {
						a.remote[owner][key] = value
					}
					if a.provider == "gmail" {
						a.remote[owner]["etag"] = `"` + version + `"`
					} else {
						a.remote[owner]["@odata.etag"] = `W/"` + version + `"`
						a.remote[owner]["changeKey"] = version
					}
				}
			} else if a.provider == "caldav" {
				if r.Method != "PUT" || r.Header.Get("If-None-Match") != "*" {
					a.t.Error("unconditional DAV create")
				}
				if a.dav[owner] != "" {
					w.WriteHeader(412)
					return
				}
				raw, _ := io.ReadAll(r.Body)
				a.dav[owner] = string(raw)
				a.etags[owner] = `"created-v1"`
				a.creates[owner]++
				a.revisions[owner] = 1
			} else {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					a.t.Error(err)
					w.WriteHeader(400)
					return
				}
				if a.provider == "gmail" {
					if payload["id"] != id {
						a.t.Error("Google create identity changed")
					}
					if a.remote[owner] != nil {
						w.WriteHeader(409)
						return
					}
					payload["etag"], payload["iCalUID"] = `"created-v1"`, "private-uid"
					payload["organizer"] = map[string]any{"email": owner + "@provider.test", "self": true}
				} else {
					if payload["transactionId"] != ownedCalendarCreateID {
						a.t.Error("Graph transaction changed")
					}
					payload["id"], payload["changeKey"], payload["@odata.etag"], payload["iCalUId"], payload["type"], payload["isOrganizer"] = "created-event", "created-v1", `W/"created-v1"`, "private-uid", "singleInstance", true
					payload["organizer"] = map[string]any{"emailAddress": map[string]any{"address": owner + "@provider.test", "name": owner}}
				}
				if a.remote[owner] == nil {
					a.remote[owner] = payload
					a.creates[owner]++
					a.revisions[owner] = 1
				}
			}
			if a.afterWrite != nil {
				a.afterWrite(r)
			}
			if a.mode == "lost-ack" && a.writes[owner] == 1 {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					a.t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
		} else if r.Method != "GET" {
			a.t.Error("unexpected create method", r.Method)
			w.WriteHeader(400)
			return
		}
		if a.provider == "caldav" {
			if a.dav[owner] == "" {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("ETag", a.etags[owner])
			if update {
				w.WriteHeader(204)
			} else if write {
				w.WriteHeader(201)
			} else {
				_, _ = io.WriteString(w, a.dav[owner])
			}
			return
		}
		if a.remote[owner] == nil {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(a.remote[owner])
	})
}
func newOwnedCalendarCreateFixture(t *testing.T, provider, mode string) (*userStorageFixture, *ownedCalendarCreateAPI, *ownedCalendarDiscoveryAPI) {
	t.Helper()
	api := &ownedCalendarCreateAPI{t: t, provider: provider, mode: mode, remote: map[string]map[string]any{}, dav: map[string]string{}, etags: map[string]string{}, writes: map[string]int{}, creates: map[string]int{}, calls: map[string]int{}, revisions: map[string]int{}}
	f, base, _ := newOwnedCalendarFixture(t, provider, func(base *ownedCalendarDiscoveryAPI) http.Handler { return api.serve(base) })
	for _, owner := range []string{"alice", "bob"} {
		if err := f.accountStore.SetCalendarServiceEnabled(t.Context(), owner, f.accounts[owner].ID, true); err != nil {
			t.Fatal(err)
		}
		if err := f.accountStore.SetCalendarSourcesSelected(t.Context(), owner, f.accounts[owner].ID, []string{"same-source"}); err != nil {
			t.Fatal(err)
		}
	}
	return f, api, base
}
func TestUserCalendarCreateHTTPNativeRecoveryReplayAndOwnerIsolation(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		for _, mode := range []string{"confirmed", "lost-ack"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				f, api, token := newOwnedCalendarCreateFixture(t, provider, mode)
				events := f.events.Subscribe()
				defer f.events.Unsubscribe(events)
				for _, owner := range []string{"alice", "bob"} {
					form := ownedCalendarCreateForm()
					form.Set("summary", owner+" planning")
					r := f.request(owner, "POST", "/api/calendar/events", form.Encode())
					if mode == "lost-ack" {
						if r.Code != 502 || !strings.Contains(r.Body.String(), `"uncertain":true`) {
							t.Fatal("lost write response", r.Code, r.Body.String())
						}
						r = f.request(owner, "POST", "/api/calendar/events", form.Encode())
					}
					var result map[string]any
					if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
						t.Fatal(err, r.Body.String())
					}
					if r.Code != 201 || result["replayed"] != false || result["event_id"] == "" {
						t.Fatal("native create", r.Code, result)
					}
					eventID, _ := result["event_id"].(string)
					if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
						event, err := db.GetCalendarEvent(t.Context(), owner, eventID)
						if err != nil {
							return err
						}
						if event.Summary != owner+" planning" {
							return fmt.Errorf("wrong copied owner event: %s", event.Summary)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					api.mu.Lock()
					before := api.calls[owner]
					api.mu.Unlock()
					// Time naturally expires a token without changing the authorization grant.
					// Local durable replay is also permitted during a provider cooldown.
					if _, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET expires_at=datetime('now','-1 hour') WHERE account_id=?`, f.accounts[owner].ID); err != nil {
						t.Fatal(err)
					}
					if err := f.routing.DeferProviderRetry(t.Context(), owner, f.accounts[owner].ID, time.Now().Add(time.Hour)); err != nil {
						t.Fatal(err)
					}
					replay := f.request(owner, "POST", "/api/calendar/events", form.Encode())
					if replay.Code != 201 || !strings.Contains(replay.Body.String(), `"replayed":true`) || !strings.Contains(replay.Body.String(), eventID) {
						t.Fatal("local completed replay", replay.Code, replay.Body.String())
					}
					api.mu.Lock()
					calls, creates := api.calls[owner], api.creates[owner]
					api.mu.Unlock()
					if calls != before || creates != 1 || token.count(owner, "token") != 0 {
						t.Fatal("completed replay contacted provider or duplicated event", calls, before, creates, token.count(owner, "token"))
					}
				}
				changed := map[string]int{}
				for len(events) > 0 {
					event := <-events
					if event.Type == mail.EventCalendarChanged {
						if event.AccountID != f.accounts[event.UserID].ID {
							t.Fatal("wrong event account", event)
						}
						changed[event.UserID]++
					}
				}
				if changed["alice"] != 1 || changed["bob"] != 1 {
					t.Fatal("wrong event publication", changed)
				}
				for _, table := range []string{"calendar_events", "calendar_create_requests"} {
					var n int
					if err := f.system.Read().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
						t.Fatal("shared create fallback", table, n, err)
					}
				}
			})
		}
	}
}
func TestUserCalendarCreateHTTPRejectsFormsAndChangedAuthority(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		for _, mode := range []string{"forms", "rejected", "changed-after-write", "publication-failure"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				f, api, _ := newOwnedCalendarCreateFixture(t, provider, mode)
				form := ownedCalendarCreateForm()
				if mode == "forms" {
					for _, body := range []string{form.Encode() + "&owner=bob", form.Encode() + "&request_id=" + ownedCalendarCreateID, strings.Repeat("x", 384<<10+1)} {
						if r := f.request("alice", "POST", "/api/calendar/events", body); r.Code != 400 {
							t.Fatal("invalid create accepted", r.Code, r.Body.String())
						}
					}
					if r := f.request("alice", "POST", "/api/calendar/events?source_id=other", form.Encode()); r.Code != 400 {
						t.Fatal("query override accepted", r.Code)
					}
					if r := f.request("", "POST", "/api/calendar/events", form.Encode()); r.Code != 401 {
						t.Fatal("unauthenticated create", r.Code)
					}
					api.mu.Lock()
					calls := api.calls["alice"]
					api.mu.Unlock()
					if calls != 0 {
						t.Fatal("invalid form reached provider")
					}
					return
				}
				if mode == "publication-failure" {
					if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
						_, err := db.Write().Exec(`CREATE TRIGGER fail_owned_create BEFORE INSERT ON calendar_events BEGIN SELECT RAISE(ABORT,'test publication failure'); END`)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "changed-after-write" {
					api.afterWrite = func(_ *http.Request) {
						if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
							_, err := db.Write().Exec(`UPDATE calendar_sources SET remote_id='replaced' WHERE id='same-source'`)
							return err
						}); err != nil {
							t.Error(err)
						}
					}
				}
				r := f.request("alice", "POST", "/api/calendar/events", form.Encode())
				expected, uncertain := 502, true
				if mode == "rejected" {
					uncertain = false
				}
				if mode == "publication-failure" {
					expected = 503
				}
				if r.Code != expected || !strings.Contains(r.Body.String(), fmt.Sprintf(`"uncertain":%t`, uncertain)) {
					t.Fatal("wrong create failure", r.Code, r.Body.String())
				}
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					var n int
					if err := db.Read().QueryRow(`SELECT count(*) FROM calendar_events`).Scan(&n); err != nil {
						return err
					}
					if n != 0 {
						t.Fatal("unconfirmed event cached")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				// Failed cache publication remains recoverable on the original request ID.
				if mode == "publication-failure" {
					if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error { _, err := db.Write().Exec(`DROP TRIGGER fail_owned_create`); return err }); err != nil {
						t.Fatal(err)
					}
					r = f.request("alice", "POST", "/api/calendar/events", form.Encode())
					if r.Code != 201 {
						t.Fatal("cache recovery", r.Code, r.Body.String())
					}
					api.mu.Lock()
					creates := api.creates["alice"]
					api.mu.Unlock()
					if creates != 1 {
						t.Fatal("cache recovery duplicated native event", creates)
					}
				}
			})
		}
	}
}
