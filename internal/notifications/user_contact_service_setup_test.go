package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type ownedDAVSetupServer struct {
	mu               sync.Mutex
	blockOwner       string
	entered, release chan struct{}
	once             sync.Once
	calls            atomic.Int32
}

func (s *ownedDAVSetupServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.calls.Add(1)
	owner, password, ok := r.BasicAuth()
	if !ok || (owner != "alice" && owner != "bob") || password != owner+"-dav-secret" {
		http.Error(w, "wrong scoped credentials", 401)
		return
	}
	s.mu.Lock()
	blocked := s.blockOwner == owner
	entered, release := s.entered, s.release
	s.mu.Unlock()
	if blocked && (r.URL.Path == "/books/"+owner+"/" || r.URL.Path == "/books/"+owner+"/default/") {
		s.once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusMultiStatus)
	switch r.URL.Path {
	case "/.well-known/carddav", "/dav":
		_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>%s</d:href><d:propstat><d:prop><d:current-user-principal><d:href>/principals/%s/</d:href></d:current-user-principal></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, r.URL.Path, owner)
	case "/principals/" + owner + "/":
		_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav"><d:response><d:href>/principals/%s/</d:href><d:propstat><d:prop><c:addressbook-home-set><d:href>/books/%s/</d:href></c:addressbook-home-set></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, owner, owner)
	case "/books/" + owner + "/":
		_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav"><d:response><d:href>/books/%s/default/</d:href><d:propstat><d:prop><d:displayname>%s private book</d:displayname><d:resourcetype><d:collection/><c:addressbook/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, owner, owner)
	default:
		_, _ = io.WriteString(w, `<d:multistatus xmlns:d="DAV:"/>`)
	}
}
func newOwnedDAVSetupFixture(t *testing.T) (*userStorageFixture, *ownedDAVSetupServer, *httptest.Server) {
	t.Helper()
	f := newUserStorageFixtureMode(t, true)
	fake := &ownedDAVSetupServer{entered: make(chan struct{}), release: make(chan struct{})}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	t.Cleanup(func() { f.stopIMAP(); f.imap.Wait() })
	for _, owner := range []string{"alice", "bob"} {
		err := f.accountStore.WithAccountForUser(t.Context(), owner, f.accounts[owner].ID, func(local *config.AccountStore, _ *storage.DB) error {
			return local.SaveContactSyncConfig(t.Context(), owner, f.accounts[owner].ID, models.ContactSyncConfig{Provider: "carddav", Enabled: true, BaseURL: server.URL + "/dav", Username: owner, AddressBooks: []models.ContactAddressBook{{ID: owner + "-owned-book", URL: server.URL + "/books/" + owner + "/default/", Default: true}}}, owner+"-dav-secret")
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return f, fake, server
}
func ownedDAVForm(server *httptest.Server, owner string) string {
	return url.Values{"base_url": {server.URL + "/dav"}, "addressbook_url": {server.URL + "/books/" + owner + "/default/"}, "username": {owner}}.Encode()
}

func TestUserContactServiceSetupSaveAndToggleOwnedStateAndWake(t *testing.T) {
	f, fake, server := newOwnedDAVSetupFixture(t)
	id := f.accounts["alice"].ID
	beforeBob, err := f.accountStore.SnapshotServices(t.Context(), "bob", f.accounts["bob"].ID)
	if err != nil {
		t.Fatal(err)
	}
	form, _ := url.ParseQuery(ownedDAVForm(server, "alice"))
	form.Set("username", "ignored-submitted-name")
	form.Set("use_account_credentials", "1")
	response := f.request("alice", "POST", "/api/accounts/"+id+"/contacts/sync", form.Encode())
	if response.Code != 200 || response.Header().Get("X-Gofer-Contact-Sync-Enabled") != "true" || !strings.Contains(response.Body.String(), "saved and verified") {
		t.Fatalf("save: %d %s", response.Code, response.Body.String())
	}
	saved, err := f.accountStore.SnapshotServices(t.Context(), "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := saved.ContactConfig(); !cfg.Enabled || cfg.AddressBooks[0].ID != "alice-owned-book" {
		t.Fatal("save lost book identity", cfg)
	}
	if password, err := saved.ContactSyncPassword(""); err != nil || password != "alice-dav-secret" {
		t.Fatal("save lost saved password fallback", err)
	}
	for _, enabled := range []string{"false", "true"} {
		if err := f.routing.ResetServiceAccounts(t.Context(), "alice", id, storage.ScheduledContacts); err != nil {
			t.Fatal(err)
		}
		revision, reserved, err := f.routing.ReserveServiceAccount(t.Context(), "alice", id, storage.ScheduledContacts, time.Now(), time.Now().Add(time.Hour), false)
		if err != nil || !reserved || revision == 0 {
			t.Fatal("reserve", err)
		}
		response := f.request("alice", "POST", "/api/accounts/"+id+"/services", "service=contacts&enabled="+enabled)
		if response.Code != 200 {
			t.Fatalf("toggle: %d %s", response.Code, response.Body.String())
		}
		after, err := f.accountStore.SnapshotServices(t.Context(), "alice", id)
		if err != nil {
			t.Fatal(err)
		}
		want, got := saved.ContactConfig(), after.ContactConfig()
		want.Enabled, want.UpdatedAt = enabled == "true", got.UpdatedAt
		if !reflect.DeepEqual(want, got) {
			t.Fatal("toggle changed saved configuration")
		}
		var due, newer int64
		if err := f.system.Read().QueryRow(`SELECT next_due_ms,revision FROM gofer_account_service_schedule WHERE account_id=? AND service='contacts'`, id).Scan(&due, &newer); err != nil || due != 0 || newer <= revision {
			t.Fatal("toggle did not durably wake contacts", due, newer, err)
		}
	}
	calls := fake.calls.Load()
	for _, path := range []string{"/contacts/sync", "/service", "/services"} {
		body := ownedDAVForm(server, "alice")
		if path == "/service" || path == "/services" {
			body = "service=contacts&enabled=false"
		}
		if response := f.request("bob", "POST", "/api/accounts/"+id+path, body); response.Code != 404 {
			t.Fatal("foreign settings accepted", path, response.Code)
		}
	}
	if fake.calls.Load() != calls {
		t.Fatal("foreign save contacted provider")
	}
	if err := f.accountStore.ValidateContactServices(t.Context(), beforeBob); err != nil {
		t.Fatal("Alice settings changed Bob", err)
	}
	response = f.request("alice", "POST", "/api/accounts/"+id+"/contacts/sync", "")
	if response.Code != 200 || response.Header().Get("X-Gofer-Contact-Sync-Enabled") != "false" || fake.calls.Load() != calls {
		t.Fatal("disabled save contacted provider", response.Code)
	}
	var count int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM account_contact_sync_configs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("contact settings wrote centrally", count, err)
	}
}

func TestUserContactServiceSetupSaveFailureAndWakeFailureAreDistinct(t *testing.T) {
	f, _, server := newOwnedDAVSetupFixture(t)
	id := f.accounts["alice"].ID
	before, err := f.accountStore.SnapshotServices(t.Context(), "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	form, _ := url.ParseQuery(ownedDAVForm(server, "alice"))
	form.Set("password", "wrong-secret")
	response := f.request("alice", "POST", "/api/accounts/"+id+"/contacts/sync", form.Encode())
	if response.Code != http.StatusOK || response.Header().Get("X-Gofer-Status") != "error" || !strings.Contains(response.Body.String(), "could not connect") {
		t.Fatalf("provider failure: %d %s", response.Code, response.Body.String())
	}
	if err := f.accountStore.ValidateContactServices(t.Context(), before); err != nil {
		t.Fatal("failed connection saved settings", err)
	}
	if _, err := f.system.Write().Exec(`CREATE TRIGGER reject_contact_settings_wake BEFORE INSERT ON gofer_account_service_schedule BEGIN SELECT RAISE(ABORT,'synthetic wake failure'); END`); err != nil {
		t.Fatal(err)
	}
	form.Del("password")
	form.Set("addressbook_name", "committed before wake")
	response = f.request("alice", "POST", "/api/accounts/"+id+"/contacts/sync", form.Encode())
	if response.Code != http.StatusOK || response.Header().Get("X-Gofer-Status") != "error" || !strings.Contains(response.Body.String(), "settings saved") {
		t.Fatalf("save wake failure: %d %s", response.Code, response.Body.String())
	}
	saved, err := f.accountStore.SnapshotServices(t.Context(), "alice", id)
	if err != nil || saved.ContactConfig().AddressBooks[0].Name != "committed before wake" {
		t.Fatal("wake failure lost committed DAV save", err)
	}
	response = f.request("alice", "POST", "/api/accounts/"+id+"/service", "service=contacts&enabled=false")
	if response.Code != 503 || !strings.Contains(response.Body.String(), "setting saved") {
		t.Fatalf("wake failure: %d %s", response.Code, response.Body.String())
	}
	after, err := f.accountStore.SnapshotServices(t.Context(), "alice", id)
	if err != nil || after.ContactConfig().Enabled {
		t.Fatal("wake failure lost committed toggle", err)
	}
	if _, err := f.system.Write().Exec(`DROP TRIGGER reject_contact_settings_wake`); err != nil {
		t.Fatal(err)
	}
	response = f.request("alice", "POST", "/api/accounts/"+id+"/service", "service=contacts&enabled=false")
	if response.Code != 200 {
		t.Fatal("retry did not wake committed setting", response.Code)
	}
}

func TestUserContactServiceSetupSlowSaveReleasesCacheAndRejectsLateCommit(t *testing.T) {
	for _, change := range []string{"config", "identity", "disabled", "deletion", "cancel"} {
		t.Run(change, func(t *testing.T) {
			f, fake, server := newOwnedDAVSetupFixture(t)
			id := f.accounts["alice"].ID
			fake.mu.Lock()
			fake.blockOwner = "alice"
			fake.mu.Unlock()
			form, _ := url.ParseQuery(ownedDAVForm(server, "alice"))
			form.Set("addressbook_name", "late book name")
			requestCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request := httptest.NewRequest("POST", "/api/accounts/"+id+"/contacts/sync", strings.NewReader(form.Encode())).WithContext(requestCtx)
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { recorder := httptest.NewRecorder(); f.http.ServeHTTP(recorder, request); done <- recorder }()
			select {
			case <-fake.entered:
			case <-time.After(5 * time.Second):
				close(fake.release)
				t.Fatal("save did not reach provider verification")
			}
			ctx, stop := context.WithTimeout(t.Context(), time.Second)
			defer stop()
			if _, err := f.accountStore.SnapshotServices(ctx, "bob", f.accounts["bob"].ID); err != nil {
				close(fake.release)
				t.Fatal("save retained the one-store cache", err)
			}
			var err error
			switch change {
			case "config", "identity":
				err = f.accountStore.WithAccountForUser(ctx, "alice", id, func(_ *config.AccountStore, db *storage.DB) error {
					query := `UPDATE account_contact_sync_configs SET username='changed' WHERE account_id=?`
					if change == "identity" {
						query = `UPDATE accounts SET email_address='changed@test.local' WHERE id=?`
					}
					_, err := db.Write().Exec(query, id)
					return err
				})
			case "disabled":
				_, err = f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
			case "deletion":
				err = f.routing.RequestAccountDeletion(ctx, "alice", id)
			case "cancel":
				cancel()
			}
			close(fake.release)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case response := <-done:
				if response.Code == 200 || strings.Contains(response.Body.String(), "saved and verified") {
					t.Fatalf("late settings escaped: %d %s", response.Code, response.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled or stale save did not finish")
			}
			if change == "disabled" {
				if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var count int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM account_contact_address_books WHERE account_id=? AND name='late book name'`, id).Scan(&count); err != nil {
					return err
				}
				if count != 0 {
					t.Fatal("late configuration was committed")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserContactServiceSetupOwnerCredentialsAndReadOnlyResults(t *testing.T) {
	f, fake, server := newOwnedDAVSetupFixture(t)
	for _, owner := range []string{"alice", "bob"} {
		id := f.accounts[owner].ID
		before, err := f.accountStore.SnapshotServices(t.Context(), owner, id)
		if err != nil {
			t.Fatal(err)
		}
		response := f.request(owner, "POST", "/api/accounts/"+id+"/contacts/sync/test", ownedDAVForm(server, owner))
		if response.Code != 200 || !strings.Contains(response.Body.String(), "CardDAV connection succeeded") {
			t.Fatalf("test %s: %d %s", owner, response.Code, response.Body.String())
		}
		response = f.request(owner, "POST", "/api/accounts/"+id+"/contacts/sync/discover", ownedDAVForm(server, owner))
		if response.Code != 200 || !strings.Contains(response.Body.String(), owner+" private book") {
			t.Fatalf("discovery %s: %d %s", owner, response.Code, response.Body.String())
		}
		var result struct {
			Books []models.ContactAddressBook `json:"address_books"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Books) != 1 || result.Books[0].URL != server.URL+"/books/"+owner+"/default/" {
			t.Fatalf("discovered wrong owner books: %v %+v", err, result)
		}
		if err := f.accountStore.ValidateContactServices(t.Context(), before); err != nil {
			t.Fatal("setup mutated saved configuration", err)
		}
	}
	calls := fake.calls.Load()
	for _, path := range []string{"test", "discover"} {
		response := f.request("bob", "POST", "/api/accounts/"+f.accounts["alice"].ID+"/contacts/sync/"+path, ownedDAVForm(server, "alice"))
		if response.Code != http.StatusNotFound {
			t.Fatalf("foreign %s: %d", path, response.Code)
		}
	}
	if fake.calls.Load() != calls {
		t.Fatal("foreign setup contacted provider")
	}
	var count int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM account_contact_sync_configs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("central configuration fallback", count, err)
	}
}

func TestUserContactServiceSetupSlowDiscoveryReleasesCacheAndRejectsChangedSettings(t *testing.T) {
	for _, change := range []string{"config", "identity", "disabled", "deletion"} {
		t.Run(change, func(t *testing.T) {
			f, fake, server := newOwnedDAVSetupFixture(t)
			fake.mu.Lock()
			fake.blockOwner = "alice"
			fake.mu.Unlock()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- f.request("alice", "POST", "/api/accounts/"+f.accounts["alice"].ID+"/contacts/sync/discover", ownedDAVForm(server, "alice"))
			}()
			select {
			case <-fake.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("DAV discovery did not block")
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, err := f.accountStore.SnapshotServices(ctx, "bob", f.accounts["bob"].ID); err != nil {
				t.Fatal("discovery retained user database", err)
			}
			if response := f.request("bob", "POST", "/api/accounts/"+f.accounts["bob"].ID+"/contacts/sync/test", ownedDAVForm(server, "bob")); response.Code != 200 {
				t.Fatal("other owner's setup blocked", response.Code)
			}
			var err error
			switch change {
			case "config":
				err = f.accountStore.WithAccountForUser(ctx, "alice", f.accounts["alice"].ID, func(_ *config.AccountStore, db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE account_contact_sync_configs SET username='changed' WHERE account_id=?`, f.accounts["alice"].ID)
					return err
				})
			case "identity":
				err = f.accountStore.WithAccountForUser(ctx, "alice", f.accounts["alice"].ID, func(_ *config.AccountStore, db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE accounts SET email_address='changed@mail.test' WHERE id=?`, f.accounts["alice"].ID)
					return err
				})
			case "disabled":
				_, err = f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
			case "deletion":
				err = f.routing.RequestAccountDeletion(ctx, "alice", f.accounts["alice"].ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			close(fake.release)
			select {
			case response := <-done:
				if response.Code == 200 || strings.Contains(response.Body.String(), "alice private book") {
					t.Fatalf("stale result escaped: %d %s", response.Code, response.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stale discovery did not finish")
			}
		})
	}
}

func TestUserContactServiceSetupStreamsProgressAndOwnerBooks(t *testing.T) {
	f, fake, server := newOwnedDAVSetupFixture(t)
	fake.mu.Lock()
	fake.blockOwner = "alice"
	fake.mu.Unlock()
	endpoint := httptest.NewServer(f.http)
	t.Cleanup(endpoint.Close)
	t.Cleanup(func() { f.stopIMAP(); f.imap.Wait() })
	req, err := http.NewRequestWithContext(t.Context(), "POST", endpoint.URL+"/api/accounts/"+f.accounts["alice"].ID+"/contacts/sync/discover", strings.NewReader(ownedDAVForm(server, "alice")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/x-ndjson")
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	response, err := endpoint.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	var first map[string]any
	if err := decoder.Decode(&first); err != nil || first["type"] != "start" {
		t.Fatal("stream did not flush start", err, first)
	}
	select {
	case <-fake.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not reach DAV provider")
	}
	close(fake.release)
	progress, done := false, false
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		if event["type"] == "progress" {
			progress = true
		}
		if event["type"] == "done" {
			done = true
			if books := event["address_books"].([]any); len(books) != 1 || !strings.Contains(fmt.Sprint(books[0]), "alice private book") {
				t.Fatal("stream returned wrong owner books")
			}
		}
	}
	if !progress || !done {
		t.Fatal("stream omitted progress or completed books")
	}
}

func TestUserContactServiceSetupStreamRejectsLateConfiguration(t *testing.T) {
	f, fake, server := newOwnedDAVSetupFixture(t)
	fake.mu.Lock()
	fake.blockOwner = "alice"
	fake.mu.Unlock()
	endpoint := httptest.NewServer(f.http)
	t.Cleanup(endpoint.Close)
	t.Cleanup(func() { f.stopIMAP(); f.imap.Wait() })
	req, err := http.NewRequestWithContext(t.Context(), "POST", endpoint.URL+"/api/accounts/"+f.accounts["alice"].ID+"/contacts/sync/discover", strings.NewReader(ownedDAVForm(server, "alice")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/x-ndjson")
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	response, err := endpoint.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	var first map[string]any
	if err := decoder.Decode(&first); err != nil || first["type"] != "start" {
		t.Fatal("stream did not start", err)
	}
	select {
	case <-fake.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not reach provider")
	}
	if err := f.accountStore.WithAccountForUser(t.Context(), "alice", f.accounts["alice"].ID, func(_ *config.AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE account_contact_sync_configs SET base_url='https://changed.test' WHERE account_id=?`, f.accounts["alice"].ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	close(fake.release)
	sawError := false
	for {
		var event map[string]any
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event["type"] == "done" || event["address_books"] != nil {
			t.Fatal("stale stream returned provider books")
		}
		if event["type"] == "error" {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("stale stream omitted structured error")
	}
}

func TestUserContactServiceSetupRejectsInvalidFormsWithoutProviderCalls(t *testing.T) {
	f, fake, server := newOwnedDAVSetupFixture(t)
	id := f.accounts["alice"].ID
	for _, tc := range []struct{ suffix, body string }{
		{"test", "username=alice"},
		{"discover", "username=alice"},
		{"test", ownedDAVForm(server, "alice") + "&unused=" + strings.Repeat("x", 65<<10)},
		{"discover", ownedDAVForm(server, "alice") + "&unused=" + strings.Repeat("x", 65<<10)},
	} {
		response := f.request("alice", "POST", "/api/accounts/"+id+"/contacts/sync/"+tc.suffix, tc.body)
		if tc.suffix == "test" {
			// HTMX connection-test errors retain the existing 200/error-header
			// contract so the inline settings result can be rendered.
			if response.Code != http.StatusOK || response.Header().Get("X-Gofer-Status") != "error" {
				t.Fatalf("invalid test: %d status=%s", response.Code, response.Header().Get("X-Gofer-Status"))
			}
		} else if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid %s: %d", tc.suffix, response.Code)
		}
	}
	if fake.calls.Load() != 0 {
		t.Fatal("invalid setup contacted provider")
	}
}
