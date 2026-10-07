package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

type ownedCalendarDiscoveryAPI struct {
	provider                               string
	mu                                     sync.Mutex
	calls                                  map[string]int
	scopes                                 []string
	block, rejectOld, throttle, failSecond bool
	next, redirect                         string
	entered, release                       chan struct{}
	once, releaseOnce                      sync.Once
}

func (a *ownedCalendarDiscoveryAPI) unblock() { a.releaseOnce.Do(func() { close(a.release) }) }
func (a *ownedCalendarDiscoveryAPI) count(owner, path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[owner+"|"+path]
}
func (a *ownedCalendarDiscoveryAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		_ = r.ParseForm()
		owner := strings.Split(r.FormValue("refresh_token"), "-")[0]
		a.mu.Lock()
		a.calls[owner+"|token"]++
		a.scopes = append(a.scopes, r.FormValue("scope"))
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		scope := mailauth.GoogleCalendarReadOnlyScope + " " + mailauth.GoogleCalendarEventsScope
		if a.provider == "outlook" {
			scope = "https://graph.microsoft.com/Calendars.Read"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": owner + "-fresh", "refresh_token": owner + "-rotated", "token_type": "Bearer", "expires_in": 3600, "scope": scope})
		return
	}
	owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
	if a.provider == "caldav" {
		username, password, ok := r.BasicAuth()
		if !ok || password != username+"-calendar-secret" || r.Method != "PROPFIND" {
			http.Error(w, "wrong calendar credentials", 401)
			return
		}
		owner = username
	} else if r.Method != "GET" {
		http.Error(w, "wrong calendar method", 400)
		return
	}
	if owner != "alice" && owner != "bob" {
		http.Error(w, "wrong calendar owner", 401)
		return
	}
	a.mu.Lock()
	a.calls[owner+"|"+r.URL.Path]++
	block, reject, throttle, next, redirect, failSecond := a.block && owner == "alice", a.rejectOld, a.throttle, a.next, a.redirect, a.failSecond
	a.mu.Unlock()
	if block {
		a.once.Do(func() { close(a.entered) })
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
	if a.provider == "caldav" {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(207)
		switch {
		case r.URL.Path == "/.well-known/caldav":
			_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:propstat><d:prop><d:current-user-principal><d:href>/users/%s/principal/</d:href></d:current-user-principal></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, owner)
		case r.URL.Path == "/users/"+owner+"/principal/":
			_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:propstat><d:prop><c:calendar-home-set><d:href>/users/%s/calendars/</d:href></c:calendar-home-set></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, owner)
		default:
			_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>/users/%s/calendars/primary/</d:href><d:propstat><d:prop><d:displayname>%s discovered primary</d:displayname><d:resourcetype><c:calendar/></d:resourcetype><d:current-user-privilege-set><d:privilege><d:write/></d:privilege></d:current-user-privilege-set></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response><d:response><d:href>/users/%s/calendars/team/</d:href><d:propstat><d:prop><d:displayname>%s discovered team</d:displayname><d:resourcetype><c:calendar/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, owner, owner, owner, owner)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	second := r.URL.Query().Get("pageToken") == "two" || r.URL.Query().Get("$skiptoken") == "two"
	if second && failSecond {
		http.Error(w, "synthetic second page failure", 500)
		return
	}
	if a.provider == "gmail" {
		remote, name := "primary", owner+" discovered primary"
		if second {
			remote, name = "team", owner+" discovered team"
		}
		body := map[string]any{"items": []any{map[string]any{"id": remote, "summary": name, "primary": !second, "accessRole": "owner", "timeZone": "Europe/Prague"}}}
		if !second {
			body["nextPageToken"] = "two"
		}
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	remote, name := "primary", owner+" discovered primary"
	if second {
		remote, name = "team", owner+" discovered team"
	}
	body := map[string]any{"value": []any{map[string]any{"id": remote, "name": name, "isDefaultCalendar": !second, "canEdit": true}}}
	if !second {
		if next == "" {
			next = "https://graph.microsoft.com/v1.0/me/calendars?$skiptoken=two"
		}
		body["@odata.nextLink"] = next
	}
	_ = json.NewEncoder(w).Encode(body)
}

func TestUserCalendarDiscoveryHTTPLocalFaultRollbackAndCompleteProviderRetry(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		t.Run(provider, func(t *testing.T) {
			f, _, _ := newOwnedCalendarDiscoveryFixture(t, provider)
			var savedPassword []byte
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				if provider == "caldav" {
					if err := db.Read().QueryRow(`SELECT encrypted_password FROM account_caldav_configs WHERE account_id=?`, f.accounts["alice"].ID).Scan(&savedPassword); err != nil {
						return err
					}
				}
				_, err := db.Write().Exec(`CREATE TRIGGER fail_http_discovery BEFORE INSERT ON calendar_sources WHEN NEW.remote_id LIKE '%team%' BEGIN SELECT RAISE(ABORT,'synthetic source write fault'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if response := calendarDiscoveryRequest(f, "alice"); response.Code != 503 {
				t.Fatal("local discovery fault reported success", response.Code, response.Body.String())
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				sources, err := db.ListCalendarSourcesForAccount(t.Context(), "alice", f.accounts["alice"].ID)
				if err != nil {
					return err
				}
				if len(sources) != 1 || sources[0].Name != "alice old primary" || sources[0].ID != "same-source" {
					t.Fatal("partial source catalog escaped", sources)
				}
				if provider == "caldav" {
					var password []byte
					if err := db.Read().QueryRow(`SELECT encrypted_password FROM account_caldav_configs WHERE account_id=?`, f.accounts["alice"].ID).Scan(&password); err != nil {
						return err
					}
					if !bytes.Equal(password, savedPassword) {
						t.Fatal("CalDAV password saved without catalog")
					}
				}
				_, err = db.Write().Exec(`DROP TRIGGER fail_http_discovery`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if response := calendarDiscoveryRequest(f, "alice"); response.Code != 200 || !strings.Contains(response.Body.String(), "Discovered 2") {
				t.Fatal("complete discovery retry", response.Code, response.Body.String())
			}
		})
	}
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider+"/partial-provider", func(t *testing.T) {
			f, api, _ := newOwnedCalendarDiscoveryFixture(t, provider)
			api.failSecond = true
			if response := calendarDiscoveryRequest(f, "alice"); response.Code != 200 || !strings.Contains(response.Body.String(), "failed") {
				t.Fatal("partial provider response", response.Code, response.Body.String())
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				sources, err := db.ListCalendarSourcesForAccount(t.Context(), "alice", f.accounts["alice"].ID)
				if err == nil && (len(sources) != 1 || sources[0].Name != "alice old primary") {
					t.Fatal("partial provider published", sources)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			api.failSecond = false
			api.mu.Unlock()
			if response := calendarDiscoveryRequest(f, "alice"); response.Code != 200 || !strings.Contains(response.Body.String(), "Discovered 2") {
				t.Fatal("provider retry", response.Code, response.Body.String())
			}
		})
	}
}

type ownedCalendarDiscoveryTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (t ownedCalendarDiscoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "www.googleapis.com" && r.URL.Host != "graph.microsoft.com" {
		return t.base.RoundTrip(r)
	}
	copy := r.Clone(r.Context())
	address := *r.URL
	address.Scheme, address.Host = t.target.Scheme, t.target.Host
	address.Path = strings.TrimPrefix(strings.TrimPrefix(address.Path, "/calendar/v3"), "/v1.0")
	copy.URL = &address
	return t.base.RoundTrip(copy)
}

func newOwnedCalendarDiscoveryFixture(t *testing.T, provider string) (*userStorageFixture, *ownedCalendarDiscoveryAPI, *httptest.Server) {
	return newOwnedCalendarFixture(t, provider, nil)
}
func newOwnedCalendarFixture(t *testing.T, provider string, wrap func(*ownedCalendarDiscoveryAPI) http.Handler) (*userStorageFixture, *ownedCalendarDiscoveryAPI, *httptest.Server) {
	return newOwnedCalendarFixtureServices(t, provider, wrap, nil)
}
func newOwnedCalendarFixtureServices(t *testing.T, provider string, wrap func(*ownedCalendarDiscoveryAPI) http.Handler, calendarOptions *handler.UserCalendarSyncOptions) (*userStorageFixture, *ownedCalendarDiscoveryAPI, *httptest.Server) {
	t.Helper()
	api := &ownedCalendarDiscoveryAPI{provider: provider, calls: map[string]int{}, entered: make(chan struct{}), release: make(chan struct{})}
	var providerHandler http.Handler = api
	if wrap != nil {
		providerHandler = wrap(api)
	}
	server := httptest.NewTLSServer(providerHandler)
	t.Cleanup(server.Close)
	t.Cleanup(api.unblock)
	previous := http.DefaultTransport
	address, _ := url.Parse(server.URL)
	http.DefaultTransport = ownedCalendarDiscoveryTransport{base: server.Client().Transport, target: address}
	t.Cleanup(func() { http.DefaultTransport = previous })
	client := &oauth2.Config{ClientID: "client", ClientSecret: "secret", Endpoint: oauth2.Endpoint{TokenURL: server.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}}
	f := newUserStorageFixtureServices(t, true, &mailauth.Config{GoogleClient: client, MicrosoftClient: client}, nil, calendarOptions)
	for _, owner := range []string{"alice", "bob"} {
		if provider != "caldav" {
			req := providers.GmailAccountRequest(owner+"@provider.test", owner, "same-subject")
			oauthProvider := "google"
			scope := mailauth.GoogleCalendarReadOnlyScope + " " + mailauth.GoogleCalendarEventsScope
			if provider == "outlook" {
				req = providers.OutlookAccountRequest(owner+"@provider.test", owner, "same-subject")
				oauthProvider = "microsoft"
				scope = userOutlookHTTPScopes + " https://graph.microsoft.com/Calendars.ReadWrite"
			}
			account, err := f.accountStore.CreateAccount(t.Context(), owner, req)
			if err != nil {
				t.Fatal(err)
			}
			f.accounts[owner] = account
			expires := time.Now().Add(time.Hour)
			if err := f.credentials.UpsertForUser(t.Context(), owner, account.ID, oauthProvider, "same-subject", owner+"-access", owner+"-refresh", "Bearer", &expires, scope); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.accountStore.WithAccountForUser(t.Context(), owner, f.accounts[owner].ID, func(local *config.AccountStore, db *storage.DB) error {
			if provider == "caldav" {
				if err := local.SaveCalDAVConfig(t.Context(), owner, f.accounts[owner].ID, server.URL+"/users/"+owner+"/dav", owner, owner+"-calendar-secret", false); err != nil {
					return err
				}
			}
			remote := "primary"
			if provider == "caldav" {
				remote = server.URL + "/users/" + owner + "/calendars/primary/"
			}
			return db.ReplaceCalendarSources(t.Context(), owner, f.accounts[owner].ID, provider, []storage.CalendarSource{{ID: "same-source", RemoteID: remote, Name: owner + " old primary", IsPrimary: true, IsSelected: false, AccessRole: "owner"}})
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, api, server
}

func calendarDiscoveryRequest(f *userStorageFixture, owner string) *httptest.ResponseRecorder {
	return f.request(owner, "POST", "/api/accounts/"+f.accounts[owner].ID+"/calendar/discover", "")
}

func TestUserCalendarDiscoveryHTTPProvidersScopedRefreshAndOwnedPublication(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		t.Run(provider, func(t *testing.T) {
			f, api, _ := newOwnedCalendarDiscoveryFixture(t, provider)
			api.rejectOld = provider != "caldav"
			for _, owner := range []string{"alice", "bob"} {
				response := calendarDiscoveryRequest(f, owner)
				other := "bob"
				if owner == "bob" {
					other = "alice"
				}
				if response.Code != 200 || !strings.Contains(response.Body.String(), "Discovered 2") || !strings.Contains(response.Body.String(), owner+" discovered primary") || strings.Contains(response.Body.String(), other+" discovered") {
					t.Fatal("provider discovery", owner, response.Code, response.Body.String())
				}
				if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
					sources, err := db.ListCalendarSourcesForAccount(t.Context(), owner, f.accounts[owner].ID)
					if err != nil {
						return err
					}
					if len(sources) != 2 {
						t.Fatal("incomplete discovery", sources)
					}
					for _, source := range sources {
						if source.IsSelected {
							t.Fatal("discovery changed existing selection or selected secondary", source)
						}
						if source.IsPrimary && source.ID != "same-source" {
							t.Fatal("local source ID replaced", source)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if provider != "caldav" {
				api.mu.Lock()
				scopes := append([]string(nil), api.scopes...)
				api.mu.Unlock()
				if len(scopes) != 2 {
					t.Fatal("unexpected refresh count", scopes)
				}
				if provider == "outlook" {
					for _, scope := range scopes {
						if !strings.Contains(scope, "Calendars.Read") || strings.Contains(scope, "Contacts") || strings.Contains(scope, "Mail.") {
							t.Fatal("calendar refresh broadened scope", scope)
						}
					}
				}
			}
			var count int
			if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM calendar_sources`).Scan(&count); err != nil || count != 0 {
				t.Fatal("central calendar fallback", count, err)
			}
		})
	}
}

