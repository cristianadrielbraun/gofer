package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
	"golang.org/x/oauth2"
)

type userContactPushTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (x userContactPushTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	u := *r.URL
	if u.Host == "people.googleapis.com" || u.Host == "graph.microsoft.com" {
		u.Scheme, u.Host = x.target.Scheme, x.target.Host
		u.Path = strings.TrimPrefix(strings.TrimPrefix(u.Path, "/v1.0"), "/v1")
		u.RawPath = strings.TrimPrefix(strings.TrimPrefix(u.RawPath, "/v1.0"), "/v1")
	}
	copy.URL = &u
	return x.base.RoundTrip(copy)
}

func TestUserContactPushGoogleOwnedCreateThenUpdate(t *testing.T) {
	var creates, updates, searches int
	var mu sync.Mutex
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer access" {
			t.Error("wrong token")
			http.Error(w, "wrong token", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/people:searchContacts":
			searches++
			_, _ = w.Write([]byte(`{"results":[]}`))
		case "/people:createContact":
			creates++
			var person googlePerson
			if err := json.NewDecoder(r.Body).Decode(&person); err != nil {
				t.Error(err)
			}
			if len(person.PhoneNumbers) != 1 || person.PhoneNumbers[0].Value != "222" {
				t.Errorf("stale contact %+v", person)
			}
			_, _ = w.Write([]byte(`{"resourceName":"people/created","etag":"v1"}`))
		case "/people/created:updateContact":
			updates++
			var person googlePerson
			_ = json.NewDecoder(r.Body).Decode(&person)
			if person.Etag != "v1" {
				t.Error("missing current ETag")
			}
			_, _ = w.Write([]byte(`{"resourceName":"people/created","etag":"v2"}`))
		default:
			http.Error(w, "unexpected provider path", 400)
		}
	}))
	defer api.Close()
	previous := http.DefaultTransport
	target, _ := url.Parse(api.URL)
	http.DefaultTransport = userContactPushTransport{base: previous, target: target}
	defer func() { http.DefaultTransport = previous }()
	system, err := storage.New(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer system.Close()
	if _, err := system.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('alice','alice','alice')`); err != nil {
		t.Fatal(err)
	}
	stores, err := storage.NewUserStores(system, storage.UserStoreOptions{MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer stores.Close(context.Background())
	routing, err := storage.NewAccountRouting(stores)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	accounts, err := config.NewUserAccountStore(routing, key)
	if err != nil {
		t.Fatal(err)
	}
	account, err := accounts.CreateAccount(t.Context(), "alice", providers.GmailAccountRequest("alice@example.com", "alice", "subject"))
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.WithAccountForUser(t.Context(), "alice", account.ID, func(_ *config.AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET email_sync_enabled=0 WHERE id=?`, account.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	imap, err := mail.NewUserIMAP(ctx, accounts, store.NewBlobStore(t.TempDir()), mail.NewSyncOrchestrator(system, nil, nil, nil).Events())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); imap.Wait() }()
	credentials, err := mailauth.NewUserCredentials(ctx, &mailauth.Config{GoogleClient: &oauth2.Config{ClientID: "client", Endpoint: oauth2.Endpoint{TokenURL: api.URL + "/token"}}}, routing, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); credentials.Wait() }()
	expires := time.Now().Add(time.Hour)
	if err := credentials.UpsertForUser(ctx, "alice", account.ID, "google", "subject", "access", "refresh", "Bearer", &expires, "https://www.googleapis.com/auth/contacts"); err != nil {
		t.Fatal(err)
	}
	h := &Handler{userStorage: routing, userAccounts: accounts, userIMAP: imap, userCredentials: credentials}
	var profile string
	seed := func() {
		if err := accounts.WithUser(ctx, "alice", func(_ *config.AccountStore, db *storage.DB) error {
			if profile == "" {
				contact, err := db.SaveContact(ctx, "alice", models.Contact{Email: "person@example.com", Phone: "222", GoferSyncEnabled: true, SaveTargets: []string{"account:" + account.ID}})
				if err != nil {
					return err
				}
				profile = contact.ID
			}
			_, err := db.EnqueueContactSyncOperation(ctx, "alice", models.Contact{ID: profile, Email: "stale@example.com", Phone: "111"}, nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed()
	if count, err := h.ProcessUserContactSyncOperations(ctx, "alice", 1); err != nil || count != 1 {
		t.Fatal("create job", count, err)
	}
	seed()
	if count, err := h.ProcessUserContactSyncOperations(ctx, "alice", 1); err != nil || count != 1 {
		t.Fatal("update job", count, err)
	}
	mu.Lock()
	gotCreates, gotUpdates, gotSearches := creates, updates, searches
	mu.Unlock()
	if gotCreates != 1 || gotUpdates != 1 || gotSearches != 1 {
		t.Fatal("wrong provider calls", gotCreates, gotUpdates, gotSearches)
	}
	if err := accounts.WithUser(ctx, "alice", func(_ *config.AccountStore, db *storage.DB) error {
		source, err := db.GetContactSource(ctx, "alice", profile, "gmail", account.ID)
		if err != nil {
			return err
		}
		if source == nil || source.Etag != "v2" {
			t.Fatal("missing acknowledged source", source)
		}
		var count int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations WHERE status='done'`).Scan(&count); err != nil {
			return err
		}
		if count != 2 {
			t.Fatal("jobs not completed", count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserContactPushResourceIDsStayWithinProviderCollection(t *testing.T) {
	for _, id := range []string{"people/a", "people/c123", "people/A_b-9"} {
		if !validOwnedGoogleContactID(id) {
			t.Fatal("valid People id rejected", id)
		}
	}
	for _, id := range []string{"", "people/", "people/..", "people/%2e%2e", "people/a/b", "people/a?x=y", "people/a#fragment", "https://other.test/people/a"} {
		if validOwnedGoogleContactID(id) {
			t.Fatal("invalid People id accepted", id)
		}
	}
	for _, id := range []string{"", " ", ".", ".."} {
		if validOwnedGraphContactID(id) {
			t.Fatal("invalid Graph id accepted", id)
		}
	}
}

type userContactPushFixture struct {
	h        *Handler
	system   *storage.DB
	accounts map[string]*models.Account
	server   *httptest.Server
	cancel   context.CancelFunc
}

func newUserContactPushFixture(t *testing.T, provider string, api http.Handler) *userContactPushFixture {
	t.Helper()
	f := &userContactPushFixture{accounts: map[string]*models.Account{}, server: httptest.NewServer(api)}
	t.Cleanup(f.server.Close)
	previous := http.DefaultTransport
	target, _ := url.Parse(f.server.URL)
	http.DefaultTransport = userContactPushTransport{base: previous, target: target}
	t.Cleanup(func() { http.DefaultTransport = previous })
	var err error
	f.system, err = storage.New(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.system.Close() })
	for _, owner := range []string{"alice", "bob"} {
		if _, err := f.system.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES(?,?,?)`, owner, owner, owner); err != nil {
			t.Fatal(err)
		}
	}
	stores, err := storage.NewUserStores(f.system, storage.UserStoreOptions{MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := stores.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	routing, err := storage.NewAccountRouting(stores)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	accounts, err := config.NewUserAccountStore(routing, key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	f.cancel = cancel
	imap, err := mail.NewUserIMAP(ctx, accounts, store.NewBlobStore(t.TempDir()), mail.NewSyncOrchestrator(f.system, nil, nil, nil).Events())
	if err != nil {
		t.Fatal(err)
	}
	client := &oauth2.Config{ClientID: "client", ClientSecret: "secret", Endpoint: oauth2.Endpoint{TokenURL: f.server.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}}
	credentials, err := mailauth.NewUserCredentials(ctx, &mailauth.Config{GoogleClient: client, MicrosoftClient: client}, routing, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); imap.Wait(); credentials.Wait() })
	f.h = &Handler{userStorage: routing, userAccounts: accounts, userIMAP: imap, userCredentials: credentials}
	for _, owner := range []string{"alice", "bob"} {
		req := providers.GmailAccountRequest(owner+"@example.com", owner, "subject")
		oauthProvider := "google"
		if provider == "outlook" {
			req = providers.OutlookAccountRequest(owner+"@example.com", owner, "subject")
			oauthProvider = "microsoft"
		}
		if provider == "carddav" {
			req = &models.CreateAccountRequest{Provider: "imap", EmailAddress: owner + "@example.com", DisplayName: owner, IMAPHost: "imap.example.com", SMTPHost: "smtp.example.com", Username: owner, Password: "synthetic"}
		}
		account, err := accounts.CreateAccount(ctx, owner, req)
		if err != nil {
			t.Fatal(err)
		}
		f.accounts[owner] = account
		if err := accounts.WithAccountForUser(ctx, owner, account.ID, func(local *config.AccountStore, db *storage.DB) error {
			if _, err := db.Write().Exec(`UPDATE accounts SET email_sync_enabled=0 WHERE id=?`, account.ID); err != nil {
				return err
			}
			if provider == "carddav" {
				return local.SaveContactSyncConfig(ctx, owner, account.ID, models.ContactSyncConfig{Enabled: true, Provider: "carddav", Username: owner, BaseURL: f.server.URL, AddressBooks: []models.ContactAddressBook{{ID: owner + "-book", URL: f.server.URL + "/books/" + owner + "/", Default: true}}}, owner+"-password")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if provider != "carddav" {
			expires := time.Now().Add(time.Hour)
			if err := credentials.UpsertForUser(ctx, owner, account.ID, oauthProvider, "subject", owner+"-access", owner+"-refresh", "Bearer", &expires, "https://www.googleapis.com/auth/contacts https://graph.microsoft.com/Contacts.ReadWrite"); err != nil {
				t.Fatal(err)
			}
		}
	}
	return f
}
func seedUserContactPush(t *testing.T, f *userContactPushFixture, owner, provider string) string {
	t.Helper()
	id := "shared-profile"
	target := "account:" + f.accounts[owner].ID
	if provider == "carddav" {
		target = "book:" + owner + "-book"
	}
	if err := f.h.userAccounts.WithUser(t.Context(), owner, func(_ *config.AccountStore, db *storage.DB) error {
		if _, err := db.SaveContact(t.Context(), owner, models.Contact{ID: id, Email: "same@example.com", Name: owner, Phone: owner, PhoneLabel: "work", GoferSyncEnabled: true, SaveTargets: []string{target}}); err != nil {
			return err
		}
		_, err := db.EnqueueContactSyncOperation(t.Context(), owner, models.Contact{ID: id, Email: "stale@example.com", Phone: "stale"}, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}
func TestUserContactPushOutlookAndDAVOwnedCreateThenUpdate(t *testing.T) {
	for _, provider := range []string{"outlook", "carddav"} {
		t.Run(provider, func(t *testing.T) {
			var mu sync.Mutex
			calls := map[string]int{}
			cards := map[string]string{}
			api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
				if provider == "carddav" {
					username, password, ok := r.BasicAuth()
					owner = username
					if !ok || password != owner+"-password" {
						http.Error(w, "wrong DAV credentials", 401)
						return
					}
				}
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				defer mu.Unlock()
				calls[owner+"|"+r.Method]++
				if provider == "outlook" {
					w.Header().Set("Content-Type", "application/json")
					if r.Header.Get("Prefer") != `IdType="ImmutableId"` {
						t.Error("Graph immutable ID missing")
					}
					switch r.Method {
					case "GET":
						if r.URL.Path != "/me/contacts" || r.URL.Query().Get("$filter") == "" {
							t.Error("wrong preflight query")
						}
						_, _ = w.Write([]byte(`{"value":[]}`))
					case "POST", "PATCH":
						var payload outlookContactPayload
						if err := json.Unmarshal(body, &payload); err != nil {
							t.Error(err)
						}
						if payload.DisplayName != owner || len(payload.BusinessPhones) != 1 || payload.BusinessPhones[0] != owner {
							t.Error("stale provider payload")
						}
						_, _ = w.Write([]byte(`{"id":"shared-remote","changeKey":"` + r.Method + `"}`))
					default:
						http.Error(w, "unexpected method", 400)
					}
					return
				}
				switch r.Method {
				case "REPORT":
					w.Header().Set("Content-Type", "application/xml")
					w.WriteHeader(207)
					_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>%s</d:href><d:status>HTTP/1.1 404 Not Found</d:status></d:response></d:multistatus>`, r.URL.Path+"missing.vcf")
				case "PUT":
					if !strings.HasPrefix(r.URL.Path, "/books/"+owner+"/") {
						t.Error("wrong owned book")
					}
					payload, err := parseVCardContacts(strings.NewReader(string(body)), nil)
					if err != nil || len(payload) != 1 || payload[0].Name != owner || payload[0].Phone != owner {
						t.Error("stale DAV payload", payload, err)
					}
					existing := cards[r.URL.Path]
					if existing == "" && r.Header.Get("If-None-Match") != "*" {
						t.Error("missing create condition")
					}
					if existing != "" && r.Header.Get("If-Match") != "v1" {
						t.Error("missing update condition")
					}
					cards[r.URL.Path] = string(body)
					w.Header().Set("ETag", "v1")
					w.WriteHeader(201)
				default:
					http.Error(w, "unexpected DAV method", 400)
				}
			})
			f := newUserContactPushFixture(t, provider, api)
			for _, owner := range []string{"alice", "bob"} {
				for i := 0; i < 2; i++ {
					id := seedUserContactPush(t, f, owner, provider)
					if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), owner, 1); err != nil || n != 1 {
						t.Fatal("owned job", n, err)
					}
					if err := f.h.userAccounts.WithUser(t.Context(), owner, func(_ *config.AccountStore, db *storage.DB) error {
						source, err := db.GetContactSource(t.Context(), owner, id, provider, f.accounts[owner].ID)
						if err != nil {
							return err
						}
						if source == nil {
							t.Fatal("source not acknowledged")
						}
						if provider == "outlook" && source.RemoteID != "shared-remote" {
							t.Fatal("wrong remote")
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			mu.Lock()
			defer mu.Unlock()
			for _, owner := range []string{"alice", "bob"} {
				if provider == "outlook" && (calls[owner+"|GET"] != 1 || calls[owner+"|POST"] != 1 || calls[owner+"|PATCH"] != 1) {
					t.Fatal("wrong Graph calls", calls)
				}
				if provider == "carddav" && calls[owner+"|PUT"] != 2 {
					t.Fatal("wrong DAV calls", calls)
				}
			}
		})
	}
}

func TestUserContactPushPreflightAliasReboundDuringRefreshStopsWrite(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			var f *userContactPushFixture
			var mu sync.Mutex
			var refreshedWrites, refreshes int
			remote := "people/existing"
			if provider == "outlook" {
				remote = "existing"
			}
			api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/token" {
					_ = r.ParseForm()
					if r.FormValue("refresh_token") != "alice-refresh" {
						t.Error("wrong refresh identity")
					}
					if err := f.h.userAccounts.WithUser(r.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
						contact, err := db.SaveContact(r.Context(), "alice", models.Contact{ID: "other-profile", Email: "other@example.com"})
						if err != nil {
							return err
						}
						return db.UpsertContactSource(r.Context(), storage.ContactSource{UserID: "alice", ContactID: contact.ID, Provider: provider, AccountID: f.accounts["alice"].ID, RemoteID: remote, Etag: "other-version"})
					}); err != nil {
						t.Error(err)
						http.Error(w, "fixture mutation failed", 500)
						return
					}
					mu.Lock()
					refreshes++
					mu.Unlock()
					_, _ = w.Write([]byte(`{"access_token":"alice-fresh","refresh_token":"alice-rotated","expires_in":3600,"token_type":"Bearer","scope":"https://www.googleapis.com/auth/contacts https://graph.microsoft.com/Contacts.ReadWrite"}`))
					return
				}
				if r.Method == "PATCH" {
					if r.Header.Get("Authorization") == "Bearer alice-access" {
						http.Error(w, "expired", 401)
						return
					}
					mu.Lock()
					refreshedWrites++
					mu.Unlock()
					http.Error(w, "late write must not reach provider", 500)
					return
				}
				if r.Method == "GET" {
					if provider == "gmail" {
						_, _ = w.Write([]byte(`{"results":[{"person":{"resourceName":"people/existing","etag":"v1","emailAddresses":[{"value":"same@example.com"}]}}]}`))
					} else {
						_, _ = w.Write([]byte(`{"value":[{"id":"existing","changeKey":"v1","emailAddresses":[{"address":"same@example.com"}]}]}`))
					}
					return
				}
				http.Error(w, "unexpected request", 400)
			})
			f = newUserContactPushFixture(t, provider, api)
			profile := seedUserContactPush(t, f, "alice", provider)
			if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); err == nil || n != 1 {
				t.Fatal("late alias accepted", n, err)
			}
			mu.Lock()
			r, w := refreshes, refreshedWrites
			mu.Unlock()
			if r != 1 || w != 0 {
				t.Fatal("wrong refresh/write count", r, w)
			}
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				source, err := db.GetContactSource(t.Context(), "alice", profile, provider, f.accounts["alice"].ID)
				if err != nil {
					return err
				}
				if source != nil {
					t.Fatal("late source published", source)
				}
				var status string
				if err := db.Read().QueryRow(`SELECT status FROM contact_sync_operations WHERE contact_id=?`, profile).Scan(&status); err != nil {
					return err
				}
				if status != "pending" {
					t.Fatal("late attempt not recoverable", status)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserContactPushDAVFailedAcknowledgementRecoveryAndConflictVersion(t *testing.T) {
	for _, retainedMissingSource := range []bool{false, true} {
		t.Run(fmt.Sprintf("retained-missing-source=%t", retainedMissingSource), func(t *testing.T) {
			var f *userContactPushFixture
			var mu sync.Mutex
			cards := map[string]string{}
			var puts int
			conflict := false
			api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, ok := r.BasicAuth()
				if !ok || user != "alice" || password != "alice-password" {
					http.Error(w, "wrong credentials", 401)
					return
				}
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				defer mu.Unlock()
				switch r.Method {
				case "REPORT":
					_, href, found := strings.Cut(string(body), "<d:href>")
					if !found {
						t.Error("no requested DAV resource")
						return
					}
					href, _, _ = strings.Cut(href, "</d:href>")
					u, err := url.Parse(href)
					if err != nil {
						t.Error(err)
						return
					}
					w.Header().Set("Content-Type", "application/xml")
					w.WriteHeader(207)
					if card := cards[u.Path]; card != "" {
						_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav"><d:response><d:href>%s</d:href><d:propstat><d:prop><d:getetag>v2</d:getetag><c:address-data><![CDATA[%s]]></c:address-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, href, card)
					} else {
						_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>%s</d:href><d:status>HTTP/1.1 404 Not Found</d:status></d:response></d:multistatus>`, href)
					}
				case "PUT":
					if strings.HasSuffix(r.URL.Path, "/deleted.vcf") {
						http.Error(w, "deleted", 404)
						return
					}
					puts++
					if conflict {
						conflict = false
						http.Error(w, "changed remotely", 412)
						return
					}
					if cards[r.URL.Path] != "" && r.Header.Get("If-Match") != "v2" {
						t.Error("recovery did not use fetched version")
					}
					cards[r.URL.Path] = string(body)
					w.Header().Set("ETag", "v1")
					w.WriteHeader(201)
				default:
					http.Error(w, "unexpected request", 400)
				}
			})
			f = newUserContactPushFixture(t, "carddav", api)
			profile := seedUserContactPush(t, f, "alice", "carddav")
			if retainedMissingSource {
				if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
					return db.UpsertContactSource(t.Context(), storage.ContactSource{UserID: "alice", ContactID: profile, Provider: "carddav", AccountID: f.accounts["alice"].ID, AddressBookID: "alice-book", RemoteID: f.server.URL + "/books/alice/deleted.vcf", Etag: "deleted-version"})
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				_, err := db.Write().Exec(`CREATE TRIGGER fail_push_ack BEFORE INSERT ON contact_cards WHEN NEW.kind='provider' BEGIN SELECT RAISE(ABORT,'ack failure'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); err == nil || n != 1 {
				t.Fatal("ack failure not observed", n, err)
			}
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				_, err := db.Write().Exec(`DROP TRIGGER fail_push_ack;UPDATE contact_sync_operations SET next_attempt_at=CURRENT_TIMESTAMP`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); err != nil || n != 1 {
				t.Fatal("recovery failed", n, err)
			}
			mu.Lock()
			count := len(cards)
			putCount := puts
			conflict = true
			mu.Unlock()
			if count != 1 || putCount != 2 {
				t.Fatal("recovery duplicated remote resource", count, putCount)
			}
			seedUserContactPush(t, f, "alice", "carddav")
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				return db.ReplaceCanonicalContact(t.Context(), "alice", profile, models.Contact{Name: "alice", Email: "same@example.com", Phone: "after-conflict", PhoneLabel: "work"})
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); err == nil || n != 1 {
				t.Fatal("conflict not surfaced", n, err)
			}
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				source, err := db.GetContactSource(t.Context(), "alice", profile, "carddav", f.accounts["alice"].ID)
				if err != nil {
					return err
				}
				if source == nil || source.Etag != "v2" {
					t.Fatal("conflict version not checkpointed", source)
				}
				fields, err := db.ListContactFields(t.Context(), "alice", profile)
				if err != nil {
					return err
				}
				var prior bool
				for _, field := range fields {
					if field.Source == "synced:"+f.accounts["alice"].ID && field.Kind == "phone" {
						if field.Value != "alice" {
							t.Fatal("failed PUT falsely acknowledged new values", field)
						}
						prior = true
					}
				}
				if !prior {
					t.Fatal("prior acknowledged values missing")
				}
				var pending int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations WHERE status='pending'`).Scan(&pending); err != nil {
					return err
				}
				if pending != 1 {
					t.Fatal("conflict falsely completed")
				}
				_, err = db.Write().Exec(`UPDATE contact_sync_operations SET next_attempt_at=CURRENT_TIMESTAMP WHERE status='pending'`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); err != nil || n != 1 {
				t.Fatal("conflict retry failed", n, err)
			}
		})
	}
}

func TestUserContactPushShutdownCancelsWholeOwnerJobWithoutLateFinalization(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-r.Context().Done():
			return
		case <-release:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	f := newUserContactPushFixture(t, "gmail", api)
	defer close(release)
	profile := seedUserContactPush(t, f, "alice", "gmail")
	done := make(chan error, 1)
	go func() { _, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider request did not start")
	}
	f.cancel()
	joined := make(chan struct{})
	go func() { f.h.userIMAP.Wait(); close(joined) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("root cancellation not propagated", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("whole job did not cancel")
	}
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not join owner job")
	}
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		var status string
		if err := db.Read().QueryRow(`SELECT status FROM contact_sync_operations WHERE contact_id=?`, profile).Scan(&status); err != nil {
			return err
		}
		if status != "running" {
			t.Fatal("late shutdown finalization", status)
		}
		var cards int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_cards WHERE kind='provider'`).Scan(&cards); err != nil {
			return err
		}
		if cards != 0 {
			t.Fatal("late card published")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserContactPushPartialFanoutRetryKeepsAcceptedFirstTarget(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	failSecond := true
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		body, _ := io.ReadAll(r.Body)
		_ = body
		mu.Lock()
		defer mu.Unlock()
		calls[token+"|"+r.Method]++
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			_, _ = w.Write([]byte(`{"results":[]}`))
			return
		}
		if token == "second" && failSecond {
			http.Error(w, "synthetic rejected write", 400)
			return
		}
		_, _ = w.Write([]byte(`{"resourceName":"people/shared","etag":"accepted"}`))
	})
	f := newUserContactPushFixture(t, "gmail", api)
	other, err := f.h.userAccounts.CreateAccount(t.Context(), "alice", providers.GmailAccountRequest("second@example.com", "second", "second-subject"))
	if err != nil {
		t.Fatal(err)
	}
	ordered := []*models.Account{f.accounts["alice"], other}
	if ordered[0].ID > ordered[1].ID {
		ordered[0], ordered[1] = ordered[1], ordered[0]
	}
	for index, account := range ordered {
		token := []string{"first", "second"}[index]
		expires := time.Now().Add(time.Hour)
		if err := f.h.userAccounts.WithAccountForUser(t.Context(), "alice", account.ID, func(_ *config.AccountStore, db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE accounts SET email_sync_enabled=0 WHERE id=?`, account.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.h.userCredentials.UpsertForUser(t.Context(), "alice", account.ID, "google", map[bool]string{true: "second-subject", false: "subject"}[account.ID == other.ID], token, "refresh", "Bearer", &expires, "https://www.googleapis.com/auth/contacts"); err != nil {
			t.Fatal(err)
		}
	}
	id := seedUserContactPush(t, f, "alice", "gmail")
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		return db.ReplaceContactSyncMemberships(t.Context(), "alice", id, []string{"account:" + ordered[0].ID, "account:" + ordered[1].ID})
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); err == nil || n != 1 {
		t.Fatal("second target failure missing", n, err)
	}
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		accepted, err := db.GetContactSource(t.Context(), "alice", id, "gmail", ordered[0].ID)
		if err != nil {
			return err
		}
		if accepted == nil {
			t.Fatal("accepted prefix lost")
		}
		failed, err := db.GetContactSource(t.Context(), "alice", id, "gmail", ordered[1].ID)
		if err != nil {
			return err
		}
		if failed != nil {
			t.Fatal("failed target acknowledged")
		}
		_, err = db.Write().Exec(`UPDATE contact_sync_operations SET next_attempt_at=CURRENT_TIMESTAMP WHERE status='pending'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	failSecond = false
	mu.Unlock()
	if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); err != nil || n != 1 {
		t.Fatal("partial retry failed", n, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["first|POST"] != 1 || calls["first|PATCH"] != 1 || calls["second|POST"] != 2 {
		t.Fatal("retry duplicated accepted resource or missed failed target", calls)
	}
}

func TestUserContactPushThrottleDefersQueueAndPreservesOtherOwnerProgress(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		calls[token]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			_, _ = w.Write([]byte(`{"value":[]}`))
			return
		}
		if token == "alice-access" {
			w.Header().Set("Retry-After", "3600")
			http.Error(w, `{"access_token":"synthetic-secret"}`, 429)
			return
		}
		_, _ = w.Write([]byte(`{"id":"created","changeKey":"v1"}`))
	})
	f := newUserContactPushFixture(t, "outlook", api)
	profile := seedUserContactPush(t, f, "alice", "outlook")
	if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); err == nil || n != 1 {
		t.Fatal("throttle not observed", n, err)
	}
	until, err := f.h.userStorage.ProviderRetryUntil(t.Context(), "alice", f.accounts["alice"].ID)
	if err != nil || !until.After(time.Now().Add(50*time.Minute)) {
		t.Fatal("cooldown missing", until, err)
	}
	for i := 0; i < 3; i++ {
		if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE contact_sync_operations SET next_attempt_at=CURRENT_TIMESTAMP WHERE status='pending'`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); err == nil || n != 1 {
			t.Fatal("deferred attempt not reported", n, err)
		}
	}
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		var status, message string
		if err := db.Read().QueryRow(`SELECT status,last_error FROM contact_sync_operations WHERE contact_id=?`, profile).Scan(&status, &message); err != nil {
			return err
		}
		if status != "pending" || strings.Contains(message, "synthetic-secret") {
			t.Fatal("deferred work terminated or exposed secret", status, message)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	next, err := f.h.userAccounts.NextContactSyncAttempt(t.Context(), "alice", userContactClaimTimeout)
	if err != nil || !next.Equal(until) {
		t.Fatal("queue did not retain provider deadline", next, until, err)
	}
	seedUserContactPush(t, f, "bob", "outlook")
	if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "bob", 1); err != nil || n != 1 {
		t.Fatal("Alice throttle blocked Bob", n, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["alice-access"] != 2 || calls["bob-access"] != 2 {
		t.Fatal("cooldown repeated provider requests or blocked other owner", calls)
	}
}

func TestUserContactPushScopedRefreshPreservesRotationAndOwnerIsolation(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			var mu sync.Mutex
			var refreshes, writes, searches int
			api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path == "/token" {
					_ = r.ParseForm()
					want := "alice-refresh"
					if refreshes > 0 {
						want = "alice-rotated"
					}
					if r.FormValue("refresh_token") != want || provider == "outlook" && !strings.Contains(r.FormValue("scope"), "Contacts.ReadWrite") {
						t.Error("refresh lost rotation or contacts purpose", r.PostForm)
					}
					refreshes++
					_, _ = w.Write([]byte(`{"access_token":"alice-fresh","refresh_token":"alice-rotated","expires_in":3600,"token_type":"Bearer","scope":"https://www.googleapis.com/auth/contacts https://graph.microsoft.com/Contacts.ReadWrite"}`))
					return
				}
				if r.Method == "GET" {
					searches++
					if provider == "gmail" {
						_, _ = w.Write([]byte(`{"results":[]}`))
					} else {
						_, _ = w.Write([]byte(`{"value":[]}`))
					}
					return
				}
				writes++
				if r.Header.Get("Authorization") == "Bearer alice-access" {
					http.Error(w, "expired", 401)
					return
				}
				if r.Header.Get("Authorization") != "Bearer alice-fresh" {
					t.Error("wrong scoped write token")
				}
				if provider == "gmail" {
					_, _ = w.Write([]byte(`{"resourceName":"people/created","etag":"v1"}`))
				} else {
					_, _ = w.Write([]byte(`{"id":"created","changeKey":"v1"}`))
				}
			})
			f := newUserContactPushFixture(t, provider, api)
			id := seedUserContactPush(t, f, "alice", provider)
			var accessBefore []byte
			if err := f.system.Read().QueryRow(`SELECT access_token_ciphertext FROM gofer_mailbox_credentials WHERE account_id=? AND user_id=?`, f.accounts["alice"].ID, "alice").Scan(&accessBefore); err != nil {
				t.Fatal(err)
			}
			if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); err != nil || n != 1 {
				t.Fatal("refreshed job", n, err)
			}
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				source, err := db.GetContactSource(t.Context(), "alice", id, provider, f.accounts["alice"].ID)
				if err == nil && (source == nil || source.Etag != "v1") {
					t.Fatal("refreshed write not acknowledged", source)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			// A second explicit refresh must use the rotated refresh token. The
			// Graph mailbox cache stays independent; Google refresh updates its shared grant.
			bound := f.h.userCredentials.ContactsAccount("alice", f.accounts["alice"].ID)
			if token, err := bound.RefreshOAuthTokenForAccount(t.Context(), f.accounts["alice"].ID); err != nil || token != "alice-fresh" {
				t.Fatal("rotated refresh", token, err)
			}
			var accessAfter []byte
			if err := f.system.Read().QueryRow(`SELECT access_token_ciphertext FROM gofer_mailbox_credentials WHERE account_id=? AND user_id=?`, f.accounts["alice"].ID, "alice").Scan(&accessAfter); err != nil {
				t.Fatal(err)
			}
			if provider == "outlook" && string(accessBefore) != string(accessAfter) {
				t.Fatal("scoped refresh replaced the stored mailbox access token")
			}
			var mailbox string
			var err error
			if provider == "gmail" {
				mailbox, err = f.h.userCredentials.ContactsAccount("bob", f.accounts["bob"].ID).GetOAuthTokenForAccount(t.Context(), f.accounts["bob"].ID)
			} else {
				mailbox, err = f.h.userCredentials.GetMicrosoftGraphContactsTokenForUser(t.Context(), "bob", f.accounts["bob"].ID)
			}
			if err != nil || mailbox != "bob-access" {
				t.Fatal("refresh changed another owner", mailbox, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if refreshes != 2 || writes != 2 || searches != 1 {
				t.Fatal("wrong refresh/write attempts", refreshes, writes, searches)
			}
		})
	}
}

func TestUserContactPushBlockedWriteReleasesLeaseAndRejectsChangedProfile(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			_, _ = w.Write([]byte(`{"value":[]}`))
			return
		}
		if r.Header.Get("Authorization") == "Bearer alice-access" {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = w.Write([]byte(`{"id":"created","changeKey":"v1"}`))
	})
	f := newUserContactPushFixture(t, "outlook", api)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	id := seedUserContactPush(t, f, "alice", "outlook")
	seedUserContactPush(t, f, "bob", "outlook")
	done := make(chan error, 1)
	go func() {
		_, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("write did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if n, err := f.h.ProcessUserContactSyncOperations(ctx, "bob", 1); err != nil || n != 1 {
		t.Fatal("blocked Alice retained the only store slot", n, err)
	}
	if err := f.h.userAccounts.WithUser(ctx, "alice", func(_ *config.AccountStore, db *storage.DB) error {
		_, err := db.SaveContact(ctx, "alice", models.Contact{ID: id, Email: "same@example.com", Name: "changed", Phone: "new-value", GoferSyncEnabled: true, SaveTargets: []string{"account:" + f.accounts["alice"].ID}})
		return err
	}); err != nil {
		t.Fatal("blocked write retained a database lease", err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stale provider response acknowledged")
		}
	case <-ctx.Done():
		t.Fatal("write did not finish", ctx.Err())
	}
	if err := f.h.userAccounts.WithUser(ctx, "alice", func(_ *config.AccountStore, db *storage.DB) error {
		source, err := db.GetContactSource(ctx, "alice", id, "outlook", f.accounts["alice"].ID)
		if err != nil {
			return err
		}
		if source != nil {
			t.Fatal("stale response published", source)
		}
		current, err := db.GetContact(ctx, "alice", id)
		if err == nil && (current == nil || current.Phone != "new-value") {
			t.Fatal("stale response replaced current canonical values", current)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserContactPushGraphPreflightChecksAllPagesBeforeWriting(t *testing.T) {
	for _, outcome := range []string{"match", "ambiguous", "foreign-page", "repeated-page"} {
		t.Run(outcome, func(t *testing.T) {
			var mu sync.Mutex
			var reads, writes int
			api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.Method != "GET" {
					writes++
					if outcome != "match" || r.Method != "PATCH" || r.URL.Path != "/me/contacts/existing" || reads != 2 {
						t.Error("write before validated complete preflight", outcome, r.Method, r.URL.Path, reads)
					}
					_, _ = w.Write([]byte(`{"id":"existing","changeKey":"v2"}`))
					return
				}
				reads++
				if reads == 1 {
					next := outlookGraphBaseURL + "/me/contacts?$skiptoken=next"
					if outcome == "foreign-page" {
						next = "https://foreign.invalid/me/contacts?$skiptoken=next"
					}
					if outcome == "repeated-page" {
						next = outlookGraphBaseURL + r.URL.RequestURI()
					}
					values := []map[string]any{}
					if outcome == "ambiguous" {
						values = append(values, map[string]any{"id": "first", "emailAddresses": []map[string]string{{"address": "same@example.com"}}})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"value": values, "@odata.nextLink": next})
					return
				}
				_, _ = w.Write([]byte(`{"value":[{"id":"existing","changeKey":"v1","emailAddresses":[{"address":"same@example.com"}]}]}`))
			})
			f := newUserContactPushFixture(t, "outlook", api)
			seedUserContactPush(t, f, "alice", "outlook")
			n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1)
			if n != 1 || (err == nil) != (outcome == "match") {
				t.Fatal("wrong preflight outcome", n, err)
			}
			mu.Lock()
			defer mu.Unlock()
			wantWrites, wantReads := 0, 1
			if outcome == "match" {
				wantWrites = 1
			}
			if outcome == "match" || outcome == "ambiguous" {
				wantReads = 2
			}
			if writes != wantWrites || reads != wantReads {
				t.Fatal("wrong preflight requests", reads, writes)
			}
		})
	}
}

func TestUserContactPushMissingRemoteRecoveryDoesNotCreateTwice(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			var mu sync.Mutex
			var creates int
			created := false
			oldID, newID := "old", "replacement"
			if provider == "gmail" {
				oldID, newID = "people/old", "people/replacement"
			}
			api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "POST" {
					creates++
					created = true
					if provider == "gmail" {
						_, _ = w.Write([]byte(`{"resourceName":"people/replacement","etag":"v2"}`))
					} else {
						_, _ = w.Write([]byte(`{"id":"replacement","changeKey":"v2"}`))
					}
					return
				}
				if r.Method == "PATCH" {
					if strings.Contains(r.URL.Path, oldID) {
						http.Error(w, "deleted", 404)
						return
					}
					if provider == "gmail" {
						_, _ = w.Write([]byte(`{"resourceName":"people/replacement","etag":"v3"}`))
					} else {
						_, _ = w.Write([]byte(`{"id":"replacement","changeKey":"v3"}`))
					}
					return
				}
				if r.Method == "GET" {
					if provider == "gmail" {
						if created {
							_, _ = w.Write([]byte(`{"results":[{"person":{"resourceName":"people/replacement","etag":"v2","emailAddresses":[{"value":"same@example.com"}]}}]}`))
						} else {
							_, _ = w.Write([]byte(`{"results":[]}`))
						}
					} else if created {
						_, _ = w.Write([]byte(`{"value":[{"id":"replacement","changeKey":"v2","emailAddresses":[{"address":"same@example.com"}]}]}`))
					} else {
						_, _ = w.Write([]byte(`{"value":[]}`))
					}
					return
				}
				http.Error(w, "unexpected request", 400)
			})
			f := newUserContactPushFixture(t, provider, api)
			id := seedUserContactPush(t, f, "alice", provider)
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				if err := db.UpsertContactSource(t.Context(), storage.ContactSource{UserID: "alice", ContactID: id, Provider: provider, AccountID: f.accounts["alice"].ID, RemoteID: oldID, Etag: "v1"}); err != nil {
					return err
				}
				_, err := db.Write().Exec(`CREATE TRIGGER fail_replacement_ack BEFORE INSERT ON contact_cards WHEN NEW.kind='provider' BEGIN SELECT RAISE(ABORT,'ack failure'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); err == nil || n != 1 {
				t.Fatal("ack failure not observed", n, err)
			}
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				source, err := db.GetContactSource(t.Context(), "alice", id, provider, f.accounts["alice"].ID)
				if err != nil {
					return err
				}
				if source == nil || source.RemoteID != oldID {
					t.Fatal("failed ack changed old source", source)
				}
				_, err = db.Write().Exec(`DROP TRIGGER fail_replacement_ack; UPDATE contact_sync_operations SET next_attempt_at=CURRENT_TIMESTAMP`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); err != nil || n != 1 {
				t.Fatal("replacement recovery", n, err)
			}
			mu.Lock()
			count := creates
			mu.Unlock()
			if count != 1 {
				t.Fatal("accepted replacement duplicated after failed ack", count)
			}
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				source, err := db.GetContactSource(t.Context(), "alice", id, provider, f.accounts["alice"].ID)
				if err == nil && (source == nil || source.RemoteID != newID || source.Etag != "v3") {
					t.Fatal("replacement not acknowledged", source)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserContactPushGoogleConflictRefreshThrottleKeepsLastAttemptPending(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case "PATCH":
			http.Error(w, "stale ETag", http.StatusPreconditionFailed)
		case "GET":
			w.Header().Set("Retry-After", "3600")
			http.Error(w, "retry later", http.StatusTooManyRequests)
		default:
			t.Error("unexpected contact request", r.Method)
			http.Error(w, "unexpected request", 400)
		}
	})
	f := newUserContactPushFixture(t, "gmail", api)
	id := seedUserContactPush(t, f, "alice", "gmail")
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		if err := db.UpsertContactSource(t.Context(), storage.ContactSource{UserID: "alice", ContactID: id, Provider: "gmail", AccountID: f.accounts["alice"].ID, RemoteID: "people/existing", Etag: "v1"}); err != nil {
			return err
		}
		_, err := db.Write().Exec(`UPDATE contact_sync_operations SET attempt_count=2 WHERE contact_id=?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); err == nil || n != 1 {
		t.Fatal("conflict refresh throttle not reported", n, err)
	}
	until, err := f.h.userStorage.ProviderRetryUntil(t.Context(), "alice", f.accounts["alice"].ID)
	if err != nil || !until.After(time.Now().Add(50*time.Minute)) {
		t.Fatal("conflict refresh cooldown missing", until, err)
	}
	next, err := f.h.userAccounts.NextContactSyncAttempt(t.Context(), "alice", userContactClaimTimeout)
	if err != nil || next.UnixMilli() != until.UnixMilli() {
		t.Fatal("throttled conflict refresh discarded pending work", next, until, err)
	}
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		var status string
		var attempts int
		if err := db.Read().QueryRow(`SELECT status,attempt_count FROM contact_sync_operations WHERE contact_id=?`, id).Scan(&status, &attempts); err != nil {
			return err
		}
		if status != "pending" || attempts != 3 {
			t.Fatal("conflict refresh exhausted throttled work", status, attempts)
		}
		source, err := db.GetContactSource(t.Context(), "alice", id, "gmail", f.accounts["alice"].ID)
		if err == nil && (source == nil || source.Etag != "v1") {
			t.Fatal("failed refresh acknowledged new version", source)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
