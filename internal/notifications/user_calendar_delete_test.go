package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
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
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type ownedCalendarDeleteAPI struct {
	t                     *testing.T
	provider, scope, mode string
	mu                    sync.Mutex
	writes                map[string]int
	beforeWrite           func(*http.Request)
	bodies                map[string]string
}

func (a *ownedCalendarDeleteAPI) serve(base http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			base.ServeHTTP(w, r)
			return
		}
		owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
		path := "/calendars/primary/events/event"
		if a.provider == "outlook" {
			path = "/me" + path
		}
		if a.provider == "caldav" {
			username, password, ok := r.BasicAuth()
			owner = username
			if !ok || password != owner+"-calendar-secret" {
				a.t.Error("wrong owned DAV credentials")
				w.WriteHeader(401)
				return
			}
			path = "/users/" + owner + "/calendars/primary/event.ics"
		}
		if owner != "alice" && owner != "bob" {
			a.t.Error("unknown owner", owner)
			w.WriteHeader(401)
			return
		}
		if a.provider == "caldav" && a.scope == "occurrence" && r.Method == "REPORT" {
			collection := strings.TrimSuffix(path, "event.ics")
			body, _ := io.ReadAll(r.Body)
			if r.URL.Path != collection || !strings.Contains(string(body), "calendar-multiget") || !strings.Contains(string(body), "<c:expand start=") {
				a.t.Error("unsafe occurrence membership request", r.URL.Path, string(body))
			}
			expanded := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Gofer//Test//EN\r\nBEGIN:VEVENT\r\nUID:private-uid\r\nDTSTAMP:20261003T090000Z\r\nRECURRENCE-ID:20261003T100000Z\r\nDTSTART:20261003T100000Z\r\nDTEND:20261003T110000Z\r\nSUMMARY:Private appointment\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			var escaped bytes.Buffer
			_ = xml.EscapeText(&escaped, []byte(expanded))
			w.WriteHeader(207)
			_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>%s</d:href><d:propstat><d:prop><d:getetag>&quot;v1&quot;</d:getetag><c:calendar-data>%s</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, path, escaped.String())
			return
		}
		target := path
		if a.scope == "series" && a.provider != "caldav" {
			target = strings.TrimSuffix(path, "event") + "master"
		}
		if r.URL.Path != path && r.URL.Path != target {
			a.t.Error("wrong selected calendar resource", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		if r.Method == http.MethodDelete || r.Method == http.MethodPut {
			a.mu.Lock()
			a.writes[owner]++
			a.mu.Unlock()
			expectedVersion := `"v1"`
			if a.scope == "series" && a.provider != "caldav" {
				expectedVersion = `"master-v1"`
			}
			if a.provider == "outlook" {
				expectedVersion = "W/" + expectedVersion
			}
			if r.Header.Get("If-Match") != expectedVersion || r.URL.Path != target {
				a.t.Error("delete lost scope/version", r.URL.Path, r.Header)
			}
			requestBody, _ := io.ReadAll(r.Body)
			if a.beforeWrite != nil {
				a.beforeWrite(r)
			}
			switch a.mode {
			case "lost-ack":
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					a.t.Error(err)
					return
				}
				_ = conn.Close()
				return
			case "rejected":
				w.WriteHeader(412)
				return
			case "accepted":
				w.WriteHeader(202)
				return
			}
			if a.provider == "caldav" && a.scope == "occurrence" {
				if r.Method != http.MethodPut || !strings.Contains(string(requestBody), "EXDATE:20261003T100000Z") || !strings.Contains(string(requestBody), "RRULE:FREQ=DAILY;COUNT=4") {
					a.t.Error("occurrence delete removed or changed series", r.Method, string(requestBody))
				}
				a.mu.Lock()
				a.bodies[owner] = string(requestBody)
				a.mu.Unlock()
			}
			w.WriteHeader(204)
			return
		}
		if r.Method != http.MethodGet {
			a.t.Error("unexpected provider request", r.Method)
			w.WriteHeader(405)
			return
		}
		if a.mode == "read-failure" {
			w.WriteHeader(503)
			return
		}
		if a.provider == "caldav" {
			w.Header().Set("ETag", `"v1"`)
			recurrence := ""
			if a.scope != "event" {
				recurrence = "RRULE:FREQ=DAILY;COUNT=4\r\n"
			}
			a.mu.Lock()
			saved := a.bodies[owner]
			a.mu.Unlock()
			if saved != "" {
				w.Header().Set("ETag", `"v2"`)
				_, _ = io.WriteString(w, saved)
				return
			}
			w.Header().Set("Content-Type", "text/calendar")
			_, _ = fmt.Fprintf(w, "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Gofer//Test//EN\r\nBEGIN:VEVENT\r\nUID:private-uid\r\nDTSTAMP:20261003T090000Z\r\nDTSTART:20261003T100000Z\r\nDTEND:20261003T110000Z\r\nSUMMARY:Private appointment\r\n%sEND:VEVENT\r\nEND:VCALENDAR\r\n", recurrence)
			return
		}
		id := "event"
		version := `"v1"`
		recurring := a.scope == "series" && r.URL.Path == target
		if recurring {
			id = "master"
			version = `"master-v1"`
		}
		event := map[string]any{"id": id, "iCalUID": "private-uid", "etag": version, "summary": "Private appointment", "organizer": map[string]any{"self": true, "email": owner + "@provider.test"}, "attendees": []any{}, "start": map[string]string{"dateTime": "2026-10-03T10:00:00Z", "timeZone": "UTC"}, "end": map[string]string{"dateTime": "2026-10-03T11:00:00Z", "timeZone": "UTC"}}
		if recurring {
			event["recurrence"] = []string{"RRULE:FREQ=DAILY;COUNT=4"}
		} else if a.scope == "occurrence" {
			event["recurringEventId"] = "master"
			event["originalStartTime"] = map[string]string{"dateTime": "2026-10-03T10:00:00Z", "timeZone": "UTC"}
		}
		if a.provider == "outlook" {
			kind := "singleInstance"
			if recurring {
				kind = "seriesMaster"
			} else if a.scope == "occurrence" {
				kind = "occurrence"
			}
			event = map[string]any{"id": id, "iCalUId": "private-uid", "changeKey": version, "@odata.etag": "W/" + version, "subject": "Private appointment", "isOrganizer": true, "type": kind, "attendees": []any{}, "start": map[string]string{"dateTime": "2026-10-03T10:00:00", "timeZone": "UTC"}, "end": map[string]string{"dateTime": "2026-10-03T11:00:00", "timeZone": "UTC"}}
			if recurring {
				event["recurrence"] = map[string]any{"pattern": map[string]any{"type": "daily", "interval": 1}, "range": map[string]any{"type": "numbered", "startDate": "2026-10-03", "numberOfOccurrences": 4, "recurrenceTimeZone": "UTC"}}
			} else if a.scope == "occurrence" {
				event["seriesMasterId"] = "master"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(event)
	})
}

