package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"

	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Exercise the registered, authenticated POST and real provider adapters. The
// emulator changes only the remote response; private cache publication is left
// entirely to the production handler and repositories.
type ownedResponseSubmitAPI struct {
	t           *testing.T
	provider    string
	mu          sync.Mutex
	mode        string
	writes      map[string]int
	accepted    map[string]bool
	tokens      int
	beforeWrite func(*http.Request)
}

func (a *ownedResponseSubmitAPI) wrap(base http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			owner := strings.Split(r.FormValue("refresh_token"), "-")[0]
			a.mu.Lock()
			a.tokens++
			a.mu.Unlock()
			if owner != "alice" || r.FormValue("refresh_token") != "alice-refresh" {
				a.t.Error("refresh adopted another grant", r.Form)
			}
			wantScope := mailauth.GoogleCalendarEventsScope
			if a.provider == "outlook" {
				wantScope = "https://graph.microsoft.com/Calendars.ReadWrite"
			}
			// Google refreshes the original grant without requesting scopes.
			// Microsoft explicitly narrows a service refresh to Calendar scopes.
			scope := r.FormValue("scope")
			if (a.provider == "outlook" && !strings.Contains(scope, wantScope)) || (a.provider == "gmail" && scope != "") || strings.Contains(scope, "Mail.") || strings.Contains(scope, "Contacts") {
				a.t.Error("refresh did not retain Calendar write purpose", r.FormValue("scope"))
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "alice-fresh", "refresh_token": "alice-rotated", "token_type": "Bearer", "expires_in": 3600, "scope": wantScope})
			return
		}
		owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
		if owner != "alice" && owner != "bob" {
			a.t.Error("response dispatched without an owned credential", r.Header.Get("Authorization"))
			http.Error(w, "bad owner", http.StatusUnauthorized)
			return
		}
		path := "/calendars/primary/events/invitation"
		if a.provider == "outlook" {
			path = "/me" + path
		}
		// A fresh background refresh may be requested after an acknowledged but
		// unconfirmed action. Failing that read must not permit another action.
		if r.Method == http.MethodGet && r.URL.Path == strings.TrimSuffix(path, "/invitation") {
			http.Error(w, "refresh unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodPatch || r.Method == http.MethodPost {
			a.mu.Lock()
			a.writes[owner]++
			mode := a.mode
			a.mu.Unlock()
			wantPath, wantETag := path, `"v1"`
			if a.provider == "outlook" {
				wantPath += "/accept"
				wantETag = `W/"v1"`
			}
			if r.URL.Path != wantPath || r.Header.Get("If-Match") != wantETag {
				a.t.Error("wrong conditional response target", r.Method, r.URL.Path, r.Header.Get("If-Match"))
			}
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				a.t.Error("invalid response JSON", err)
			}
			if a.provider == "gmail" {
				people, ok := payload["attendees"].([]any)
				if r.Method != http.MethodPatch || r.URL.Query().Get("sendUpdates") != "all" || payload["attendeesOmitted"] != true || !ok || len(people) != 1 || len(payload) != 2 {
					a.t.Error("response reconstructed an attendee roster", payload, r.Method, r.URL.RawQuery)
				} else if person, ok := people[0].(map[string]any); !ok || len(person) != 2 || person["email"] != owner+"@provider.test" || person["responseStatus"] != "accepted" {
					a.t.Error("response answered as another participant", people)
				}
			} else if r.Method != http.MethodPost || r.Header.Get("Prefer") != `IdType="ImmutableId"` || len(payload) != 1 || payload["sendResponse"] != true {
				a.t.Error("wrong Graph response action", payload, r.Header)
			}
			// Drain the request before blocking: net/http cannot detect a
			// disconnected HTTP/1 peer while its request body is still unread.
			_, _ = io.Copy(io.Discard, r.Body)
			if a.beforeWrite != nil {
				a.beforeWrite(r)
			}
			switch mode {
			case "own-401", "repeated-401", "refresh-lost-ack":
				if mode == "repeated-401" || strings.HasSuffix(r.Header.Get("Authorization"), "-access") {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if mode == "refresh-lost-ack" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						a.t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
			case "rejected-412":
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			case "rejected-429":
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			case "unknown-503":
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			case "lost-ack":
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					a.t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			a.mu.Lock()
			a.accepted[owner] = true
			a.mu.Unlock()
			if a.provider == "gmail" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			} else {
				w.WriteHeader(http.StatusAccepted)
			}
			return
		}
		a.mu.Lock()
		accepted, mode := a.accepted[owner], a.mode
		a.mu.Unlock()
		if accepted && mode == "readback-404" {
			http.Error(w, "event temporarily unavailable", http.StatusNotFound)
			return
		}
		rec := httptest.NewRecorder()
		base.ServeHTTP(rec, r)
		body := rec.Body.Bytes()
		if accepted && rec.Code == http.StatusOK {
			var event map[string]any
			if err := json.Unmarshal(body, &event); err != nil {
				a.t.Error(err)
			} else {
				if a.provider == "gmail" {
					event["etag"] = `"v2"`
					event["attendees"].([]any)[0].(map[string]any)["responseStatus"] = "accepted"
				} else {
					event["changeKey"], event["@odata.etag"] = `"v2"`, `W/"v2"`
					event["responseStatus"].(map[string]any)["response"] = "accepted"
					event["attendees"].([]any)[0].(map[string]any)["status"].(map[string]any)["response"] = "accepted"
				}
				body, _ = json.Marshal(event)
			}
		}
		for key, values := range rec.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(body)
	})
}

