package notifications

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Exercise actual managed routes and provider HTTP, including idempotent remote
// recovery when local acknowledgement or finalization fails after a DELETE.
type ownedContactDeleteAPI struct {
	provider          string
	mu                sync.Mutex
	calls             map[string]int
	statuses          map[string]int
	deleted           map[string]bool
	rejectOld, block  bool
	redirect          string
	entered, release  chan struct{}
	once, releaseOnce sync.Once
}

func (a *ownedContactDeleteAPI) unblock() { a.releaseOnce.Do(func() { close(a.release) }) }
func (a *ownedContactDeleteAPI) count(owner, number string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[owner+"|"+number]
}
func (a *ownedContactDeleteAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
	if a.provider == "carddav" {
		user, password, ok := r.BasicAuth()
		if !ok || password != user+"-dav-secret" {
			http.Error(w, "wrong owned DAV credentials", 401)
			return
		}
		owner = user
	}
	if owner != "alice" && owner != "bob" || r.Method != http.MethodDelete {
		http.Error(w, "wrong delete method or credential owner", 400)
		return
	}
	number := ""
	for _, candidate := range []string{"one", "two"} {
		path := "/people/" + candidate + ":deleteContact"
		if a.provider == "outlook" {
			path = "/me/contacts/same-" + candidate
		}
		if a.provider == "carddav" {
			path = "/books/" + owner + "/one/" + candidate + ".vcf"
		}
		if r.URL.Path == path {
			number = candidate
		}
	}
	if number == "" || r.URL.RawQuery != "" {
		http.Error(w, "wrong captured delete identity", 400)
		return
	}
	if a.provider == "outlook" && !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
		http.Error(w, "missing immutable Graph IDs", 400)
		return
	}
	if a.provider == "carddav" && r.Header.Get("If-Match") != owner+"-"+number+"-etag" {
		http.Error(w, "wrong captured DAV etag", 412)
		return
	}
	key := owner + "|" + number
	a.mu.Lock()
	a.calls[key]++
	status, gone := a.statuses[key], a.deleted[key]
	block, reject, redirect := a.block && owner == "alice", a.rejectOld, a.redirect
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
	if redirect != "" {
		http.Redirect(w, r, redirect, 302)
		return
	}
	if status == 429 {
		w.Header().Set("Retry-After", "120")
	}
	if status == 0 && gone {
		status = 404
	}
	if status != 0 {
		http.Error(w, "synthetic provider failure", status)
		return
	}
	a.mu.Lock()
	a.deleted[key] = true
	a.mu.Unlock()
	w.WriteHeader(204)
}

