package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func ownedCalendarAction(t *testing.T, f *userContactPushFixture, owner string, write bool) (*userCalendarRequest, context.Context) {
	t.Helper()
	snapshot, err := f.h.userAccounts.SnapshotCalendarSource(t.Context(), owner, "same-source")
	if err != nil {
		t.Fatal(err)
	}
	p := &userCalendarRequest{h: f.h, source: snapshot}
	ctx, err := p.actionContext(t.Context(), write)
	if err != nil {
		t.Fatal(err)
	}
	return p, ctx
}

func TestUserCalendarProviderReplaysOnlyRejectedRequestWithScopedWriteGrant(t *testing.T) {
	for _, method := range []string{http.MethodPatch, http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			var calls, refreshes atomic.Int32
			var firstBody string
			f := ownedCalendarWorkerFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/token" {
					refreshes.Add(1)
					if err := r.ParseForm(); err != nil {
						t.Error(err)
					}
					if r.Form.Get("scope") != "https://graph.microsoft.com/Calendars.ReadWrite" || r.Form.Get("refresh_token") != "alice-refresh" {
						t.Error("wrong refresh scope or owner", r.Form)
					}
					_, _ = io.WriteString(w, `{"access_token":"alice-fresh","refresh_token":"rotated","token_type":"Bearer","expires_in":3600,"scope":"https://graph.microsoft.com/Calendars.ReadWrite"}`)
					return
				}
				call := calls.Add(1)
				body, _ := io.ReadAll(r.Body)
				if call == 1 {
					firstBody = string(body)
				} else if string(body) != firstBody {
					t.Error("replay changed payload")
				}
				if r.Method != method || r.Header.Get("Prefer") != `IdType="ImmutableId"` || ((method == http.MethodPatch || method == http.MethodDelete) && r.Header.Get("If-Match") != `"v1"`) {
					t.Error("method or concurrency/identity headers changed", r.Method, r.Header)
				}
				if r.Header.Get("Authorization") == "Bearer alice-access" {
					http.Error(w, "old token", 401)
					return
				}
				if r.Header.Get("Authorization") != "Bearer alice-fresh" {
					t.Error("foreign or stale token dispatched")
					http.Error(w, "wrong token", 401)
					return
				}
				if method == http.MethodDelete {
					w.WriteHeader(204)
					return
				}
				_, _ = io.WriteString(w, `{"id":"confirmed"}`)
			}), "alice", "bob")
			p, ctx := ownedCalendarAction(t, f, "alice", true)
			endpoint := outlookGraphBaseURL + "/me/calendars/primary/events/event"
			var err error
			switch method {
			case http.MethodPatch:
				err = calendarUpdateJSON(ctx, endpoint, "wrong-caller-token", `"v1"`, map[string]string{"subject": "updated"}, &map[string]any{})
			case http.MethodPost:
				err = calendarCreateJSON(ctx, http.MethodPost, endpoint+"/accept", "wrong-caller-token", map[string]bool{"sendResponse": true}, &map[string]any{})
			case http.MethodDelete:
				err = calendarDeleteJSON(ctx, endpoint, "wrong-caller-token", `"v1"`)
			}
			if err != nil || calls.Load() != 2 || refreshes.Load() != 1 {
				t.Fatal("single rejected request replay", calls.Load(), refreshes.Load(), err)
			}
			if err := p.validate(t.Context()); err != nil {
				t.Fatal("own scoped refresh invalidated operation", err)
			}
			if err := f.h.userCredentials.ValidateCalendarAuthorization(t.Context(), p.authorization, "bob", f.accounts["bob"].ID, true); !errors.Is(err, mailauth.ErrMailboxAuthorizationChanged) {
				t.Fatal("Alice authority accepted for Bob", err)
			}
		})
	}
}