func ownedResponseSubmit(t *testing.T, f *userStorageFixture, owner, version string) *httptest.ResponseRecorder {
	t.Helper()
	return f.request(owner, http.MethodPost, "/api/calendar/events/same-event/response", url.Values{"scope": {"event"}, "response": {"accepted"}, "version": {version}}.Encode())
}

func ownedResponseState(t *testing.T, f *userStorageFixture, owner string) (storage.CalendarEvent, int) {
	t.Helper()
	var event storage.CalendarEvent
	var reservations int
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		var err error
		event, err = db.GetCalendarEvent(t.Context(), owner, "same-event")
		if err != nil {
			return err
		}
		return db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_response_requests`).Scan(&reservations)
	}); err != nil {
		t.Fatal(err)
	}
	return event, reservations
}

func TestUserCalendarResponsePostHTTPNativeProviderIsolationAndNoop(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			api := &ownedResponseSubmitAPI{t: t, provider: provider, writes: map[string]int{}, accepted: map[string]bool{}}
			f, _ := newOwnedCalendarResponseFormFixture(t, provider, nil, api.wrap)
			for _, owner := range []string{"alice", "bob"} {
				r := ownedResponseSubmit(t, f, owner, `"v1"`)
				if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"responded":true`) || !strings.Contains(r.Body.String(), `"refresh_pending":false`) {
					t.Fatal("owned response", owner, r.Code, r.Body.String())
				}
				event, n := ownedResponseState(t, f, owner)
				if event.ResponseStatus != "accepted" || event.ETag != `"v2"` || n != 1 {
					t.Fatal("private response publication", owner, event, n)
				}
				if r := ownedResponseSubmit(t, f, owner, `"v2"`); r.Code != http.StatusOK {
					t.Fatal("already answered", r.Code, r.Body.String())
				}
				api.mu.Lock()
				writes := api.writes[owner]
				api.mu.Unlock()
				if writes != 1 {
					t.Fatal("already answered sent another response", writes)
				}
			}
			for _, table := range []string{"calendar_events", "calendar_response_requests"} {
				var n int
				if err := f.system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
					t.Fatal("shared response fallback", table, n, err)
				}
			}
		})
	}
}

