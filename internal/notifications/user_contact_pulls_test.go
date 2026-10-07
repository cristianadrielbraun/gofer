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
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

type ownedContactPullAPI struct {
	mu                                        sync.Mutex
	provider                                  string
	calls                                     map[string]int
	scopes                                    []string
	blockOwner, blockPath                     string
	entered, release                          chan struct{}
	once, releaseOnce                         sync.Once
	rejectOld, throttle, delta, invalidCursor bool
	badNext, redirect                         string
}

func (a *ownedContactPullAPI) unblock() { a.releaseOnce.Do(func() { close(a.release) }) }
func (a *ownedContactPullAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		_ = r.ParseForm()
		owner := strings.Split(r.FormValue("refresh_token"), "-")[0]
		a.mu.Lock()
		a.calls[owner+"|token"]++
		a.scopes = append(a.scopes, r.FormValue("scope"))
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		scope := userOutlookHTTPScopes + " https://graph.microsoft.com/Contacts.ReadWrite"
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": owner + "-fresh", "refresh_token": owner + "-rotated", "expires_in": 3600, "token_type": "Bearer", "scope": scope})
		return
	}
	owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
	if a.provider == "carddav" {
		user, password, ok := r.BasicAuth()
		if !ok || password != user+"-dav-secret" {
			http.Error(w, "wrong owned DAV credentials", 401)
			return
		}
		owner = user
	}
	wire, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	a.calls[owner+"|"+r.URL.Path]++
	block := owner == a.blockOwner && r.URL.Path == a.blockPath
	reject, throttle, delta, invalid, badNext, redirect := a.rejectOld, a.throttle, a.delta, a.invalidCursor, a.badNext, a.redirect
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
		http.Error(w, `{"access_token":"synthetic-secret"}`, 429)
		return
	}
	if a.provider == "carddav" {
		if strings.Contains(string(wire), "sync-collection") && invalid {
			http.Error(w, "invalid sync token", 403)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(207)
		book := "one"
		if strings.Contains(r.URL.Path, "/two/") {
			book = "two"
		}
		href := r.URL.Path + "person.vcf"
		token := owner + "-" + book + "-cursor"
		phone := owner + "-" + book + "-phone"
		if delta {
			token += "-new"
			phone += "-new"
		}
		if delta && book == "one" && strings.Contains(string(wire), "sync-collection") {
			_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:"><d:sync-token>%s</d:sync-token><d:response><d:href>%s</d:href><d:status>HTTP/1.1 404 Not Found</d:status></d:response></d:multistatus>`, token, href)
			return
		}
		card := fmt.Sprintf("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:%s %s\r\nEMAIL:%s-%s@contacts.test\r\nTEL:%s\r\nEND:VCARD\r\n", owner, book, owner, book, phone)
		// Include a harmless collection response, as real DAV implementations can.
		_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav"><d:sync-token>%s</d:sync-token><d:response><d:href>%s</d:href><d:propstat><d:prop/><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response><d:response><d:href>%s</d:href><d:propstat><d:prop><d:getetag>%s</d:getetag><c:address-data><![CDATA[%s]]></c:address-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, token, r.URL.Path, href, phone, card)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/photo/$value") {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("synthetic-photo-" + owner))
		return
	}
	if redirect != "" {
		http.Redirect(w, r, redirect, 302)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	page := "one"
	if r.URL.Query().Get("pageToken") == "two" || r.URL.Query().Get("$skiptoken") == "two" {
		page = "two"
	}
	phone := owner + "-" + page + "-phone"
	if delta {
		phone += "-new"
	}
	if a.provider == "gmail" {
		if r.URL.Path != "/people/me/connections" || r.URL.Query().Get("pageSize") != "1000" {
			http.Error(w, "wrong People collection", 400)
			return
		}
		result := map[string]any{"connections": []any{map[string]any{"resourceName": "people/" + page, "etag": owner + "-" + page, "names": []any{map[string]string{"displayName": owner + " " + page}}, "emailAddresses": []any{map[string]string{"value": owner + "-" + page + "@contacts.test"}}, "phoneNumbers": []any{map[string]string{"value": phone}}}, map[string]any{"resourceName": "people/no-email"}}}
		if page == "one" {
			result["nextPageToken"] = "two"
		}
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	if r.URL.Path != "/me/contacts" || !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
		http.Error(w, "wrong immutable Graph collection", 400)
		return
	}
	result := map[string]any{"value": []any{map[string]any{
		"id": "same-" + page, "changeKey": owner + "-" + page,
		"displayName":    owner + " " + page,
		"emailAddresses": []any{map[string]string{"address": owner + "-" + page + "@contacts.test"}},
		"businessPhones": []string{phone},
	}}}
	if page == "one" {
		result["@odata.nextLink"] = "https://graph.microsoft.com/v1.0/me/contacts?$skiptoken=two"
		if badNext != "" {
			result["@odata.nextLink"] = badNext
		}
	}
	_ = json.NewEncoder(w).Encode(result)
}

type ownedContactPullTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (t ownedContactPullTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "people.googleapis.com" && r.URL.Host != "graph.microsoft.com" {
		return t.base.RoundTrip(r)
	}
	copy := r.Clone(r.Context())
	address := *r.URL
	address.Scheme, address.Host = t.target.Scheme, t.target.Host
	address.Path = strings.TrimPrefix(strings.TrimPrefix(address.Path, "/v1.0"), "/v1")
	copy.URL = &address
	return t.base.RoundTrip(copy)
}

func newOwnedContactPullFixture(t *testing.T, provider string, oauthOverride ...*mailauth.Config) (*userStorageFixture, *ownedContactPullAPI, *httptest.Server) {
	t.Helper()
	api := &ownedContactPullAPI{provider: provider, calls: map[string]int{}, entered: make(chan struct{}), release: make(chan struct{})}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	t.Cleanup(api.unblock)
	previous := http.DefaultTransport
	address, _ := url.Parse(server.URL)
	http.DefaultTransport = ownedContactPullTransport{base: previous, target: address}
	t.Cleanup(func() { http.DefaultTransport = previous })
	client := &oauth2.Config{ClientID: "client", ClientSecret: "secret", Endpoint: oauth2.Endpoint{TokenURL: server.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}}
	oauth := &mailauth.Config{GoogleClient: client, MicrosoftClient: client}
	if len(oauthOverride) > 0 {
		oauth = oauthOverride[0]
	}
	f := newUserStorageFixtureConfigured(t, true, oauth)
	for _, owner := range []string{"alice", "bob"} {
		if provider == "carddav" {
			if err := f.accountStore.WithAccountForUser(t.Context(), owner, f.accounts[owner].ID, func(local *config.AccountStore, _ *storage.DB) error {
				return local.SaveContactSyncConfig(t.Context(), owner, f.accounts[owner].ID, models.ContactSyncConfig{Provider: "carddav", Enabled: true, Username: owner, BaseURL: server.URL, AddressBooks: []models.ContactAddressBook{{ID: owner + "-one", URL: server.URL + "/books/" + owner + "/one/", Default: true}, {ID: owner + "-two", URL: server.URL + "/books/" + owner + "/two/"}}}, owner+"-dav-secret")
			}); err != nil {
				t.Fatal(err)
			}
		} else {
			req := providers.GmailAccountRequest(owner+"@provider.test", owner, "same-subject")
			oauthProvider := "google"
			if provider == "outlook" {
				req = providers.OutlookAccountRequest(owner+"@provider.test", owner, "same-subject")
				oauthProvider = "microsoft"
			}
			account, err := f.accountStore.CreateAccount(t.Context(), owner, req)
			if err != nil {
				t.Fatal(err)
			}
			f.accounts[owner] = account
			expires := time.Now().Add(time.Hour)
			if err := f.credentials.UpsertForUser(t.Context(), owner, account.ID, oauthProvider, "same-subject", owner+"-access", owner+"-refresh", "Bearer", &expires, userOutlookHTTPScopes+" https://graph.microsoft.com/Contacts.ReadWrite"); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.accountStore.WithAccountForUser(t.Context(), owner, f.accounts[owner].ID, func(_ *config.AccountStore, db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE accounts SET email_sync_enabled=0 WHERE id=?`, f.accounts[owner].ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, api, server
}

func contactPullRequest(f *userStorageFixture, owner string) *httptest.ResponseRecorder {
	return f.request(owner, "POST", "/api/settings/contacts/accounts/sync", url.Values{"account_id": {f.accounts[owner].ID}}.Encode())
}

func assertOwnedPulledContacts(t *testing.T, f *userStorageFixture, owner, provider string) {
	t.Helper()
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		var count int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_cards WHERE user_id=? AND account_id=? AND provider=?`, owner, f.accounts[owner].ID, provider).Scan(&count); err != nil {
			return err
		}
		if count != 2 {
			t.Fatal("wrong owned card count", owner, count)
		}
		for _, page := range []string{"one", "two"} {
			profile, err := db.FindContactProfileByIdentity(t.Context(), owner, "email", owner+"-"+page+"@contacts.test")
			if err != nil {
				return err
			}
			if profile == nil {
				t.Fatal("missing provider profile", owner, page)
			}
			contact, err := db.GetContact(t.Context(), owner, profile.ID)
			if err != nil {
				return err
			}
			if contact == nil || contact.Phone != owner+"-"+page+"-phone" || (provider == "outlook" && !strings.HasPrefix(contact.AvatarURL, "data:image/png;base64,")) {
				t.Fatal("wrong scoped contact/photo", contact)
			}
		}
		var imported, success int
		var message string
		if err := db.Read().QueryRow(`SELECT last_import_count,last_success_at IS NOT NULL,last_error FROM account_contact_sync_configs WHERE account_id=?`, f.accounts[owner].ID).Scan(&imported, &success, &message); err != nil {
			return err
		}
		if imported != 2 || success != 1 || message != "" {
			t.Fatal("wrong success status", imported, success, message)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserContactPullHTTPAllProvidersOwnedPagesAndStatus(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "carddav"} {
		t.Run(provider, func(t *testing.T) {
			f, api, _ := newOwnedContactPullFixture(t, provider)
			for _, owner := range []string{"alice", "bob"} {
				rec := contactPullRequest(f, owner)
				if rec.Code != 200 || rec.Header().Get("X-Gofer-Status") == "error" || !strings.Contains(rec.Body.String(), "2 imported or updated") {
					t.Fatal("provider pull", owner, rec.Code, rec.Body.String())
				}
				assertOwnedPulledContacts(t, f, owner, provider)
			}
			before := 0
			api.mu.Lock()
			for _, count := range api.calls {
				before += count
			}
			api.mu.Unlock()
			foreign := f.request("bob", "POST", "/api/settings/contacts/accounts/sync", url.Values{"account_id": {f.accounts["alice"].ID}}.Encode())
			if foreign.Code != 404 {
				t.Fatal("foreign contact account accepted", foreign.Code, foreign.Body.String())
			}
			after := 0
			api.mu.Lock()
			for _, count := range api.calls {
				after += count
			}
			api.mu.Unlock()
			if after != before {
				t.Fatal("foreign pull reached provider")
			}
			for _, table := range []string{"contact_cards", "account_contact_sync_configs", "account_contact_address_books"} {
				var count int
				if err := f.system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatal("central contact copies", table, count, err)
				}
			}
		})
	}
}

func TestUserContactPullHTTPRefreshAndDurableCooldown(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api, _ := newOwnedContactPullFixture(t, provider)
			api.mu.Lock()
			api.rejectOld = true
			api.mu.Unlock()
			if rec := contactPullRequest(f, "alice"); rec.Code != 200 || rec.Header().Get("X-Gofer-Status") == "error" {
				t.Fatal("forced contacts refresh failed", rec.Body.String())
			}
			api.mu.Lock()
			tokens := api.calls["alice|token"]
			scopes := append([]string(nil), api.scopes...)
			api.throttle = true
			api.mu.Unlock()
			if tokens != 1 || (provider == "outlook" && (len(scopes) != 1 || scopes[0] != "https://graph.microsoft.com/Contacts.ReadWrite")) {
				t.Fatal("wrong scoped forced refresh", tokens, scopes)
			}
			if rec := contactPullRequest(f, "alice"); rec.Header().Get("X-Gofer-Status") != "error" || strings.Contains(rec.Body.String(), "synthetic-secret") {
				t.Fatal("throttle missing/redaction failed", rec.Body.String())
			}
			until, err := f.routing.ProviderRetryUntil(t.Context(), "alice", f.accounts["alice"].ID)
			if err != nil || time.Until(until) < time.Minute {
				t.Fatal("cooldown not persisted", until, err)
			}
			api.mu.Lock()
			before := 0
			for _, count := range api.calls {
				before += count
			}
			api.mu.Unlock()
			if rec := contactPullRequest(f, "alice"); rec.Header().Get("X-Gofer-Status") != "error" {
				t.Fatal("cooldown ignored")
			}
			api.mu.Lock()
			after := 0
			for _, count := range api.calls {
				after += count
			}
			api.mu.Unlock()
			if after != before {
				t.Fatal("cooldown reached provider")
			}
		})
	}
}