func newOwnedCalendarDeleteFixture(t *testing.T, provider, scope, mode string) (*userStorageFixture, *ownedCalendarDeleteAPI) {
	t.Helper()
	api := &ownedCalendarDeleteAPI{t: t, provider: provider, scope: scope, mode: mode, writes: map[string]int{}, bodies: map[string]string{}}
	f, _, server := newOwnedCalendarFixture(t, provider, func(base *ownedCalendarDiscoveryAPI) http.Handler { return api.serve(base) })
	for _, owner := range []string{"alice", "bob"} {
		if err := f.accountStore.SetCalendarServiceEnabled(t.Context(), owner, f.accounts[owner].ID, true); err != nil {
			t.Fatal(err)
		}
		if err := f.accountStore.SetCalendarSourcesSelected(t.Context(), owner, f.accounts[owner].ID, []string{"same-source"}); err != nil {
			t.Fatal(err)
		}
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			start := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
			end := start.Add(time.Hour)
			remote, parent := "event", ""
			if scope != "event" {
				parent = "master"
			}
			if provider == "caldav" {
				remote = server.URL + "/users/" + owner + "/calendars/primary/event.ics"
				if scope != "event" {
					parent = remote
					remote += "#recurrence=" + url.QueryEscape("2026-10-03T10:00:00Z")
				}
			}
			rows := []storage.CalendarEvent{{ID: "same-event", RemoteID: remote, SeriesRemoteID: parent, ICalUID: "private-uid", ETag: `"v1"`, Summary: owner + " cached", StartAt: &start, EndAt: &end}}
			if scope != "event" {
				rows = append(rows, storage.CalendarEvent{ID: "outside-event", RemoteID: "outside", SeriesRemoteID: parent, ICalUID: "private-uid", ETag: `"sibling-v1"`, Summary: owner + " outside", StartAt: &start, EndAt: &end})
			}
			return db.ReplaceCalendarEvents(t.Context(), owner, "same-source", rows, start.Add(-time.Hour), end.Add(time.Hour))
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, api
}

func ownedCalendarDeleteQuery(provider, scope string, f *userStorageFixture, owner string) string {
	query := url.Values{"version": {`"v1"`}}
	if scope != "event" {
		query.Set("scope", scope)
	}
	if scope == "series" {
		query.Set("series_id", "master")
		query.Set("version", `"master-v1"`)
		if provider == "caldav" {
			_ = f.routing.WithUser(context.Background(), owner, func(db *storage.DB) error {
				event, err := db.GetCalendarEvent(context.Background(), owner, "same-event")
				query.Set("series_id", event.SeriesRemoteID)
				return err
			})
			query.Set("version", `"v1"`)
		}
	}
	return "/api/calendar/events/same-event?" + query.Encode()
}

func ownedCalendarDeletedState(t *testing.T, f *userStorageFixture, owner, id string) bool {
	t.Helper()
	var deleted int
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT is_deleted FROM calendar_events WHERE id=?`, id).Scan(&deleted)
	}); err != nil {
		t.Fatal(err)
	}
	return deleted == 1
}

func TestUserCalendarDeleteHTTPNativeScopesAndOwnerIsolation(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		for _, scope := range []string{"event", "occurrence", "series"} {

			t.Run(provider+"/"+scope, func(t *testing.T) {
				f, api := newOwnedCalendarDeleteFixture(t, provider, scope, "")
				events := f.events.Subscribe()
				defer f.events.Unsubscribe(events)
				path := ownedCalendarDeleteQuery(provider, scope, f, "alice")
				r := f.request("alice", http.MethodDelete, path, "")
				if r.Code != 200 || !strings.Contains(r.Body.String(), `"deleted":true`) {
					t.Fatal("native delete", r.Code, r.Body.String())
				}
				if !ownedCalendarDeletedState(t, f, "alice", "same-event") || ownedCalendarDeletedState(t, f, "bob", "same-event") {
					t.Fatal("wrong owned tombstone")
				}
				if scope != "event" && ownedCalendarDeletedState(t, f, "alice", "outside-event") != (scope == "series") {
					t.Fatal("wrong recurrence cache scope")
				}
				api.mu.Lock()
				writes := api.writes["alice"]
				others := api.writes["bob"]
				api.mu.Unlock()
				if writes != 1 || others != 0 {
					t.Fatal("wrong provider writes", writes, others)
				}
				if retry := f.request("alice", http.MethodDelete, path, ""); retry.Code != 404 {
					t.Fatal("deleted event was resent", retry.Code, retry.Body.String())
				}
				found := false
				for len(events) > 0 {
					event := <-events
					if event.Type == mail.EventCalendarChanged {
						if event.UserID != "alice" || event.AccountID != f.accounts["alice"].ID {
							t.Fatal("wrong SSE owner", event)
						}
						found = true
					}
				}
				if !found {
					t.Fatal("no committed event change")
				}
				var n int
				if err := f.system.Read().QueryRow(`SELECT count(*) FROM calendar_events`).Scan(&n); err != nil || n != 0 {
					t.Fatal("shared fallback", n, err)
				}
			})
		}
	}
}

func TestUserCalendarDeleteHTTPUnknownRejectedAndPreflightFailure(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		for _, mode := range []string{"lost-ack", "rejected", "accepted", "read-failure"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				f, api := newOwnedCalendarDeleteFixture(t, provider, "event", mode)
				r := f.request("alice", http.MethodDelete, ownedCalendarDeleteQuery(provider, "event", f, "alice"), "")
				var result map[string]any
				if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
					t.Fatal(err, r.Body.String())
				}
				uncertain := mode == "lost-ack" || mode == "accepted"
				if r.Code == 200 || result["uncertain"] != uncertain || ownedCalendarDeletedState(t, f, "alice", "same-event") {
					t.Fatal("wrong failure/uncertainty", mode, r.Code, result)
				}
				api.mu.Lock()
				writes := api.writes["alice"]
				api.mu.Unlock()
				expected := 1
				if mode == "read-failure" {
					expected = 0
				}
				if writes != expected {
					t.Fatal("wrong dispatch count", writes)
				}
			})
		}
	}
}

func TestUserCalendarDeleteHTTPChangedAuthorityAndAtomicLocalFailure(t *testing.T) {
	for _, mode := range []string{"event", "source", "grant", "disabled", "local-fault"} {
		t.Run(mode, func(t *testing.T) {
			f, api := newOwnedCalendarDeleteFixture(t, "outlook", "event", "")
			if mode == "local-fault" {
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`CREATE TRIGGER deny_delete BEFORE UPDATE OF is_deleted ON calendar_events BEGIN SELECT RAISE(ABORT,'synthetic publication failure'); END`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				api.beforeWrite = func(*http.Request) {
					if mode == "disabled" {
						_, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
						if err != nil {
							t.Error(err)
						}
						return
					}
					if mode == "grant" {
						_, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET revision=revision+1 WHERE account_id=?`, f.accounts["alice"].ID)
						if err != nil {
							t.Error(err)
						}
						return
					}
					if err := f.accountStore.WithAccountForUser(t.Context(), "alice", f.accounts["alice"].ID, func(_ *config.AccountStore, db *storage.DB) error {
						query := `UPDATE calendar_events SET summary='replacement' WHERE id='same-event'`
						if mode == "source" {
							query = `UPDATE calendar_sources SET remote_id='replacement' WHERE id='same-source'`
						}
						_, err := db.Write().Exec(query)
						return err
					}); err != nil {
						t.Error(err)
					}
				}
			}
			r := f.request("alice", http.MethodDelete, ownedCalendarDeleteQuery("outlook", "event", f, "alice"), "")
			if r.Code == 200 || !strings.Contains(r.Body.String(), `"uncertain":true`) {
				t.Fatal("stale accepted result published", r.Code, r.Body.String())
			}
			if mode == "disabled" {
				if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			}
			if ownedCalendarDeletedState(t, f, "alice", "same-event") || ownedCalendarDeletedState(t, f, "bob", "same-event") {
				t.Fatal("local/foreign mutation escaped")
			}
		})
	}
}

