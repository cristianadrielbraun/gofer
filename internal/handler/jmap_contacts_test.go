package handler

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// jmapContactsTestServer serves a JMAP account with two address books to
// any username with the password. Cards are JSON keyed by id; changes is
// what ContactCard/changes reports next.
type jmapContactsTestServer struct {
	password string
	mu       sync.Mutex
	state    string
	cards    map[string]string
	changes  map[string]any
}

func (f *jmapContactsTestServer) set(state string, cards map[string]string, changes map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state, f.cards, f.changes = state, cards, changes
}

func (f *jmapContactsTestServer) start(t *testing.T) (*httptest.Server, string, int) {
	t.Helper()
	server := httptest.NewServer(f.handler())
	t.Cleanup(server.Close)
	host, portText, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	port, _ := strconv.Atoi(portText)
	return server, host, port
}

func (f *jmapContactsTestServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pass, ok := r.BasicAuth(); !ok || pass != f.password {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/.well-known/jmap" {
			_, _ = w.Write([]byte(`{"capabilities": {"urn:ietf:params:jmap:core": {}, "urn:ietf:params:jmap:mail": {}, "urn:ietf:params:jmap:contacts": {}},
			  "accounts": {"a1": {"name": "lena"}}, "primaryAccounts": {"urn:ietf:params:jmap:mail": "a1", "urn:ietf:params:jmap:contacts": "a1"},
			  "apiUrl": "http://` + r.Host + `/api/", "state": "s"}`))
			return
		}
		var request struct {
			MethodCalls [][]json.RawMessage `json:"methodCalls"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		var name string
		var args struct {
			IDs *[]string `json:"ids"`
		}
		_ = json.Unmarshal(request.MethodCalls[0][0], &name)
		_ = json.Unmarshal(request.MethodCalls[0][1], &args)
		f.mu.Lock()
		defer f.mu.Unlock()
		var answer any
		switch name {
		case "AddressBook/get":
			answer = map[string]any{"accountId": "a1", "state": "b", "list": []any{
				map[string]any{"id": "B1", "name": "Personal", "isDefault": true, "myRights": map[string]bool{"mayRead": true, "mayWrite": true}},
				map[string]any{"id": "B2", "name": "Team", "myRights": map[string]bool{"mayRead": true}},
			}}
		case "ContactCard/get":
			list := []json.RawMessage{}
			for _, id := range *args.IDs {
				if card, ok := f.cards[id]; ok {
					list = append(list, json.RawMessage(card))
				}
			}
			answer = map[string]any{"accountId": "a1", "state": f.state, "list": list, "notFound": []string{}}
		case "ContactCard/query":
			ids := make([]string, 0, len(f.cards))
			for id := range f.cards {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			answer = map[string]any{"accountId": "a1", "ids": ids, "position": 0, "total": len(ids)}
		case "ContactCard/changes":
			answer = f.changes
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{name, answer, "0"}}, "sessionState": "s"})
	})
}

func TestJMAPContactSyncSetupAndPull(t *testing.T) {
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	h, db := newAccountOwnershipTestHandler(t)
	fake := &jmapContactsTestServer{password: "saved-secret"}
	fake.set("s1", map[string]string{
		"c1": `{"id": "c1", "addressBookIds": {"B1": true}, "name": {"full": "Lena Brandt"}, "emails": {"e": {"address": "lena.brandt@example.test"}}}`,
		"c2": `{"id": "c2", "addressBookIds": {"B1": true}, "name": {"full": "Milo Hart"}, "phones": {"p": {"number": "+1 555 0123", "features": {"mobile": true}}}}`,
		"c3": `{"id": "c3", "addressBookIds": {"B2": true}, "name": {"full": "Team Only"}, "emails": {"e": {"address": "team@example.test"}}}`,
	}, map[string]any{"accountId": "a1", "oldState": "s1", "newState": "s1", "hasMoreChanges": false, "created": []string{}, "updated": []string{}, "destroyed": []string{}})
	server, host, port := fake.start(t)
	if err := db.AddPlaintextTransportException(t.Context(), "http", host, port, "administrator"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPrivateTargetException(t.Context(), "http", host, port, "administrator"); err != nil {
		t.Fatal(err)
	}
	account, err := h.accountStore.CreateAccount(t.Context(), "owner", &models.CreateAccountRequest{
		Provider: "jmap", EmailAddress: "lena@example.test", DisplayName: "Lena",
		JMAPSessionURL: server.URL + "/.well-known/jmap", Username: "lena@example.test", Password: "saved-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	post := func(path string, form url.Values, handle func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetPathValue("id", account.ID)
		rec := httptest.NewRecorder()
		handle(rec, ownerRequest(req))
		return rec
	}

	rec := post("/api/accounts/"+account.ID+"/contacts/sync/discover", url.Values{"provider": {"jmap"}}, h.handleDiscoverAccountContactSync)
	var discovered struct {
		AddressBooks []models.ContactAddressBook `json:"address_books"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &discovered); err != nil || len(discovered.AddressBooks) != 2 || discovered.AddressBooks[0].URL != "B1" || !discovered.AddressBooks[0].Default {
		t.Fatalf("discovery = %d %s", rec.Code, rec.Body.String())
	}

	rec = post("/api/accounts/"+account.ID+"/contacts/sync", url.Values{"provider": {"jmap"}, "addressbook_url": {"B1", "missing"}, "default_addressbook_url": {"B1"}}, h.handleSaveAccountContactSync)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Gofer-Contact-Sync-Enabled") != "true" {
		t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
	}
	cfg, err := h.accountStore.GetContactSyncConfig(t.Context(), "owner", account.ID)
	if err != nil || cfg.Provider != "jmap" || !cfg.Enabled || len(cfg.AddressBooks) != 1 || cfg.AddressBooks[0].Name != "Personal" {
		t.Fatalf("saved config = %+v, %v", cfg, err)
	}

	source := func(remote string) bool {
		t.Helper()
		found, err := db.GetContactSourceByRemoteID(t.Context(), "owner", "jmap", account.ID, remote)
		if err != nil {
			t.Fatal(err)
		}
		return found != nil
	}
	// Saving starts a sync in the background.
	for deadline := time.Now().Add(5 * time.Second); !source("c2"); {
		if time.Now().After(deadline) {
			t.Fatal("background sync after saving imported nothing")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := h.SyncContactAccount(t.Context(), account.ID); err == nil {
			break
		} else if !strings.Contains(err.Error(), "already running") || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !source("c1") || source("c3") {
		t.Fatal("the full pull should take only the selected book")
	}
	if cfg, _ := h.accountStore.GetContactSyncConfig(t.Context(), "owner", account.ID); cfg.LastSyncToken != "s1" {
		t.Fatalf("state after full pull = %q", cfg.LastSyncToken)
	}

	fake.set("s2", map[string]string{
		"c1": `{"id": "c1", "addressBookIds": {"B1": true}, "name": {"full": "Lena Brandt-Ochoa"}, "emails": {"e": {"address": "lena.brandt@example.test"}}}`,
	}, map[string]any{"accountId": "a1", "oldState": "s1", "newState": "s2", "hasMoreChanges": false,
		"created": []string{}, "updated": []string{"c1"}, "destroyed": []string{"c2"}})
	if _, err := h.SyncContactAccount(t.Context(), account.ID); err != nil {
		t.Fatal(err)
	}
	if !source("c1") || source("c2") {
		t.Fatal("changes did not remove the destroyed card")
	}
	// The account's copy of the card changes; the name Gofer shows stays
	// as it was, as it does for every provider.
	found, _ := db.GetContactSourceByRemoteID(t.Context(), "owner", "jmap", account.ID, "c1")
	var synced string
	if err := db.Read().QueryRow(`SELECT value FROM contact_fields WHERE user_id='owner' AND profile_id=? AND source=? AND kind='name'`, found.ContactID, "synced:"+account.ID).Scan(&synced); err != nil || synced != "Lena Brandt-Ochoa" {
		t.Fatalf("synced name = %q, %v", synced, err)
	}
	if cfg, _ := h.accountStore.GetContactSyncConfig(t.Context(), "owner", account.ID); cfg.LastSyncToken != "s2" {
		t.Fatalf("state after changes = %q", cfg.LastSyncToken)
	}
}

func TestUserJMAPContactPullPublishesToTheOwnersStore(t *testing.T) {
	fake := &jmapContactsTestServer{password: "synthetic"}
	fake.set("s1", map[string]string{
		"c1": `{"id": "c1", "addressBookIds": {"B1": true}, "name": {"full": "Ruth Okafor"}, "emails": {"e": {"address": "ruth@example.test"}}}`,
		"c2": `{"id": "c2", "addressBookIds": {"B2": true}, "name": {"full": "Other Book"}, "emails": {"e": {"address": "other@example.test"}}}`,
	}, nil)
	f := newUserContactPushFixture(t, "jmap", fake.handler())
	f.h.contactSyncRunning = map[string]struct{}{}
	alice := f.accounts["alice"]
	imported, err := f.h.SyncUserContactAccount(t.Context(), "alice", alice.ID)
	if err != nil || imported != 1 {
		t.Fatalf("pull = %d, %v", imported, err)
	}
	snapshot, err := f.h.userAccounts.SnapshotServices(t.Context(), "alice", alice.ID)
	if err != nil || snapshot.ContactConfig().LastSyncToken != "s1" {
		t.Fatalf("state = %q, %v", snapshot.ContactConfig().LastSyncToken, err)
	}

	fake.set("s2", map[string]string{}, map[string]any{"accountId": "a1", "oldState": "s1", "newState": "s2", "hasMoreChanges": false,
		"created": []string{}, "updated": []string{}, "destroyed": []string{"c1"}})
	if _, err := f.h.SyncUserContactAccount(t.Context(), "alice", alice.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.h.userAccounts.WithAccountForUser(t.Context(), "alice", alice.ID, func(_ *config.AccountStore, db *storage.DB) error {
		source, err := db.GetContactSourceByRemoteID(t.Context(), "alice", "jmap", alice.ID, "c1")
		if err != nil || source != nil {
			t.Fatalf("destroyed card still synced: %+v, %v", source, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