func TestUserCalendarProviderDoesNotReplayAmbiguousWritesAndPersistsCooldown(t *testing.T) {
	for _, status := range []int{408, 429, 500, 503, 307} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls, refreshes atomic.Int32
			f := ownedCalendarWorkerFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					refreshes.Add(1)
					http.Error(w, "unexpected refresh", 500)
					return
				}
				calls.Add(1)
				w.Header().Set("Retry-After", "90")
				w.Header().Set("Location", "https://foreign.test/events")
				w.WriteHeader(status)
			}), "alice")
			_, ctx := ownedCalendarAction(t, f, "alice", true)
			endpoint := outlookGraphBaseURL + "/me/calendars/primary/events/event"
			err := calendarUpdateJSON(ctx, endpoint, "ignored", `"v1"`, map[string]string{"subject": "change"}, &map[string]any{})
			if err == nil || calls.Load() != 1 || refreshes.Load() != 0 {
				t.Fatal("ambiguous action retried or accepted", calls.Load(), refreshes.Load(), err)
			}
			if status == 429 || status == 503 {
				until, err := f.h.userStorage.ProviderRetryUntil(t.Context(), "alice", f.accounts["alice"].ID)
				if err != nil || until.Before(time.Now().Add(time.Minute)) {
					t.Fatal("Retry-After lost by legacy status adapter", until, err)
				}
				err = calendarDeleteJSON(ctx, endpoint, "ignored", `"v1"`)
				var deferred userContactDeferred
				if !errors.As(err, &deferred) || calls.Load() != 1 {
					t.Fatal("cooldown allowed another compound dispatch", calls.Load(), err)
				}
			}
		})
	}
}