func TestUserContactPullHTTPGraphPaginationAndRedirectBoundary(t *testing.T) {
	for _, target := range []string{"https://foreign.test/v1.0/me/contacts", "https://graph.microsoft.com/v1.0/users/foreign/contacts", "https://graph.microsoft.com/v1.0/me/messages", "https://graph.microsoft.com/v1.0/me/contacts?$skiptoken=two#fragment"} {
		t.Run(target, func(t *testing.T) {
			f, api, _ := newOwnedContactPullFixture(t, "outlook")
			api.mu.Lock()
			api.badNext = target
			api.mu.Unlock()
			if rec := contactPullRequest(f, "alice"); rec.Header().Get("X-Gofer-Status") != "error" {
				t.Fatal("invalid page accepted", rec.Body.String())
			}
			api.mu.Lock()
			calls := api.calls["alice|/me/contacts"]
			api.mu.Unlock()
			if calls != 1 {
				t.Fatal("foreign page followed", calls)
			}
		})
	}
	f, api, _ := newOwnedContactPullFixture(t, "gmail")
	api.mu.Lock()
	api.redirect = "https://people.googleapis.com/v1/people/foreign/connections"
	api.mu.Unlock()
	if rec := contactPullRequest(f, "alice"); rec.Header().Get("X-Gofer-Status") != "error" {
		t.Fatal("foreign collection redirect followed", rec.Body.String())
	}
	api.mu.Lock()
	calls := api.calls["alice|/people/foreign/connections"]
	api.mu.Unlock()
	if calls != 0 {
		t.Fatal("redirect dispatched with scoped token")
	}
}