func TestUserCalendarDiscoveryHTTPBlockedProviderEvictionAndAuthorityChanges(t *testing.T) {
	for _, change := range []string{"none", "source", "settings", "account", "reconnect"} {
		t.Run(change, func(t *testing.T) {
			f, api, _ := newOwnedCalendarDiscoveryFixture(t, "outlook")
			api.block = true
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- calendarDiscoveryRequest(f, "alice") }()
			select {
			case <-api.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("calendar HTTP not reached")
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := f.routing.WithUser(ctx, "bob", func(db *storage.DB) error {
				_, err := db.ListCalendarSourcesForAccount(ctx, "bob", f.accounts["bob"].ID)
				return err
			}); err != nil {
				t.Fatal("provider retained sole cache slot", err)
			}
			if change == "source" || change == "settings" {
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					query := `UPDATE calendar_sources SET remote_id='changed' WHERE id='same-source'`
					if change == "settings" {
						query = `UPDATE accounts SET username='changed'`
					}
					_, err := db.Write().Exec(query)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if change == "account" {
				if _, err := f.system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, f.accounts["alice"].ID); err != nil {
					t.Fatal(err)
				}
			}
			if change == "reconnect" {
				expires := time.Now().Add(time.Hour)
				if err := f.credentials.UpsertForUser(t.Context(), "alice", f.accounts["alice"].ID, "microsoft", "same-subject", "reconnected-access", "alice-new-refresh", "Bearer", &expires, userOutlookHTTPScopes); err != nil {
					t.Fatal(err)
				}
			}
			api.unblock()
			select {
			case response := <-done:
				want := 409
				if change == "none" {
					want = 200
				}
				if change == "account" {
					want = 404
				}
				if response.Code != want {
					t.Fatal("changed calendar response", change, response.Code, response.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("calendar HTTP did not finish")
			}
		})
	}
}

func TestUserCalendarDiscoveryHTTPRootShutdownAndDurableThrottle(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		t.Run(provider, func(t *testing.T) {
			f, api, _ := newOwnedCalendarDiscoveryFixture(t, provider)
			api.block = true
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- calendarDiscoveryRequest(f, "alice") }()
			select {
			case <-api.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("calendar provider not reached")
			}
			f.stopIMAP()
			joined := make(chan struct{})
			go func() { f.imap.Wait(); close(joined) }()
			select {
			case response := <-done:
				if response.Code == 200 {
					t.Fatal("root cancellation reported discovery success")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("root did not cancel provider")
			}
			select {
			case <-joined:
			case <-time.After(5 * time.Second):
				t.Fatal("root did not join discovery")
			}
		})
	}
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		t.Run(provider+"/throttle", func(t *testing.T) {
			f, api, _ := newOwnedCalendarDiscoveryFixture(t, provider)
			api.throttle = true
			if response := calendarDiscoveryRequest(f, "alice"); response.Code != 200 || !strings.Contains(response.Body.String(), "failed") {
				t.Fatal("throttle response", response.Code, response.Body.String())
			}
			path := "/me/calendars"
			if provider == "gmail" {
				path = "/users/me/calendarList"
			}
			if provider == "caldav" {
				path = "/.well-known/caldav"
			}
			before := api.count("alice", path)
			if response := calendarDiscoveryRequest(f, "alice"); response.Code != 200 || !strings.Contains(response.Body.String(), "deferred") {
				t.Fatal("throttle replay", response.Code, response.Body.String())
			}
			if api.count("alice", path) != before {
				t.Fatal("cooldown dispatched calendar HTTP")
			}
			until, err := f.routing.ProviderRetryUntil(t.Context(), "alice", f.accounts["alice"].ID)
			if err != nil || !until.After(time.Now()) {
				t.Fatal("calendar cooldown not durable", until, err)
			}
		})
	}
}