func TestUserCalendarResponsePostHTTPUnknownAndDefiniteRejection(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, mode := range []string{"lost-ack", "unknown-503", "readback-404", "rejected-412", "rejected-429"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				api := &ownedResponseSubmitAPI{t: t, provider: provider, mode: mode, writes: map[string]int{}, accepted: map[string]bool{}}
				f, _ := newOwnedCalendarResponseFormFixture(t, provider, nil, api.wrap)
				r := ownedResponseSubmit(t, f, "alice", `"v1"`)
				var body map[string]any
				if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
					t.Fatal(err, r.Body.String())
				}
				uncertain := !strings.HasPrefix(mode, "rejected-")
				event, reservations := ownedResponseState(t, f, "alice")
				want := 0
				if uncertain {
					want = 1
				}
				if event.ResponseStatus != "needsAction" || event.ETag != `"v1"` || reservations != want {
					t.Fatal("wrong write outcome cache/barrier", r.Code, body, event, reservations)
				}
				if mode == "readback-404" {
					if r.Code != http.StatusOK || body["pending"] != true || body["refresh_pending"] != true {
						t.Fatal("accepted action lost during readback failure", r.Code, body)
					}
				} else if r.Code != http.StatusBadGateway || body["uncertain"] != uncertain {
					t.Fatal("wrong response failure", r.Code, body)
				}
				if uncertain {
					if r := ownedResponseSubmit(t, f, "alice", `"v1"`); r.Code != http.StatusConflict {
						t.Fatal("uncertain response retried", r.Code, r.Body.String())
					}
				} else if mode == "rejected-412" {
					api.mu.Lock()
					api.mode = ""
					api.mu.Unlock()
					if r := ownedResponseSubmit(t, f, "alice", `"v1"`); r.Code != http.StatusOK {
						t.Fatal("definitely rejected response cannot be retried", r.Code, r.Body.String())
					}
				}
				api.mu.Lock()
				writes := api.writes["alice"]
				api.mu.Unlock()
				wantWrites := 1
				if mode == "rejected-412" {
					wantWrites = 2
				}
				if writes != wantWrites {
					t.Fatal(fmt.Sprintf("unexpected provider dispatches: %d != %d", writes, wantWrites))
				}
				_, otherReservations := ownedResponseState(t, f, "bob")
				if otherReservations != 0 {
					t.Fatal("Alice response reserved Bob's event")
				}
			})
		}
	}
}

func TestUserCalendarResponsePostHTTPRefreshOnlyRejectedAction(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, mode := range []string{"own-401", "repeated-401", "refresh-lost-ack"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				api := &ownedResponseSubmitAPI{t: t, provider: provider, mode: mode, writes: map[string]int{}, accepted: map[string]bool{}}
				f, _ := newOwnedCalendarResponseFormFixture(t, provider, nil, api.wrap)
				r := ownedResponseSubmit(t, f, "alice", `"v1"`)
				api.mu.Lock()
				writes, tokens := api.writes["alice"], api.tokens
				api.mu.Unlock()
				if writes != 2 || tokens != 1 {
					t.Fatal("refresh replay was not bounded to one rejected action", writes, tokens, r.Code, r.Body.String())
				}
				event, n := ownedResponseState(t, f, "alice")
				if mode == "own-401" {
					if r.Code != http.StatusOK || event.ResponseStatus != "accepted" || event.ETag != `"v2"` || n != 1 {
						t.Fatal("own refresh lost accepted response publication", r.Code, r.Body.String(), event, n)
					}
				} else {
					var body map[string]any
					_ = json.Unmarshal(r.Body.Bytes(), &body)
					want := 0
					if mode == "refresh-lost-ack" {
						want = 1
						if r := ownedResponseSubmit(t, f, "alice", `"v1"`); r.Code != http.StatusConflict {
							t.Fatal("refreshed unknown action was resent", r.Code, r.Body.String())
						}
					}
					if r.Code != http.StatusBadGateway || n != want || body["uncertain"] != (want == 1) || event.ResponseStatus != "needsAction" {
						t.Fatal("wrong refreshed action outcome", r.Code, body, n, event)
					}
				}
			})
		}
	}
}