func TestUserCalendarDeleteHTTPStrictQueriesDoNotDispatch(t *testing.T) {
	f, api := newOwnedCalendarDeleteFixture(t, "outlook", "event", "")
	for _, query := range []string{"", "version=", "version=one&version=two", "version=one&owner=bob", "version=one&scope=series", "version=one&scope=invalid", "version=one&scope=occurrence&series_id=master", "version=" + strings.Repeat("x", 2049), "%XX"} {
		r := f.request("alice", http.MethodDelete, "/api/calendar/events/same-event?"+query, "")
		if r.Code != 400 {
			t.Fatal("malformed delete", query, r.Code, r.Body.String())
		}
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.writes) != 0 {
		t.Fatal("malformed delete reached provider")
	}
}

func TestUserCalendarDeleteHTTPBrowserDetachRootJoinAndOtherOwnerProgress(t *testing.T) {
	for _, mode := range []string{"browser-detach", "root-stop"} {
		t.Run(mode, func(t *testing.T) {
			f, api := newOwnedCalendarDeleteFixture(t, "outlook", "event", "")
			entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
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
			browser, stopBrowser := context.WithCancel(t.Context())
			defer stopBrowser()
			req := httptest.NewRequest(http.MethodDelete, ownedCalendarDeleteQuery("outlook", "event", f, "alice"), nil).WithContext(browser)
			req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { rec := httptest.NewRecorder(); f.http.ServeHTTP(rec, req); done <- rec }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("delete did not dispatch")
			}
			if r := f.request("bob", http.MethodDelete, ownedCalendarDeleteQuery("outlook", "event", f, "bob"), ""); r.Code != 200 {
				t.Fatal("blocked Alice provider held Bob's store/source gate", r.Code, r.Body.String())
			}
			if mode == "browser-detach" {
				stopBrowser()
				select {
				case <-canceled:
					t.Fatal("browser cancellation stopped accepted delete")
				default:
				}
				unblock()
			} else {
				f.stopIMAP()
			}
			select {
			case r := <-done:
				if mode == "browser-detach" && r.Code != 200 {
					t.Fatal("detached delete did not finish", r.Code, r.Body.String())
				}
				if mode == "root-stop" && (r.Code == 200 || !strings.Contains(r.Body.String(), `"uncertain":true`)) {
					t.Fatal("shutdown lost unknown deletion", r.Code, r.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("delete did not join shutdown")
			}
			if mode == "root-stop" {
				select {
				case <-canceled:
				case <-time.After(5 * time.Second):
					t.Fatal("root stop did not cancel native HTTP")
				}
				f.imap.Wait()
			}
			if ownedCalendarDeletedState(t, f, "alice", "same-event") != (mode == "browser-detach") {
				t.Fatal("wrong canceled/detached cache state")
			}
		})
	}
}