func TestUserCalendarDiscoveryHTTPRejectsForeignPaginationRedirectAndAccount(t *testing.T) {
	for _, operation := range []string{"pagination", "redirect"} {
		t.Run(operation, func(t *testing.T) {
			var leaked atomic.Int32
			foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1); w.WriteHeader(200) }))
			defer foreign.Close()
			f, api, _ := newOwnedCalendarDiscoveryFixture(t, "outlook")
			if operation == "pagination" {
				api.next = foreign.URL + "/me/calendars?token=foreign"
			} else {
				api.redirect = foreign.URL + "/me/calendars"
			}
			if response := calendarDiscoveryRequest(f, "alice"); response.Code != 200 || !strings.Contains(response.Body.String(), "failed") {
				t.Fatal("foreign URL accepted", response.Code, response.Body.String())
			}
			if leaked.Load() != 0 {
				t.Fatal("calendar credential leaked to foreign server")
			}
			before := api.count("alice", "/me/calendars")
			if response := f.request("alice", "POST", "/api/accounts/"+f.accounts["bob"].ID+"/calendar/discover", ""); response.Code != 404 {
				t.Fatal("foreign calendar account", response.Code)
			}
			if api.count("alice", "/me/calendars") != before {
				t.Fatal("foreign account dispatched provider")
			}
		})
	}
}

func TestUserCalendarDiscoveryHTTPWriterWaitRejectsChangedAuthorization(t *testing.T) {
	f, _, _ := newOwnedCalendarDiscoveryFixture(t, "outlook")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := f.routing.WithUser(ctx, "alice", func(db *storage.DB) error {
		tx, err := db.Write().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		before := db.Write().Stats().WaitCount
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- calendarDiscoveryRequest(f, "alice") }()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for db.Write().Stats().WaitCount == before {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
			}
		}
		// Reproduce a changed durable central grant after provider reads, while
		// the caller is still waiting for its local publication transaction.
		if _, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET revision=revision+1 WHERE account_id=?`, f.accounts["alice"].ID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		select {
		case response := <-done:
			if response.Code != 409 {
				t.Fatal("writer wait missed changed grant", response.Code, response.Body.String())
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		sources, err := db.ListCalendarSourcesForAccount(ctx, "alice", f.accounts["alice"].ID)
		if err == nil && (len(sources) != 1 || sources[0].Name != "alice old primary") {
			t.Fatal("late authorization published calendar catalog", sources)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