func TestUserCalendarProvider401ReplayIsBoundedAndRequiresReproducibleBody(t *testing.T) {
	for _, replayable := range []bool{false, true} {
		t.Run(map[bool]string{false: "streamed-body", true: "second-401"}[replayable], func(t *testing.T) {
			var refreshes, calls atomic.Int32
			f := ownedCalendarWorkerFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/token" {
					t.Error("unexpected API request")
					http.Error(w, "unexpected", 500)
					return
				}
				refreshes.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"access_token":"alice-fresh","expires_in":3600,"scope":"https://graph.microsoft.com/Calendars.ReadWrite"}`)
			}), "alice")
			_, ctx := ownedCalendarAction(t, f, "alice", true)
			client := &http.Client{Transport: calendarActionTransportFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				body, err := io.ReadAll(r.Body)
				r.Body.Close()
				if err != nil || string(body) != `{"sendResponse":true}` {
					t.Error("body changed", string(body), err)
				}
				return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("rejected")), Request: r}, nil
			})}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, outlookGraphBaseURL+"/me/calendars/primary/events/event/accept", strings.NewReader(`{"sendResponse":true}`))
			if err != nil {
				t.Fatal(err)
			}
			if !replayable {
				req.GetBody = nil
			}
			response, err := calendarProviderDo(client, req)
			if err != nil || response.StatusCode != 401 {
				t.Fatal("rejected request classification", err, response)
			}
			response.Body.Close()
			wantCalls, wantRefreshes := int32(1), int32(0)
			if replayable {
				wantCalls, wantRefreshes = 2, 1
			}
			if calls.Load() != wantCalls || refreshes.Load() != wantRefreshes {
				t.Fatal("unbounded or unreproducible replay", calls.Load(), refreshes.Load())
			}
		})
	}
}

func TestUserCalendarProviderGoogleCalendarSpecificCapabilityAndOpaqueIdentity(t *testing.T) {
	f := newUserContactPushFixture(t, "gmail", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("endpoint check reached provider") }))
	expires := time.Now().Add(time.Hour)
	if err := f.h.userCredentials.UpsertForUser(t.Context(), "alice", f.accounts["alice"].ID, "google", "subject", "alice-access", "alice-refresh", "Bearer", &expires, mailauth.GoogleCalendarReadOnlyScope+" "+mailauth.GoogleCalendarEventsScope); err != nil {
		t.Fatal(err)
	}
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		return db.ReplaceCalendarSources(t.Context(), "alice", f.accounts["alice"].ID, "gmail", []storage.CalendarSource{{ID: "same-source", RemoteID: "opaque/calendar@example.com", IsSelected: true}})
	}); err != nil {
		t.Fatal(err)
	}
	p, _ := ownedCalendarAction(t, f, "alice", true)
	calendar := googleCalendarAPIBaseURL + "/calendars/opaque%2Fcalendar@example.com/events"
	capability := googleCalendarAPIBaseURL + "/users/me/calendarList/opaque%2Fcalendar@example.com"
	for _, tc := range []struct {
		method, endpoint string
		ok               bool
	}{
		{"GET", capability, true}, {"POST", capability, false}, {"GET", calendar, true}, {"POST", calendar, true},
		{"PATCH", calendar + "/event%2Fid", true}, {"GET", calendar + "/event/instances", true},
		{"POST", calendar + "/event/accept", false}, {"GET", strings.Replace(calendar, "opaque%2Fcalendar", "opaque/calendar", 1), false},
		{"GET", googleCalendarAPIBaseURL + "/users/me/calendarList/other", false},
	} {
		req, err := http.NewRequestWithContext(t.Context(), tc.method, tc.endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.providerEndpoint(req); (err == nil) != tc.ok {
			t.Fatal("Google endpoint scope", tc, err)
		}
	}
}

func TestUserCalendarProviderRechecksAuthorityAfterResponseWithoutStoreLease(t *testing.T) {
	for _, change := range []string{"reconnect", "deselected", "central-account", "owner"} {
		t.Run(change, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			f := ownedCalendarWorkerFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, `{"id":"remote-accepted"}`)
			}), "alice", "bob")
			_, ctx := ownedCalendarAction(t, f, "alice", true)
			done := make(chan error, 1)
			go func() {
				done <- calendarCreateJSON(ctx, http.MethodPost, outlookGraphBaseURL+"/me/calendars/primary/events", "ignored", map[string]string{"subject": "created"}, &map[string]any{})
			}()
			wait, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			select {
			case <-entered:
			case <-wait.Done():
				t.Fatal("provider not reached")
			}
			// Bob's store opens in the sole cache slot while Alice is waiting on
			// HTTP. This cannot finish if dispatch retains Alice's local lease.
			if _, err := f.h.userAccounts.SnapshotCalendarSource(wait, "bob", "same-source"); err != nil {
				t.Fatal("provider wait pinned store", err)
			}
			switch change {
			case "reconnect":
				expires := time.Now().Add(time.Hour)
				if err := f.h.userCredentials.UpsertForUser(wait, "alice", f.accounts["alice"].ID, "microsoft", "subject", "reconnected", "new-refresh", "Bearer", &expires, "https://graph.microsoft.com/Calendars.ReadWrite"); err != nil {
					t.Fatal(err)
				}
			case "deselected":
				if err := f.h.userAccounts.SetCalendarSourcesSelected(wait, "alice", f.accounts["alice"].ID, nil); err != nil {
					t.Fatal(err)
				}
			case "central-account":
				if _, err := f.system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, f.accounts["alice"].ID); err != nil {
					t.Fatal(err)
				}
			case "owner":
				if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			select {
			case err := <-done:
				want := mailauth.ErrMailboxAuthorizationChanged
				switch change {
				case "deselected":
					want = storage.ErrCalendarSourceChanged
				case "central-account":
					want = storage.ErrAccountRoute
				case "owner":
					want = storage.ErrUserStoreOwner
				}
				if !errors.Is(err, want) {
					t.Fatal("late authority change reported remote success", err, want)
				}
			case <-wait.Done():
				t.Fatal("provider operation did not finish")
			}
			if calls.Load() != 1 {
				t.Fatal("remote write replayed after authority changed", calls.Load())
			}
		})
	}
}

func TestUserCalendarProviderJoinedRootShutdownCancelsNetwork(t *testing.T) {
	entered := make(chan struct{})
	f := ownedCalendarWorkerFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}), "alice")
	done := make(chan error, 1)
	go func() {
		done <- f.h.userIMAP.RunAccountService(t.Context(), "alice", f.accounts["alice"].ID, mail.AccountServiceCalendar, time.Minute, func(ctx context.Context) error {
			snapshot, err := f.h.userAccounts.SnapshotCalendarSource(ctx, "alice", "same-source")
			if err != nil {
				return err
			}
			p := &userCalendarRequest{h: f.h, source: snapshot}
			ctx, err = p.actionContext(ctx, true)
			if err != nil {
				return err
			}
			return calendarCreateJSON(ctx, http.MethodGet, outlookGraphBaseURL+"/me/calendars/primary/events/event", "ignored", nil, &map[string]any{})
		})
	}()
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("provider not reached")
	}
	f.cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("root cancellation lost", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("calendar provider not joined on shutdown")
	}
}

func TestUserCalendarProviderEndpointPreservesOpaqueIDsAndMethods(t *testing.T) {
	f := ownedCalendarWorkerFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("endpoint unit check dispatched HTTP") }), "alice")
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE calendar_sources SET remote_id='opaque/identifier' WHERE id='same-source'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p, _ := ownedCalendarAction(t, f, "alice", true)
	base := outlookGraphBaseURL + "/me/calendars/opaque%2Fidentifier"
	for _, tc := range []struct {
		method, endpoint string
		ok               bool
	}{
		{"GET", base, true}, {"GET", base + "/events", true}, {"POST", base + "/events", true}, {"PATCH", base + "/events/event%2Fid", true},
		{"DELETE", base + "/events/event", true}, {"POST", base + "/events/event/accept", true}, {"GET", base + "/events/event/instances", true},
		{"GET", strings.Replace(base, "opaque%2Fidentifier", "opaque/identifier", 1) + "/events", false},
		{"GET", strings.Replace(base, "opaque%2Fidentifier", "opaque%2fidentifier", 1) + "/events", false},
		{"GET", strings.Replace(base, "opaque%2Fidentifier", "other", 1) + "/events", false},
		{"GET", base + "/eventsSuffix", false}, {"PATCH", base + "/events", false}, {"GET", base + "/events/../event", false},
		{"GET", base + "/events/%2e%2e", false}, {"POST", base + "/events/event/sendMail", false}, {"GET", base + "/events/event#fragment", false},
		{"GET", "https://foreign.test/me/calendars/opaque%2Fidentifier/events", false},
		{"GET", "https://user:secret@graph.microsoft.com/v1.0/me/calendars/opaque%2Fidentifier/events", false},
	} {
		req, err := http.NewRequestWithContext(t.Context(), tc.method, tc.endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.providerEndpoint(req); (err == nil) != tc.ok {
			t.Fatal("endpoint policy", tc, err)
		}
	}
	// Read-purpose actions cannot use a write method even with a current grant.
	p.write = false
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodDelete, base+"/events/event", nil)
	if err := p.providerEndpoint(req); err == nil {
		t.Fatal("read-purpose action authorized write")
	}
	for _, invalid := range []*userCalendarRequest{nil, {}, {h: f.h}, {h: f.h, source: p.source, event: &config.UserCalendarEventSnapshot{}}} {
		if err := invalid.validate(t.Context()); err == nil {
			t.Fatal("invalid/multi-scope request accepted")
		}
	}
}

func TestUserCalendarProviderDAVCredentialsCollectionAndPrincipal(t *testing.T) {
	f := newUserContactPushFixture(t, "carddav", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(local *config.AccountStore, db *storage.DB) error {
		if err := local.SaveCalDAVConfig(t.Context(), "alice", f.accounts["alice"].ID, "https://dav.test/", "alice-calendar", "calendar-secret", false); err != nil {
			return err
		}
		return db.ReplaceCalendarSources(t.Context(), "alice", f.accounts["alice"].ID, "caldav", []storage.CalendarSource{{ID: "same-source", RemoteID: "https://dav.test/calendars/alice/", IsSelected: true}})
	}); err != nil {
		t.Fatal(err)
	}
	_, ctx := ownedCalendarAction(t, f, "alice", true)
	var calls atomic.Int32
	client := &http.Client{Transport: calendarActionTransportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		username, password, ok := r.BasicAuth()
		if !ok || username != "alice-calendar" || password != "calendar-secret" {
			t.Error("wrong DAV credentials", username)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
	})}
	for _, tc := range []struct {
		method, endpoint string
		ok               bool
	}{
		{"GET", "https://dav.test/calendars/alice/event.ics", true},
		{"PUT", "https://dav.test/calendars/alice/event.ics", true},
		{"DELETE", "https://dav.test/calendars/alice/event.ics", true},
		{"OPTIONS", "https://dav.test/calendars/alice/", true},
		{"PROPFIND", "https://dav.test/principals/alice/", true},
		{"GET", "https://dav.test/calendars/bob/event.ics", false},
		{"PUT", "https://dav.test/calendars/alice/../bob/event.ics", false},
		{"PUT", "https://dav.test/calendars/alice/%2e%2e/bob/event.ics", false},
		{"GET", "https://dav.test/calendars/alice%2Fevent.ics", false},
		{"PROPFIND", "https://foreign.test/principals/alice/", false},
		{"POST", "https://dav.test/calendars/alice/event.ics", false},
	} {
		before := calls.Load()
		req, err := http.NewRequestWithContext(ctx, tc.method, tc.endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth("wrong", "wrong")
		response, err := calendarProviderDo(client, req)
		if response != nil {
			response.Body.Close()
		}
		if (err == nil) != tc.ok || (calls.Load() != before) != tc.ok {
			t.Fatal("DAV dispatch policy", tc, err, calls.Load())
		}
	}
}

type calendarActionTransportFunc func(*http.Request) (*http.Response, error)

func (f calendarActionTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
