package notifications

import (
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/handler"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserCalendarSyncHTTPConfiguredWorkerWakeAndLifetime(t *testing.T) {
	f, api := newOwnedCalendarSyncFixtureServices(t, "outlook", &handler.UserCalendarSyncOptions{ScanInterval: 20 * time.Millisecond, RecoveryInterval: time.Hour})
	// Real route publication wakes the already registered dispatcher; sources
	// were seeded after its initial startup pass without a direct handler hook.
	r := f.request("alice", "POST", "/api/accounts/"+f.accounts["alice"].ID+"/calendar/sources", "source_id=same-source")
	if r.Code != 200 {
		t.Fatal("source selection wake", r.Code, r.Body.String())
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		ready := false
		if err := f.routing.WithUser(ctx, "alice", func(db *storage.DB) error {
			event, err := db.GetCalendarEvent(ctx, "alice", "same-event")
			if err == nil {
				ready = strings.Contains(event.Summary, "refreshed")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("registered worker not reached", ctx.Err())
		case <-tick.C:
		}
	}
	api.mu.Lock()
	api.blockOwner = "alice"
	api.mu.Unlock()
	r = f.request("alice", "POST", "/api/accounts/"+f.accounts["alice"].ID+"/calendar/sources", "source_id=same-source")
	if r.Code != 200 {
		t.Fatal(r.Code)
	}
	select {
	case <-api.entered:
	case <-ctx.Done():
		t.Fatal("configuration wake ignored future local deadline", ctx.Err())
	}
	// Root shutdown must join this worker even though the handler's context is live.
	f.stopIMAP()
	done := make(chan struct{})
	go func() { f.imap.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("configured dispatcher/provider not joined", ctx.Err())
	}
}

type ownedCalendarSyncAPI struct {
	base                                               *ownedCalendarDiscoveryAPI
	mu                                                 sync.Mutex
	calls                                              map[string]int
	blockPath, blockOwner, next, redirect, foreignHref string
	rejectOld, throttle, failSecond                    bool
	entered, release                                   chan struct{}
	enteredOnce, releaseOnce                           sync.Once
}

func (a *ownedCalendarSyncAPI) unblock() { a.releaseOnce.Do(func() { close(a.release) }) }
func (a *ownedCalendarSyncAPI) count(owner string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[owner]
}
func (a *ownedCalendarSyncAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		a.base.ServeHTTP(w, r)
		return
	}
	owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
	if a.base.provider == "caldav" {
		username, password, ok := r.BasicAuth()
		if !ok || password != username+"-calendar-secret" || r.Method != "REPORT" {
			http.Error(w, "wrong calendar credentials", 401)
			return
		}
		owner = username
	} else if r.Method != "GET" {
		http.Error(w, "wrong method", 400)
		return
	}
	if owner != "alice" && owner != "bob" {
		http.Error(w, "wrong owner", 401)
		return
	}
	second := r.URL.Query().Get("pageToken") == "two" || r.URL.Query().Get("$skiptoken") == "two"
	body, _ := io.ReadAll(r.Body)
	metadata := strings.Contains(string(body), "calendar-multiget")
	a.mu.Lock()
	a.calls[owner]++
	block := a.blockOwner == owner && (a.blockPath == "" || a.blockPath == r.URL.Path)
	if a.blockPath == "metadata" {
		block = a.blockOwner == owner && metadata
	}
	reject, throttle, fail, next, redirect, foreign := a.rejectOld, a.throttle, a.failSecond, a.next, a.redirect, a.foreignHref
	a.mu.Unlock()
	if block {
		a.enteredOnce.Do(func() { close(a.entered) })
		select {
		case <-a.release:
		case <-r.Context().Done():
			return
		}
	}
	if reject && strings.HasSuffix(r.Header.Get("Authorization"), "-access") {
		http.Error(w, "expired", 401)
		return
	}
	if throttle {
		w.Header().Set("Retry-After", "120")
		http.Error(w, "throttled", 429)
		return
	}
	if redirect != "" {
		http.Redirect(w, r, redirect, 302)
		return
	}
	if (second || metadata) && fail {
		http.Error(w, "partial calendar failure", 500)
		return
	}
	if a.base.provider == "caldav" {
		ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Gofer//Test//EN\r\nBEGIN:VEVENT\r\nUID:private-uid\r\nDTSTART:20261003T100000Z\r\nDTEND:20261003T110000Z\r\nSUMMARY:" + owner + " private refreshed appointment\r\nDESCRIPTION:Full notes\r\nORGANIZER:mailto:" + owner + "@example.com\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:guest@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		href := r.URL.Path + "event.ics"
		if foreign != "" {
			href = foreign
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(207)
		_, _ = fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>`)
		_ = xml.EscapeText(w, []byte(href))
		_, _ = fmt.Fprint(w, `</d:href><d:propstat><d:prop><d:getetag>"new-version"</d:getetag><c:calendar-data>`)
		_ = xml.EscapeText(w, []byte(ics))
		_, _ = fmt.Fprint(w, `</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	id := "kept"
	if second {
		id = "new"
	}
	if a.base.provider == "gmail" {
		event := map[string]any{"id": id, "iCalUID": "private-uid", "etag": "new-version", "status": "confirmed", "summary": owner + " private refreshed appointment", "description": "Full notes", "start": map[string]string{"dateTime": "2026-10-03T10:00:00Z", "timeZone": "UTC"}, "end": map[string]string{"dateTime": "2026-10-03T11:00:00Z", "timeZone": "UTC"}, "organizer": map[string]string{"email": owner + "@provider.test"}, "attendees": []any{map[string]string{"email": "guest@example.com", "responseStatus": "accepted"}}, "hangoutLink": "https://meet.google.com/abc-defg-hij"}
		page := map[string]any{"items": []any{event}}
		if !second {
			page["nextPageToken"] = "two"
		}
		_ = json.NewEncoder(w).Encode(page)
		return
	}
	event := map[string]any{"id": id, "iCalUId": "private-uid", "changeKey": "new-version", "subject": owner + " private refreshed appointment", "body": map[string]string{"contentType": "text", "content": "Full notes"}, "start": map[string]string{"dateTime": "2026-10-03T10:00:00", "timeZone": "UTC"}, "end": map[string]string{"dateTime": "2026-10-03T11:00:00", "timeZone": "UTC"}, "organizer": map[string]any{"emailAddress": map[string]string{"address": owner + "@provider.test"}}, "attendees": []any{map[string]any{"emailAddress": map[string]string{"address": "guest@example.com"}, "status": map[string]string{"response": "accepted"}}}, "onlineMeeting": map[string]string{"joinUrl": "https://teams.microsoft.com/l/meetup-join/private"}}
	page := map[string]any{"value": []any{event}}
	if !second {
		if next == "" {
			next = "https://graph.microsoft.com/v1.0/me/calendars/primary/calendarView?$skiptoken=two"
		}
		page["@odata.nextLink"] = next
	}
	_ = json.NewEncoder(w).Encode(page)
}

func newOwnedCalendarSyncFixture(t *testing.T, provider string) (*userStorageFixture, *ownedCalendarSyncAPI) {
	return newOwnedCalendarSyncFixtureServices(t, provider, nil)
}
func newOwnedCalendarSyncFixtureServices(t *testing.T, provider string, calendarOptions *handler.UserCalendarSyncOptions) (*userStorageFixture, *ownedCalendarSyncAPI) {
	t.Helper()
	var api *ownedCalendarSyncAPI
	f, _, server := newOwnedCalendarFixtureServices(t, provider, func(base *ownedCalendarDiscoveryAPI) http.Handler {
		api = &ownedCalendarSyncAPI{base: base, calls: map[string]int{}, entered: make(chan struct{}), release: make(chan struct{})}
		return api
	}, calendarOptions)
	t.Cleanup(api.unblock)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.accountStore.SetCalendarServiceEnabled(t.Context(), owner, f.accounts[owner].ID, true); err != nil {
			t.Fatal(err)
		}
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			start := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
			end := start.Add(time.Hour)
			outside := start.AddDate(1, 0, 0)
			outsideEnd := outside.Add(time.Hour)
			remote := "kept"
			if provider == "caldav" {
				remote = server.URL + "/users/" + owner + "/calendars/primary/event.ics"
			}
			return db.ReplaceCalendarEvents(t.Context(), owner, "same-source", []storage.CalendarEvent{
				{ID: "same-event", RemoteID: remote, Summary: owner + " old cached appointment", StartAt: &start, EndAt: &end, ETag: "old-version"},
				{ID: "missing-event", RemoteID: "missing", Summary: "absent", AllDay: true, StartDate: "2026-10-04", EndDate: "2026-10-05"},
				{ID: "outside-event", RemoteID: "outside", Summary: "outside", StartAt: &outside, EndAt: &outsideEnd},
			}, start.Add(-time.Hour), end.Add(time.Hour))
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, api
}

func calendarSyncRequest(f *userStorageFixture, owner string) *httptest.ResponseRecorder {
	return f.request(owner, "POST", "/api/calendar/sync", url.Values{"month": {"2026-10"}, "account_id": {f.accounts[owner].ID}}.Encode())
}

func TestUserCalendarSyncHTTPProvidersRefreshOwnerIsolationAndEvents(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newOwnedCalendarSyncFixture(t, provider)
			api.rejectOld = provider != "caldav"
			events := f.events.Subscribe()
			defer f.events.Unsubscribe(events)
			for _, owner := range []string{"alice", "bob"} {
				r := calendarSyncRequest(f, owner)
				if r.Code != 200 || r.Header().Get("X-Gofer-Status") == "error" || !strings.Contains(r.Body.String(), owner+" private refreshed appointment") {
					t.Fatal("refresh", provider, owner, r.Code, r.Body.String())
				}
				if strings.Contains(r.Body.String(), map[string]string{"alice": "bob", "bob": "alice"}[owner]+" private refreshed") {
					t.Fatal("cross-owner rendering")
				}
				if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
					event, err := db.GetCalendarEvent(t.Context(), owner, "same-event")
					if err != nil {
						return err
					}
					if event.Summary != owner+" private refreshed appointment" || event.Description != "Full notes" || !strings.Contains(event.AttendeesJSON, "guest@example.com") {
						t.Fatal("details/local ID", event)
					}
					if provider == "gmail" && !strings.Contains(event.OnlineMeetingJSON, "meet.google.com") {
						t.Fatal("Google meeting details lost", event.OnlineMeetingJSON)
					}
					if provider == "outlook" && !strings.Contains(event.OnlineMeetingJSON, "teams.microsoft.com") {
						t.Fatal("Microsoft meeting details lost", event.OnlineMeetingJSON)
					}
					var missing, outside int
					if err := db.Read().QueryRow(`SELECT is_deleted FROM calendar_events WHERE id='missing-event'`).Scan(&missing); err != nil {
						return err
					}
					if err := db.Read().QueryRow(`SELECT is_deleted FROM calendar_events WHERE id='outside-event'`).Scan(&outside); err != nil {
						return err
					}
					if missing != 1 || outside != 0 {
						t.Fatal("window boundaries", missing, outside)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			seen := map[string]bool{}
			for len(events) > 0 {
				event := <-events
				if event.Type == mail.EventCalendarSync {
					if event.Payload["changed"] == true {
						seen[event.UserID] = true
					}
				}
			}
			if !seen["alice"] || !seen["bob"] {
				t.Fatal("owned successful SSE events missing", seen)
			}
			if provider != "caldav" {
				api.base.mu.Lock()
				scopes := append([]string(nil), api.base.scopes...)
				api.base.mu.Unlock()
				if len(scopes) != 2 {
					t.Fatal("scoped refresh", scopes)
				}
				if provider == "outlook" {
					for _, scope := range scopes {
						if !strings.Contains(strings.ToLower(scope), "calendars.read") || strings.Contains(strings.ToLower(scope), "contacts") || strings.Contains(strings.ToLower(scope), "mail.read") {
							t.Fatal("wrong calendar token scope", scope)
						}
					}
				}
			}
		})
	}
}

func TestUserCalendarSyncHTTPPartialProviderAndLocalFailureRetry(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		for _, fault := range []string{"provider", "local"} {
			t.Run(provider+"/"+fault, func(t *testing.T) {
				f, api := newOwnedCalendarSyncFixture(t, provider)
				if fault == "provider" {
					api.failSecond = true
				} else {
					if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
						_, err := db.Write().Exec(`CREATE TRIGGER fail_calendar BEFORE UPDATE ON calendar_events WHEN NEW.id='missing-event' AND NEW.is_deleted=1 BEGIN SELECT RAISE(ABORT,'synthetic calendar failure'); END`)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				r := calendarSyncRequest(f, "alice")
				if r.Code != 200 || r.Header().Get("X-Gofer-Status") != "error" {
					t.Fatal("failure response", r.Code, r.Body.String())
				}
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					event, err := db.GetCalendarEvent(t.Context(), "alice", "same-event")
					if err != nil {
						return err
					}
					if event.Summary != "alice old cached appointment" {
						t.Fatal("partial provider/local publication", event)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				api.mu.Lock()
				api.failSecond = false
				api.mu.Unlock()
				if fault == "local" {
					if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error { _, err := db.Write().Exec(`DROP TRIGGER fail_calendar`); return err }); err != nil {
						t.Fatal(err)
					}
				}
				if r := calendarSyncRequest(f, "alice"); r.Code != 200 || r.Header().Get("X-Gofer-Status") == "error" {
					t.Fatal("retry", r.Code, r.Body.String())
				}
			})
		}
	}
}

func TestUserCalendarSyncHTTPLateChangesEvictionAndShutdown(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		for _, change := range []string{"cache", "source", "config", "account", "reconnect", "shutdown"} {
			if provider == "caldav" && change == "reconnect" {
				continue
			}
			t.Run(provider+"/"+change, func(t *testing.T) {
				f, api := newOwnedCalendarSyncFixture(t, provider)
				api.blockOwner = "alice"
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() { done <- calendarSyncRequest(f, "alice") }()
				select {
				case <-api.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("provider not reached")
				}
				// MaxOpen=1: Bob must be able to read while Alice is in HTTP.
				if r := f.request("bob", "GET", "/calendar?month=2026-10&cache=1", ""); r.Code != 200 {
					t.Fatal("HTTP retained Alice lease", r.Code)
				}
				want := 409
				switch change {
				case "cache":
					if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
						_, err := db.Write().Exec(`UPDATE calendar_events SET summary='new incoming response',etag='incoming-version' WHERE id='same-event'`)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				case "source":
					if err := f.accountStore.SetCalendarServiceEnabled(t.Context(), "alice", f.accounts["alice"].ID, false); err != nil {
						t.Fatal(err)
					}
				case "config":
					if err := f.accountStore.WithAccountForUser(t.Context(), "alice", f.accounts["alice"].ID, func(_ *config.AccountStore, db *storage.DB) error {
						_, err := db.Write().Exec(`UPDATE accounts SET username='edited' WHERE id=?`, f.accounts["alice"].ID)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				case "account":
					want = 404
					if _, err := f.system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, f.accounts["alice"].ID); err != nil {
						t.Fatal(err)
					}
				case "reconnect":
					if _, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET revision=revision+1 WHERE user_id='alice' AND account_id=?`, f.accounts["alice"].ID); err != nil {
						t.Fatal(err)
					}
				case "shutdown":
					want = 503
					f.stopIMAP()
				}
				if change != "shutdown" {
					api.unblock()
				}
				select {
				case r := <-done:
					if r.Code != want {
						t.Fatal("late change", change, r.Code, r.Body.String())
					}
				case <-time.After(5 * time.Second):
					t.Fatal("operation did not join")
				}
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					var summary string
					if err := db.Read().QueryRow(`SELECT summary FROM calendar_events WHERE id='same-event'`).Scan(&summary); err != nil {
						return err
					}
					expected := "alice old cached appointment"
					if change == "cache" {
						expected = "new incoming response"
					}
					if summary != expected {
						t.Fatal("late result overwrote cache", summary)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestUserCalendarSyncHTTPThrottleForeignContinuationAndRedirect(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newOwnedCalendarSyncFixture(t, provider)
			api.throttle = true
			if r := calendarSyncRequest(f, "alice"); r.Header().Get("X-Gofer-Status") != "error" {
				t.Fatal("throttle", r.Code)
			}
			before := api.count("alice")
			if r := calendarSyncRequest(f, "alice"); r.Code != 200 || r.Header().Get("X-Gofer-Status") != "error" {
				t.Fatal("durable throttle", r.Code)
			}
			if api.count("alice") != before {
				t.Fatal("request bypassed durable cooldown")
			}
		})
	}
	for _, fault := range []string{"next", "redirect", "dav-resource"} {
		t.Run(fault, func(t *testing.T) {
			provider := "outlook"
			if fault == "dav-resource" {
				provider = "caldav"
			}
			f, api := newOwnedCalendarSyncFixture(t, provider)
			var leaked atomic.Int32
			foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1); w.WriteHeader(200) }))
			defer foreign.Close()
			switch fault {
			case "next":
				api.next = foreign.URL + "/events"
			case "redirect":
				api.redirect = foreign.URL + "/events"
			case "dav-resource":
				api.foreignHref = "/users/bob/calendars/primary/other.ics"
			}
			if r := calendarSyncRequest(f, "alice"); r.Code != 200 || r.Header().Get("X-Gofer-Status") != "error" {
				t.Fatal("foreign endpoint", r.Code, r.Body.String())
			}
			if leaked.Load() != 0 {
				t.Fatal("foreign endpoint contacted")
			}
		})
	}
}

func TestUserCalendarSyncHTTPWriterWaitRechecksAuthorizationAndCancellation(t *testing.T) {
	for _, change := range []string{"authorization", "shutdown"} {
		t.Run(change, func(t *testing.T) {
			f, api := newOwnedCalendarSyncFixture(t, "outlook")
			api.blockOwner = "alice"
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- calendarSyncRequest(f, "alice") }()
			select {
			case <-api.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("provider not reached")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := f.routing.WithUser(ctx, "alice", func(db *storage.DB) error {
				tx, err := db.Write().BeginTx(t.Context(), nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				before := db.Write().Stats().WaitCount
				api.unblock()
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for db.Write().Stats().WaitCount == before {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-tick.C:
					}
				}
				want := 409
				if change == "authorization" {
					if _, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET revision=revision+1 WHERE account_id=?`, f.accounts["alice"].ID); err != nil {
						return err
					}
					if err := tx.Commit(); err != nil {
						return err
					}
				} else {
					want = 503
					f.stopIMAP()
				}
				select {
				case response := <-done:
					if response.Code != want {
						t.Fatal("writer-wait publication", change, response.Code, response.Body.String())
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				if change == "shutdown" {
					if err := tx.Rollback(); err != nil {
						return err
					}
				}
				var summary string
				if err := db.Read().QueryRow(`SELECT summary FROM calendar_events WHERE id='same-event'`).Scan(&summary); err != nil {
					return err
				}
				if summary != "alice old cached appointment" {
					t.Fatal("late result published", summary)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