func TestUserCalendarResponsePostHTTPRejectsAuthorityChangesAfterDispatch(t *testing.T) {
	for _, mode := range []string{"nonce", "event", "source", "grant", "reconnect-on-401"} {
		t.Run(mode, func(t *testing.T) {
			api := &ownedResponseSubmitAPI{t: t, provider: "outlook", writes: map[string]int{}, accepted: map[string]bool{}}
			if mode == "reconnect-on-401" {
				api.mode = "own-401"
			}
			f, _ := newOwnedCalendarResponseFormFixture(t, "outlook", nil, api.wrap)
			api.beforeWrite = func(r *http.Request) {
				if mode == "grant" || mode == "reconnect-on-401" {
					expires := time.Now().Add(time.Hour)
					if err := f.credentials.UpsertForUser(t.Context(), "alice", f.accounts["alice"].ID, "microsoft", "same-subject", "new-access", "new-refresh", "Bearer", &expires, userOutlookHTTPScopes+" https://graph.microsoft.com/Calendars.ReadWrite"); err != nil {
						t.Error(err)
					}
					return
				}
				query := map[string]string{
					"nonce":  `UPDATE calendar_response_requests SET claim_id='replacement'`,
					"event":  `UPDATE calendar_events SET attendees_json='[{"email":"changed@example.com"}]' WHERE id='same-event'`,
					"source": `UPDATE calendar_sources SET remote_id='replacement' WHERE id='same-source'`,
				}[mode]
				if err := f.accountStore.WithAccountForUser(t.Context(), "alice", f.accounts["alice"].ID, func(_ *config.AccountStore, db *storage.DB) error {
					_, err := db.Write().Exec(query)
					return err
				}); err != nil {
					t.Error(err)
				}
			}
			r := ownedResponseSubmit(t, f, "alice", `"v1"`)
			var body map[string]any
			_ = json.Unmarshal(r.Body.Bytes(), &body)
			event, n := ownedResponseState(t, f, "alice")
			if r.Code == http.StatusOK || event.ResponseStatus != "needsAction" || event.ETag != `"v1"` || n != 1 {
				t.Fatal("stale action published or released a replacement barrier", r.Code, body, event, n)
			}
			if body["uncertain"] != (mode != "reconnect-on-401") {
				t.Fatal("wrong dispatched uncertainty after authority change", mode, body)
			}
			api.mu.Lock()
			writes, tokens := api.writes["alice"], api.tokens
			api.mu.Unlock()
			if writes != 1 || tokens != 0 {
				t.Fatal("stale write adopted or refreshed new grant", writes, tokens)
			}
			if mode == "nonce" {
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					var nonce string
					if err := db.Read().QueryRow(`SELECT claim_id FROM calendar_response_requests`).Scan(&nonce); err != nil {
						return err
					}
					if nonce != "replacement" {
						t.Fatal("stale acknowledgement changed replacement nonce", nonce)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestUserCalendarResponsePostHTTPStrictFormsDoNotDispatch(t *testing.T) {
	api := &ownedResponseSubmitAPI{t: t, provider: "gmail", writes: map[string]int{}, accepted: map[string]bool{}}
	f, calls := newOwnedCalendarResponseFormFixture(t, "gmail", nil, api.wrap)
	valid := url.Values{"scope": {"event"}, "response": {"accepted"}, "version": {`"v1"`}}.Encode()
	for _, form := range []string{"", `{}`, "scope=event", valid + "&owner=bob", valid + "&scope=event", valid + "&response=declined", valid + "&version=v2", strings.Replace(valid, "accepted", "organizer", 1), strings.Replace(valid, "event", "series", 1), "scope=event&response=accepted&version=", "scope=event&response=accepted&version=" + strings.Repeat("x", 9000)} {
		r := f.request("alice", http.MethodPost, "/api/calendar/events/same-event/response", form)
		if r.Code != http.StatusBadRequest || r.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("invalid response form accepted", r.Code, r.Body.String())
		}
	}
	if r := f.request("alice", http.MethodPost, "/api/calendar/events/same-event/response?owner=bob", valid); r.Code != http.StatusBadRequest {
		t.Fatal("query retargeted response", r.Code, r.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("invalid response form contacted provider", calls.Load())
	}
	for _, owner := range []string{"alice", "bob"} {
		_, n := ownedResponseState(t, f, owner)
		if n != 0 {
			t.Fatal("invalid form reserved a response", owner, n)
		}
	}
}

func TestUserCalendarResponsePostHTTPBrowserDetachRootJoinAndOtherOwnerProgress(t *testing.T) {
	for _, mode := range []string{"browser-detach", "root-stop"} {
		t.Run(mode, func(t *testing.T) {
			entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			api := &ownedResponseSubmitAPI{t: t, provider: "outlook", writes: map[string]int{}, accepted: map[string]bool{}}
			api.beforeWrite = func(r *http.Request) {
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
			f, _ := newOwnedCalendarResponseFormFixture(t, "outlook", nil, api.wrap)
			browser, stopBrowser := context.WithCancel(t.Context())
			defer stopBrowser()
			form := url.Values{"scope": {"event"}, "response": {"accepted"}, "version": {`"v1"`}}.Encode()
			req := httptest.NewRequest(http.MethodPost, "/api/calendar/events/same-event/response", strings.NewReader(form)).WithContext(browser)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				r := httptest.NewRecorder()
				f.http.ServeHTTP(r, req)
				done <- r
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("native response did not dispatch")
			}
			// Same source/event IDs, MaxOpen=1, and the first owner's provider
			// write is blocked: no DB lease or global source gate may be held.
			if r := ownedResponseSubmit(t, f, "bob", `"v1"`); r.Code != http.StatusOK {
				t.Fatal("blocked response prevented other owner progress", r.Code, r.Body.String())
			}
			if mode == "browser-detach" {
				stopBrowser()
				select {
				case <-canceled:
					t.Fatal("browser disconnect canceled accepted RSVP operation")
				default:
				}
				unblock()
			} else {
				f.stopIMAP()
			}
			select {
			case r := <-done:
				if mode == "browser-detach" && r.Code != http.StatusOK {
					t.Fatal("detached response did not finish", r.Code, r.Body.String())
				}
				if mode == "root-stop" && (r.Code == http.StatusOK || !strings.Contains(r.Body.String(), `"uncertain":true`)) {
					t.Fatal("root shutdown published or discarded unknown response", r.Code, r.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("response did not join runtime cancellation")
			}
			if mode == "root-stop" {
				select {
				case <-canceled:
				case <-time.After(5 * time.Second):
					t.Fatal("root shutdown did not cancel provider HTTP")
				}
				f.imap.Wait()
			}
			event, n := ownedResponseState(t, f, "alice")
			if n != 1 || (mode == "browser-detach" && event.ResponseStatus != "accepted") || (mode == "root-stop" && event.ResponseStatus != "needsAction") {
				t.Fatal("detached/root-canceled response has wrong durable state", event, n)
			}
		})
	}
}

func TestUserCalendarResponsePostHTTPRecurringScopeAndFreshSeriesRefresh(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, scope := range []string{"occurrence", "series"} {
			t.Run(provider+"/"+scope, func(t *testing.T) {
				api := &ownedResponseSubmitAPI{t: t, provider: provider, writes: map[string]int{}, accepted: map[string]bool{}}
				var refreshes int
				var mu sync.Mutex
				// Extend the single-invitation emulator with a native parent and
				// expanded collection. Validate the original URI/validator before
				// reusing its credential and minimal attendee-payload checks.
				recurring := func(base http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/token" {
							base.ServeHTTP(w, r)
							return
						}
						collection := "/calendars/primary/events"
						listPath := collection
						if provider == "outlook" {
							collection = "/me" + collection
							listPath = "/me/calendars/primary/calendarView"
						}
						parent := strings.HasPrefix(r.URL.Path, collection+"/master")
						list := r.Method == http.MethodGet && r.URL.Path == listPath
						copy := r.Clone(r.Context())
						address := *r.URL
						copy.URL = &address
						if parent {
							copy.URL.Path = strings.Replace(copy.URL.Path, "/master", "/invitation", 1)
						}
						if r.Method != http.MethodGet {
							if parent != (scope == "series") {
								t.Error("response wrote the wrong recurrence scope", scope, r.URL.Path)
							}
							if parent {
								etag := `"master-v1"`
								if provider == "outlook" {
									etag = `W/"master-v1"`
								}
								if r.Header.Get("If-Match") != etag {
									t.Error("series response lost the parent's version", r.Header)
								}
								etag = `"v1"`
								if provider == "outlook" {
									etag = `W/"v1"`
								}
								copy.Header.Set("If-Match", etag)
							}
							base.ServeHTTP(w, copy)
							return
						}
						if list {
							mu.Lock()
							refreshes++
							mu.Unlock()
							copy.URL.Path = collection + "/invitation"
						}
						rec := httptest.NewRecorder()
						base.ServeHTTP(rec, copy)
						var event map[string]any
						if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &event) != nil {
							t.Error("recurring native provider read failed", rec.Code, rec.Body.String())
							w.WriteHeader(http.StatusBadGateway)
							return
						}
						if parent {
							event["id"] = "master"
							version := `"master-v1"`
							api.mu.Lock()
							answered := api.accepted[strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]]
							api.mu.Unlock()
							if answered {
								version = `"master-v2"`
							}
							if provider == "gmail" {
								event["recurrence"], event["etag"] = []string{"RRULE:FREQ=WEEKLY;BYDAY=MO,FR"}, version
							} else {
								event["type"], event["recurrence"] = "seriesMaster", map[string]any{"pattern": map[string]any{"type": "weekly", "interval": 1, "daysOfWeek": []string{"monday", "friday"}}}
								event["changeKey"], event["@odata.etag"] = version, "W/"+version
							}
						} else if provider == "gmail" {
							event["recurringEventId"], event["originalStartTime"] = "master", map[string]string{"dateTime": "2026-10-03T10:00:00Z"}
						} else {
							event["type"], event["seriesMasterId"] = "occurrence", "master"
						}
						var body any = event
						if list {
							key := "items"
							if provider == "outlook" {
								key = "value"
							}
							body = map[string]any{key: []any{event}}
						}
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(body)
					})
				}
				f, _ := newOwnedCalendarResponseFormFixture(t, provider, nil, api.wrap, recurring)
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE calendar_events SET series_remote_id='master' WHERE id='same-event'`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				version := `"v1"`
				if scope == "series" {
					version = `"master-v1"`
				}
				form := url.Values{"scope": {scope}, "response": {"accepted"}, "version": {version}}
				r := f.request("alice", http.MethodPost, "/api/calendar/events/same-event/response", form.Encode())
				if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"responded":true`) || !strings.Contains(r.Body.String(), `"refresh_pending":false`) {
					t.Fatal("recurring response", scope, r.Code, r.Body.String())
				}
				event, n := ownedResponseState(t, f, "alice")
				if event.SeriesRemoteID != "master" || event.RemoteID != "invitation" || event.ResponseStatus != "accepted" || event.ETag != `"v2"` || n != 1 {
					t.Fatal("recurring response changed the wrong private cache row", event, n)
				}
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					var remote, savedVersion string
					if err := db.Read().QueryRow(`SELECT remote_id,version FROM calendar_response_requests`).Scan(&remote, &savedVersion); err != nil {
						return err
					}
					want := "invitation"
					if scope == "series" {
						want = "master"
					}
					if remote != want || savedVersion != version {
						t.Fatal("recurrence reservation retargeted", remote, savedVersion)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				api.mu.Lock()
				writes := api.writes["alice"]
				api.mu.Unlock()
				mu.Lock()
				reads := refreshes
				mu.Unlock()
				if writes != 1 || (scope == "series" && reads == 0) || (scope == "occurrence" && reads != 0) {
					t.Fatal("series refresh was skipped or occurrence became a series refresh", writes, reads)
				}
			})
		}
	}
}