func newOwnedContactDeleteFixture(t *testing.T, provider string) (*userStorageFixture, *ownedContactDeleteAPI, *ownedContactPullAPI) {
	t.Helper()
	f, tokens, _ := newOwnedContactPullFixture(t, provider)
	api := &ownedContactDeleteAPI{provider: provider, calls: map[string]int{}, statuses: map[string]int{}, deleted: map[string]bool{}, entered: make(chan struct{}), release: make(chan struct{})}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	t.Cleanup(api.unblock)
	if provider == "carddav" {
		for _, owner := range []string{"alice", "bob"} {
			if err := f.accountStore.WithAccountForUser(t.Context(), owner, f.accounts[owner].ID, func(local *config.AccountStore, _ *storage.DB) error {
				return local.SaveContactSyncConfig(t.Context(), owner, f.accounts[owner].ID, models.ContactSyncConfig{Provider: provider, Enabled: true, Username: owner, BaseURL: server.URL, AddressBooks: []models.ContactAddressBook{{ID: owner + "-one", URL: server.URL + "/books/" + owner + "/one/", Default: true}}}, owner+"-dav-secret")
			}); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		previous := http.DefaultTransport
		target, _ := url.Parse(server.URL)
		http.DefaultTransport = ownedContactPullTransport{base: previous, target: target}
		t.Cleanup(func() { http.DefaultTransport = previous })
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			var cards []models.ContactCard
			var fields []models.ContactField
			for _, number := range []string{"one", "two"} {
				remote := "people/" + number
				book := ""
				if provider == "outlook" {
					remote = "same-" + number
				}
				if provider == "carddav" {
					remote, book = server.URL+"/books/"+owner+"/one/"+number+".vcf", owner+"-one"
				}
				cards = append(cards, models.ContactCard{ID: "same-delete-" + number, Kind: "provider", Provider: provider, AccountID: f.accounts[owner].ID, AddressBookID: book, RemoteID: remote, Etag: owner + "-" + number + "-etag", RawPayload: owner + "-" + number + "-raw", RawPayloadType: "retained/type", LastError: "retained error"})
				fields = append(fields, models.ContactField{ID: "same-delete-field-" + number, CardID: "same-delete-" + number, Kind: "phone", Value: owner + "-" + number, Source: "synced:" + f.accounts[owner].ID})
			}
			fields = append(fields, models.ContactField{ID: "same-delete-email", Kind: "email", Value: owner + "-delete@example.com", Source: "observed"})
			_, err := db.SaveContactProfile(t.Context(), owner, models.ContactProfile{ID: "same-delete-profile", PrimaryEmail: owner + "-delete@example.com", DisplayName: owner + " deletion", SyncEnabled: true, Cards: cards, Fields: fields})
			if err != nil {
				return err
			}
			if err := db.ReplaceContactSyncMemberships(t.Context(), owner, "same-delete-profile", []string{"local", "account:" + f.accounts[owner].ID}); err != nil {
				return err
			}
			if err := db.UpsertObservedContact(t.Context(), owner, owner+" deletion", owner+"-delete@example.com", time.Now()); err != nil {
				return err
			}
			_, err = db.EnqueueContactSyncOperation(t.Context(), owner, models.Contact{ID: "same-delete-profile", Email: owner + "-delete@example.com", GoferSyncEnabled: true}, nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, api, tokens
}

func requestContactDelete(f *userStorageFixture, owner string) *httptest.ResponseRecorder {
	return f.request(owner, http.MethodPost, "/api/contacts/same-delete-profile/delete", "")
}

func assertContactDeleteHTTPState(t *testing.T, f *userStorageFixture, owner string, cards int, deleted bool) {
	t.Helper()
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		var tombstone, gotCards, suppressed, queue, events int
		queries := []struct {
			query string
			dest  *int
		}{
			{`SELECT is_deleted FROM contact_profiles WHERE id='same-delete-profile'`, &tombstone},
			{`SELECT COUNT(*) FROM contact_cards WHERE profile_id='same-delete-profile' AND kind='provider'`, &gotCards},
			{`SELECT COUNT(*) FROM contact_observations WHERE profile_id='same-delete-profile' AND is_suppressed=1 AND suppress_auto_create=1`, &suppressed},
			{`SELECT COUNT(*) FROM contact_sync_operations WHERE contact_id='same-delete-profile' AND status IN ('pending','running')`, &queue},
			{`SELECT COUNT(*) FROM contact_activity_events WHERE event_type='contact_deleted'`, &events},
		}
		for _, q := range queries {
			if err := db.Read().QueryRow(q.query).Scan(q.dest); err != nil {
				return err
			}
		}
		if (tombstone == 1) != deleted || gotCards != cards || (suppressed == 1) != deleted || (events == 1) != deleted || (queue == 0) != deleted {
			t.Fatalf("%s deletion state: tombstone=%d cards=%d suppressed=%d queue=%d events=%d", owner, tombstone, gotCards, suppressed, queue, events)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserContactDeleteHTTPScopedRefreshAndOwnerIsolation(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "carddav"} {
		t.Run(provider, func(t *testing.T) {
			f, api, tokens := newOwnedContactDeleteFixture(t, provider)
			api.mu.Lock()
			api.rejectOld = true
			api.mu.Unlock()
			// Explicit deletion remains available when periodic contact sync is off.
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE account_contact_sync_configs SET enabled=0 WHERE account_id=?`, f.accounts["alice"].ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			response := requestContactDelete(f, "alice")
			if response.Code != 303 || response.Header().Get("Location") != "/contacts" {
				t.Fatal("delete response", response.Code, response.Body.String())
			}
			assertContactDeleteHTTPState(t, f, "alice", 0, true)
			assertContactDeleteHTTPState(t, f, "bob", 2, false)
			want := 1
			if provider != "carddav" {
				want = 2
			}
			wantTwo := 1
			if provider == "outlook" {
				wantTwo = 2
			}
			if api.count("alice", "one") != want || api.count("alice", "two") != wantTwo || api.count("bob", "one") != 0 {
				t.Fatal("wrong provider dispatch counts", api.count("alice", "one"), api.count("alice", "two"))
			}
			tokens.mu.Lock()
			refreshAlice, refreshBob := tokens.calls["alice|token"], tokens.calls["bob|token"]
			scopes := append([]string(nil), tokens.scopes...)
			tokens.mu.Unlock()
			wantRefresh := 1
			if provider == "outlook" {
				wantRefresh = 2 // Scoped refresh preserves the cached mailbox token.
			}
			if provider != "carddav" && (refreshAlice != wantRefresh || refreshBob != 0) {
				t.Fatal("wrong refresh owner", refreshAlice, refreshBob)
			}
			if provider == "outlook" {
				for _, scope := range scopes {
					if !strings.Contains(scope, "Contacts.ReadWrite") || strings.Contains(scope, "Mail.") {
						t.Fatal("unscoped contact refresh", scopes)
					}
				}
			}
			for _, table := range []string{"contact_profiles", "contact_cards", "contact_sync_operations", "contact_activity_events"} {
				var count int
				if err := f.system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatal("central fallback", table, count, err)
				}
			}
			if response := requestContactDelete(f, "alice"); response.Code != 404 {
				t.Fatal("repeated tombstone", response.Code)
			}
		})
	}
}

func TestUserContactDeleteHTTPPartialFailureRetainsExactRemainingSource(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "carddav"} {
		t.Run(provider, func(t *testing.T) {
			f, api, _ := newOwnedContactDeleteFixture(t, provider)
			api.mu.Lock()
			api.statuses["alice|two"] = 500
			api.mu.Unlock()
			response := requestContactDelete(f, "alice")
			if response.Code != 502 {
				t.Fatal("partial provider failure", response.Code, response.Body.String())
			}
			assertContactDeleteHTTPState(t, f, "alice", 1, false)
			assertContactDeleteHTTPState(t, f, "bob", 2, false)
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var raw, tag, field string
				if err := db.Read().QueryRow(`SELECT raw_payload,etag FROM contact_cards WHERE id='same-delete-two'`).Scan(&raw, &tag); err != nil {
					return err
				}
				if err := db.Read().QueryRow(`SELECT value FROM contact_fields WHERE id='same-delete-field-two'`).Scan(&field); err != nil {
					return err
				}
				if raw != "alice-two-raw" || tag != "alice-two-etag" || field != "alice-two" {
					t.Fatal("remaining source lost metadata", raw, tag, field)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			api.statuses["alice|two"] = 404
			if provider == "carddav" {
				api.statuses["alice|two"] = 410
			}
			api.mu.Unlock()
			if response := requestContactDelete(f, "alice"); response.Code != 303 {
				t.Fatal("partial retry", response.Code, response.Body.String())
			}
			assertContactDeleteHTTPState(t, f, "alice", 0, true)
			if api.count("alice", "one") != 1 || api.count("alice", "two") != 2 {
				t.Fatal("retry repeated acknowledged source")
			}
		})
	}
}

func TestUserContactDeleteHTTPFailedLocalCommitRecoversRemoteNotFound(t *testing.T) {
	for _, fault := range []string{"ack", "finish"} {
		t.Run(fault, func(t *testing.T) {
			f, api, _ := newOwnedContactDeleteFixture(t, "gmail")
			trigger := `BEFORE DELETE ON contact_cards WHEN OLD.id='same-delete-one'`
			if fault == "finish" {
				trigger = `BEFORE INSERT ON contact_activity_events WHEN NEW.event_type='contact_deleted'`
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`CREATE TRIGGER reject_http_delete ` + trigger + ` BEGIN SELECT RAISE(ABORT,'synthetic local delete failure'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if response := requestContactDelete(f, "alice"); response.Code != 503 || response.Header().Get("Location") != "" {
				t.Fatal("local failure claimed success", response.Code, response.Body.String())
			}
			wantCards := 2
			if fault == "finish" {
				wantCards = 0
			}
			assertContactDeleteHTTPState(t, f, "alice", wantCards, false)
			assertContactDeleteHTTPState(t, f, "bob", 2, false)
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error { _, err := db.Write().Exec(`DROP TRIGGER reject_http_delete`); return err }); err != nil {
				t.Fatal(err)
			}
			if _, err := f.system.Write().Exec(`CREATE TRIGGER reject_http_delete_wake BEFORE INSERT ON gofer_contact_queue_schedule BEGIN SELECT RAISE(ABORT,'synthetic wake failure'); END`); err != nil {
				t.Fatal(err)
			}
			if response := requestContactDelete(f, "alice"); response.Code != 303 {
				t.Fatal("recover known remote deletion", response.Code, response.Body.String())
			}
			assertContactDeleteHTTPState(t, f, "alice", 0, true)
			wantOne := 2
			if fault == "finish" {
				wantOne = 1
			}
			if api.count("alice", "one") != wantOne || api.count("alice", "two") != 1 {
				t.Fatal("wrong recovery provider calls")
			}
		})
	}
}

func TestUserContactDeleteHTTPBlockedProviderReleasesStoreAndRejectsChangedAuthority(t *testing.T) {
	for _, change := range []string{"account", "profile", "card"} {
		t.Run(change, func(t *testing.T) {
			f, api, _ := newOwnedContactDeleteFixture(t, "outlook")
			api.mu.Lock()
			api.block = true
			api.mu.Unlock()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- requestContactDelete(f, "alice") }()
			select {
			case <-api.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("delete provider not reached")
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, err := f.accountStore.SnapshotServices(ctx, "bob", f.accounts["bob"].ID); err != nil {
				t.Fatal("blocked delete pinned sole cache slot", err)
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				query := map[string]string{"account": `UPDATE accounts SET provider_account_id='replacement' WHERE id=?`, "profile": `UPDATE contact_profiles SET notes='intervening manual edit' WHERE id=?`, "card": `UPDATE contact_cards SET remote_id='replacement' WHERE id=?`}[change]
				id := f.accounts["alice"].ID
				if change == "profile" {
					id = "same-delete-profile"
				}
				if change == "card" {
					id = "same-delete-one"
				}
				_, err := db.Write().Exec(query, id)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			api.unblock()
			select {
			case response := <-done:
				if response.Code != 409 {
					t.Fatal("late delete acknowledged changed authority", response.Code, response.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("delete did not finish")
			}
			assertContactDeleteHTTPState(t, f, "alice", 2, false)
			if api.count("alice", "two") != 0 {
				t.Fatal("deleted next source after stale acknowledgement")
			}
		})
	}
}

func TestUserContactDeleteHTTPRootShutdownJoinsProviderWithoutRelease(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "carddav"} {
		t.Run(provider, func(t *testing.T) {
			f, api, _ := newOwnedContactDeleteFixture(t, provider)
			api.mu.Lock()
			api.block = true
			api.mu.Unlock()
			done := make(chan int, 1)
			go func() { done <- requestContactDelete(f, "alice").Code }()
			select {
			case <-api.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("delete not entered")
			}
			f.stopIMAP()
			drained := make(chan struct{})
			go func() { f.imap.Wait(); close(drained) }()
			select {
			case <-drained:
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown did not cancel/join delete")
			}
			select {
			case code := <-done:
				if code == 303 {
					t.Fatal("stopped delete claimed success")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("HTTP delete not joined")
			}
			assertContactDeleteHTTPState(t, f, "alice", 2, false)
		})
	}
}

func TestUserContactDeleteHTTPProviderRetryFence(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "carddav"} {
		t.Run(provider, func(t *testing.T) {
			f, api, _ := newOwnedContactDeleteFixture(t, provider)
			api.mu.Lock()
			api.statuses["alice|one"] = 429
			api.mu.Unlock()
			if response := requestContactDelete(f, "alice"); response.Code != 502 {
				t.Fatal("throttle", response.Code, response.Body.String())
			}
			until, err := f.routing.ProviderRetryUntil(t.Context(), "alice", f.accounts["alice"].ID)
			if err != nil || !until.After(time.Now()) {
				t.Fatal("missing durable retry hint", until, err)
			}
			api.mu.Lock()
			delete(api.statuses, "alice|one")
			api.mu.Unlock()
			if response := requestContactDelete(f, "alice"); response.Code == 303 {
				t.Fatal("deferred delete succeeded")
			}
			if api.count("alice", "one") != 1 || api.count("alice", "two") != 0 {
				t.Fatal("retry fence dispatched provider HTTP")
			}
			assertContactDeleteHTTPState(t, f, "alice", 2, false)
		})
	}
}

func TestUserContactDeleteHTTPDAVBoundaryRedirectAndPrecondition(t *testing.T) {
	for _, failure := range []string{"foreign-source", "redirect", "precondition"} {
		t.Run(failure, func(t *testing.T) {
			f, api, _ := newOwnedContactDeleteFixture(t, "carddav")
			var leaked atomic.Int32
			foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1); w.WriteHeader(204) }))
			defer foreign.Close()
			if failure == "foreign-source" {
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE contact_cards SET remote_id=? WHERE id='same-delete-one'`, foreign.URL+"/contact.vcf")
					return err
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				api.mu.Lock()
				if failure == "redirect" {
					api.redirect = foreign.URL + "/contact.vcf"
				} else {
					api.statuses["alice|one"] = 412
				}
				api.mu.Unlock()
			}
			response := requestContactDelete(f, "alice")
			if response.Code == 303 || leaked.Load() != 0 {
				t.Fatal("DAV failure acknowledged or credentials redirected", failure, response.Code, leaked.Load(), response.Body.String())
			}
			assertContactDeleteHTTPState(t, f, "alice", 2, false)
			assertContactDeleteHTTPState(t, f, "bob", 2, false)
			if api.count("alice", "two") != 0 {
				t.Fatal("continued after DAV failure")
			}
			if failure == "foreign-source" && api.count("alice", "one") != 0 {
				t.Fatal("foreign binding made provider request")
			}
		})
	}
}