func TestUserContactPullHTTPDAVDeltaAndInvalidCursorFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprint(fallback), func(t *testing.T) {
			f, api, server := newOwnedContactPullFixture(t, "carddav")
			if rec := contactPullRequest(f, "alice"); rec.Header().Get("X-Gofer-Status") == "error" {
				t.Fatal("initial DAV pull", rec.Body.String())
			}
			api.mu.Lock()
			api.delta = true
			api.invalidCursor = fallback
			api.mu.Unlock()
			if rec := contactPullRequest(f, "alice"); rec.Header().Get("X-Gofer-Status") == "error" {
				t.Fatal("DAV delta/fallback", rec.Body.String())
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				first, err := db.GetContactSourceByRemoteID(t.Context(), "alice", "carddav", f.accounts["alice"].ID, server.URL+"/books/alice/one/person.vcf")
				if err != nil {
					return err
				}
				if (first != nil) != fallback {
					t.Fatal("wrong DAV deletion/fallback", first)
				}
				second, err := db.FindContactProfileByIdentity(t.Context(), "alice", "email", "alice-two@contacts.test")
				if err != nil {
					return err
				}
				contact, err := db.GetContact(t.Context(), "alice", second.ID)
				if err != nil {
					return err
				}
				if contact.Phone != "alice-two-phone-new" {
					t.Fatal("delta fields missing", contact)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			snapshot, err := f.accountStore.SnapshotServices(t.Context(), "alice", f.accounts["alice"].ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, book := range snapshot.ContactConfig().AddressBooks {
				if !strings.HasSuffix(book.LastSyncToken, "-cursor-new") {
					t.Fatal("DAV token not saved", book)
				}
			}
		})
	}
}

// Blocked real HTTP must leave the one-slot user-store cache available to Bob.
func TestUserContactPullHTTPBlockedRequestReleasesStoreAndRejectsLateSettings(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "carddav"} {
		t.Run(provider, func(t *testing.T) {
			f, api, _ := newOwnedContactPullFixture(t, provider)
			path := "/people/me/connections"
			if provider == "outlook" {
				path = "/me/contacts"
			}
			if provider == "carddav" {
				path = "/books/alice/one/"
			}
			api.mu.Lock()
			api.blockOwner, api.blockPath = "alice", path
			api.mu.Unlock()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- contactPullRequest(f, "alice") }()
			select {
			case <-api.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("provider not reached")
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, err := f.accountStore.SnapshotServices(ctx, "bob", f.accounts["bob"].ID); err != nil {
				t.Fatal("blocked provider pinned user store", err)
			}
			if rec := contactPullRequest(f, "bob"); rec.Header().Get("X-Gofer-Status") == "error" {
				t.Fatal("Bob blocked behind Alice", rec.Body.String())
			}
			if err := f.accountStore.WithAccountForUser(t.Context(), "alice", f.accounts["alice"].ID, func(_ *config.AccountStore, db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='replacement' WHERE id=?`, f.accounts["alice"].ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			api.unblock()
			select {
			case rec := <-done:
				if rec.Header().Get("X-Gofer-Status") != "error" {
					t.Fatal("late provider result accepted", rec.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("late pull did not finish")
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var count int
				err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_cards WHERE account_id=?`, f.accounts["alice"].ID).Scan(&count)
				if count != 0 {
					t.Fatal("late provider cards escaped", count)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserContactPullHTTPDAVThrottleDoesNotFallBackOrAdvanceCursor(t *testing.T) {
	f, api, _ := newOwnedContactPullFixture(t, "carddav")
	if rec := contactPullRequest(f, "alice"); rec.Header().Get("X-Gofer-Status") == "error" {
		t.Fatal(rec.Body.String())
	}
	before, err := f.accountStore.SnapshotServices(t.Context(), "alice", f.accounts["alice"].ID)
	if err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	calls := api.calls["alice|/books/alice/one/"]
	second := api.calls["alice|/books/alice/two/"]
	api.throttle = true
	api.mu.Unlock()
	if rec := contactPullRequest(f, "alice"); rec.Header().Get("X-Gofer-Status") != "error" || strings.Contains(rec.Body.String(), "synthetic-secret") {
		t.Fatal("DAV throttle/redaction", rec.Body.String())
	}
	api.mu.Lock()
	afterCalls, afterSecond := api.calls["alice|/books/alice/one/"], api.calls["alice|/books/alice/two/"]
	api.mu.Unlock()
	if afterCalls != calls+1 || afterSecond != second {
		t.Fatal("throttled DAV made fallback/next-book requests", calls, afterCalls, second, afterSecond)
	}
	until, err := f.routing.ProviderRetryUntil(t.Context(), "alice", f.accounts["alice"].ID)
	if err != nil || time.Until(until) < time.Minute {
		t.Fatal("DAV cooldown missing", until, err)
	}
	after, err := f.accountStore.SnapshotServices(t.Context(), "alice", f.accounts["alice"].ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, book := range before.ContactConfig().AddressBooks {
		if after.ContactConfig().AddressBooks[i].LastSyncToken != book.LastSyncToken {
			t.Fatal("throttled cursor advanced")
		}
	}
}

func TestUserContactPullHTTPPublicationFailureAndRetry(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "carddav"} {
		t.Run(provider, func(t *testing.T) {
			f, api, _ := newOwnedContactPullFixture(t, provider)
			trigger := `CREATE TRIGGER fail_owned_provider_card BEFORE INSERT ON contact_cards WHEN NEW.remote_id IN ('people/two','same-two') BEGIN SELECT RAISE(ABORT,'injected page failure'); END`
			if provider == "carddav" {
				trigger = `CREATE TRIGGER fail_owned_provider_card BEFORE UPDATE ON account_contact_address_books BEGIN SELECT RAISE(ABORT,'injected cursor failure'); END`
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error { _, err := db.Write().Exec(trigger); return err }); err != nil {
				t.Fatal(err)
			}
			if rec := contactPullRequest(f, "alice"); rec.Header().Get("X-Gofer-Status") != "error" {
				t.Fatal("failed publication succeeded", rec.Body.String())
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var count, success int
				var message string
				if err := db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM contact_cards WHERE account_id=?),last_success_at IS NOT NULL,last_error FROM account_contact_sync_configs WHERE account_id=?`, f.accounts["alice"].ID, f.accounts["alice"].ID).Scan(&count, &success, &message); err != nil {
					return err
				}
				want := 1
				if provider == "carddav" {
					want = 0
				}
				if count != want || success != 0 || !strings.Contains(message, "injected") {
					t.Fatal("partial failed page/cursor escaped", count, success, message)
				}
				_, err := db.Write().Exec(`DROP TRIGGER fail_owned_provider_card`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if provider == "carddav" {
				api.mu.Lock()
				second := api.calls["alice|/books/alice/two/"]
				api.mu.Unlock()
				if second != 0 {
					t.Fatal("next DAV book ran after failed checkpoint")
				}
			}
			if rec := contactPullRequest(f, "alice"); rec.Header().Get("X-Gofer-Status") == "error" {
				t.Fatal("provider retry failed", rec.Body.String())
			}
			assertOwnedPulledContacts(t, f, "alice", provider)
		})
	}
}

func TestUserContactPullHTTPAccountEditDeletionDisabledOwnerAndShutdownCancelNetwork(t *testing.T) {
	for _, change := range []string{"account-edit", "account-deletion", "disabled-owner", "shutdown"} {
		t.Run(change, func(t *testing.T) {
			f, api, _ := newOwnedContactPullFixture(t, "gmail")
			api.mu.Lock()
			api.blockOwner, api.blockPath = "alice", "/people/me/connections"
			api.mu.Unlock()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- contactPullRequest(f, "alice") }()
			select {
			case <-api.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("network not reached")
			}
			switch change {
			case "account-edit":
				if err := f.imap.RestartAccount(t.Context(), f.accounts["alice"].ID); err != nil {
					t.Fatal(err)
				}
			case "account-deletion":
				if err := f.routing.RequestAccountDeletion(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
					t.Fatal(err)
				}
			case "disabled-owner":
				if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				f.stopIMAP()
			}
			select {
			case rec := <-done:
				if rec.Header().Get("X-Gofer-Status") != "error" {
					t.Fatal("cancelled pull succeeded", rec.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancel did not release real provider request")
			}
			if change == "disabled-owner" {
				if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var count int
				err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_cards WHERE account_id=?`, f.accounts["alice"].ID).Scan(&count)
				if count != 0 {
					t.Fatal("cancelled cards escaped")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if change == "shutdown" {
				joined := make(chan struct{})
				go func() { f.imap.Wait(); close(joined) }()
				select {
				case <-joined:
				case <-time.After(5 * time.Second):
					t.Fatal("shutdown did not join contact operation")
				}
			}
		})
	}
}

func TestUserContactPullHTTPOwnerWideSelectionAndDisabledContacts(t *testing.T) {
	f, api, _ := newOwnedContactPullFixture(t, "gmail")
	if rec := f.request("alice", "POST", "/api/settings/contacts/providers/gmail/sync", ""); rec.Header().Get("X-Gofer-Status") == "error" || !strings.Contains(rec.Body.String(), "2 imported or updated") {
		t.Fatal("owner-wide provider selection", rec.Body.String())
	}
	api.mu.Lock()
	bobCalls := api.calls["bob|/people/me/connections"]
	api.mu.Unlock()
	if bobCalls != 0 {
		t.Fatal("owner-wide sync crossed owners")
	}
	snapshot, err := f.accountStore.SnapshotServices(t.Context(), "alice", f.accounts["alice"].ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := snapshot.ContactConfig()
	cfg.Enabled = false
	if err := f.accountStore.SaveContactServices(t.Context(), snapshot, cfg, ""); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	before := api.calls["alice|/people/me/connections"]
	api.mu.Unlock()
	if rec := contactPullRequest(f, "alice"); rec.Header().Get("X-Gofer-Status") != "error" {
		t.Fatal("disabled contact account selected")
	}
	api.mu.Lock()
	after := api.calls["alice|/people/me/connections"]
	api.mu.Unlock()
	if before != after {
		t.Fatal("disabled contacts made network calls")
	}
}

func TestUserContactPullHTTPLateThrottleDoesNotPublishNewIdentityCooldown(t *testing.T) {
	f, api, _ := newOwnedContactPullFixture(t, "gmail")
	api.mu.Lock()
	api.throttle = true
	api.blockOwner, api.blockPath = "alice", "/people/me/connections"
	api.mu.Unlock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- contactPullRequest(f, "alice") }()
	select {
	case <-api.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked throttle not reached")
	}
	if err := f.accountStore.WithAccountForUser(t.Context(), "alice", f.accounts["alice"].ID, func(_ *config.AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='replacement' WHERE id=?`, f.accounts["alice"].ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	api.unblock()
	select {
	case rec := <-done:
		if rec.Header().Get("X-Gofer-Status") != "error" {
			t.Fatal("late throttle accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late throttle did not finish")
	}
	until, err := f.routing.ProviderRetryUntil(t.Context(), "alice", f.accounts["alice"].ID)
	if err != nil || !until.IsZero() {
		t.Fatal("old identity throttle delayed replacement", until, err)
	}
}

func TestUserContactPullHTTPInboundChangeQueuesOwnedFanoutOnce(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "carddav"} {
		t.Run(provider, func(t *testing.T) {
			f, api, _ := newOwnedContactPullFixture(t, provider)
			if rec := contactPullRequest(f, "alice"); rec.Header().Get("X-Gofer-Status") == "error" {
				t.Fatal("initial pull", rec.Body.String())
			}
			target, err := f.accountStore.CreateAccount(t.Context(), "alice", providers.GmailAccountRequest("target@provider.test", "target", "target-subject"))
			if err != nil {
				t.Fatal(err)
			}
			var profile string
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				contact, err := db.FindContactProfileByIdentity(t.Context(), "alice", "email", "alice-two@contacts.test")
				if err != nil {
					return err
				}
				profile = contact.ID
				if err := db.InitializeContactCanonicalFields(t.Context(), "alice", profile, nil); err != nil {
					return err
				}
				if err := db.ReplaceContactSyncMemberships(t.Context(), "alice", profile, []string{"account:" + f.accounts["alice"].ID, "account:" + target.ID}); err != nil {
					return err
				}
				return db.SetContactProfileSyncEnabled(t.Context(), "alice", profile, true)
			}); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			api.delta = true
			api.mu.Unlock()
			for repeat := 0; repeat < 2; repeat++ {
				if rec := contactPullRequest(f, "alice"); rec.Header().Get("X-Gofer-Status") == "error" {
					t.Fatal("inbound change/readback", rec.Body.String())
				}
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var count int
				var payload string
				if err := db.Read().QueryRow(`SELECT COUNT(*),MAX(payload_json) FROM contact_sync_operations WHERE user_id='alice' AND contact_id=?`, profile).Scan(&count, &payload); err != nil {
					return err
				}
				if count != 1 {
					t.Fatal("fanout absent or unchanged readback echoed", count)
				}
				var queued storage.ContactSyncOperationPayload
				if err := json.Unmarshal([]byte(payload), &queued); err != nil {
					return err
				}
				if queued.ExcludedAccountID != f.accounts["alice"].ID || queued.Contact.ID != profile {
					t.Fatal("wrong owned fanout", queued)
				}
				contact, err := db.GetContact(t.Context(), "alice", profile)
				if err != nil {
					return err
				}
				if contact.Phone != "alice-two-phone-new" {
					t.Fatal("canonical provider change lost", contact)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserContactPullHTTPDAVSettingChangeBetweenBooksRejectsOnlyLateBook(t *testing.T) {
	f, api, server := newOwnedContactPullFixture(t, "carddav")
	api.mu.Lock()
	api.blockOwner, api.blockPath = "alice", "/books/alice/two/"
	api.mu.Unlock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- contactPullRequest(f, "alice") }()
	select {
	case <-api.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("second book not reached")
	}
	if err := f.accountStore.WithAccountForUser(t.Context(), "alice", f.accounts["alice"].ID, func(_ *config.AccountStore, db *storage.DB) error {
		var firstToken string
		if err := db.Read().QueryRow(`SELECT last_sync_token FROM account_contact_address_books WHERE id='alice-one'`).Scan(&firstToken); err != nil {
			return err
		}
		if firstToken != "alice-one-cursor" {
			t.Fatal("first book was not checkpointed before next HTTP call", firstToken)
		}
		_, err := db.Write().Exec(`UPDATE account_contact_address_books SET url=? WHERE id='alice-two'`, server.URL+"/books/alice/replacement/")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	api.unblock()
	select {
	case rec := <-done:
		if rec.Header().Get("X-Gofer-Status") != "error" {
			t.Fatal("late second book accepted", rec.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late second book did not finish")
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		var cards int
		var secondToken, secondURL string
		if err := db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM contact_cards WHERE account_id=?),last_sync_token,url FROM account_contact_address_books WHERE id='alice-two'`, f.accounts["alice"].ID).Scan(&cards, &secondToken, &secondURL); err != nil {
			return err
		}
		if cards != 1 || secondToken != "" || secondURL != server.URL+"/books/alice/replacement/" {
			t.Fatal("late book/cursor/settings escaped", cards, secondToken, secondURL)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserContactPullHTTPMissingProviderConfigurationDoesNotUseCachedToken(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api, _ := newOwnedContactPullFixture(t, provider, &mailauth.Config{})
			if rec := contactPullRequest(f, "alice"); rec.Header().Get("X-Gofer-Status") != "error" || !strings.Contains(rec.Body.String(), "OAuth is not configured") {
				t.Fatal("unconfigured provider used cached authorization", rec.Body.String())
			}
			api.mu.Lock()
			calls := len(api.calls)
			api.mu.Unlock()
			if calls != 0 {
				t.Fatal("unconfigured provider reached network", calls)
			}
		})
	}
}
