package handler

import (
	"encoding/json"
	"fmt"
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

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// jmapContactsTestServer serves a JMAP account with two address books to
// any username with the password. Cards are JSON keyed by id; changes is
// what ContactCard/changes reports next. ContactCard/set changes the cards
// and is recorded in sets. AddressBook/set adds the address books named in
// books.
type jmapContactsTestServer struct {
	password string
	mu       sync.Mutex
	state    string
	cards    map[string]string
	changes  map[string]any
	sets     []jmapContactsTestSet
	created  int
	books    []string
}

type jmapContactsTestSet struct {
	Create  map[string]map[string]any `json:"create"`
	Update  map[string]map[string]any `json:"update"`
	Destroy []string                  `json:"destroy"`
}

// card is a stored card as a map, or nil.
func (f *jmapContactsTestServer) card(id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var card map[string]any
	if raw, ok := f.cards[id]; ok {
		_ = json.Unmarshal([]byte(raw), &card)
	}
	return card
}

func (f *jmapContactsTestServer) apply(set jmapContactsTestSet) map[string]any {
	f.sets = append(f.sets, set)
	if f.cards == nil {
		f.cards = map[string]string{}
	}
	created, notUpdated, notDestroyed := map[string]any{}, map[string]any{}, map[string]any{}
	for creation, card := range set.Create {
		f.created++
		id := "n" + strconv.Itoa(f.created)
		card["id"] = id
		raw, _ := json.Marshal(card)
		f.cards[id] = string(raw)
		created[creation] = map[string]any{"id": id}
	}
	for id, patch := range set.Update {
		raw, ok := f.cards[id]
		if !ok {
			notUpdated[id] = map[string]any{"type": "notFound"}
			continue
		}
		var card map[string]any
		_ = json.Unmarshal([]byte(raw), &card)
		for path, value := range patch {
			property, key, nested := strings.Cut(path, "/")
			switch {
			case nested:
				inner, _ := card[property].(map[string]any)
				if inner == nil {
					inner = map[string]any{}
				}
				inner[key] = value
				card[property] = inner
			case value == nil:
				delete(card, property)
			default:
				card[property] = value
			}
		}
		updated, _ := json.Marshal(card)
		f.cards[id] = string(updated)
	}
	for _, id := range set.Destroy {
		if _, ok := f.cards[id]; !ok {
			notDestroyed[id] = map[string]any{"type": "notFound"}
		}
		delete(f.cards, id)
	}
	return map[string]any{"accountId": "a1", "newState": "s-set", "created": created, "notUpdated": notUpdated, "notDestroyed": notDestroyed}
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
			IDs    *[]string         `json:"ids"`
			Filter map[string]string `json:"filter"`
		}
		_ = json.Unmarshal(request.MethodCalls[0][0], &name)
		_ = json.Unmarshal(request.MethodCalls[0][1], &args)
		f.mu.Lock()
		defer f.mu.Unlock()
		var answer any
		switch name {
		case "AddressBook/get":
			list := []any{
				map[string]any{"id": "B1", "name": "Personal", "isDefault": true, "myRights": map[string]bool{"mayRead": true, "mayWrite": true}},
				map[string]any{"id": "B2", "name": "Team", "myRights": map[string]bool{"mayRead": true}},
			}
			for i, book := range f.books {
				list = append(list, map[string]any{"id": fmt.Sprintf("NB%d", i), "name": book, "myRights": map[string]bool{"mayRead": true, "mayWrite": true}})
			}
			answer = map[string]any{"accountId": "a1", "state": "b", "list": list}
		case "AddressBook/set":
			var set jmapContactsTestSet
			_ = json.Unmarshal(request.MethodCalls[0][1], &set)
			created := map[string]any{}
			for key, book := range set.Create {
				created[key] = map[string]string{"id": fmt.Sprintf("NB%d", len(f.books))}
				f.books = append(f.books, fmt.Sprint(book["name"]))
			}
			answer = map[string]any{"accountId": "a1", "created": created}
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
			for id, raw := range f.cards {
				var card struct {
					UID string `json:"uid"`
				}
				_ = json.Unmarshal([]byte(raw), &card)
				if uid, ok := args.Filter["uid"]; !ok || card.UID == uid {
					ids = append(ids, id)
				}
			}
			sort.Strings(ids)
			answer = map[string]any{"accountId": "a1", "ids": ids, "position": 0, "total": len(ids)}
		case "ContactCard/changes":
			answer = f.changes
		case "ContactCard/set":
			var set jmapContactsTestSet
			_ = json.Unmarshal(request.MethodCalls[0][1], &set)
			answer = f.apply(set)
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

func TestJMAPContactPushAndDeleteForTheSharedStore(t *testing.T) {
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	h, db := newAccountOwnershipTestHandler(t)
	fake := &jmapContactsTestServer{password: "saved-secret"}
	fake.set("s1", map[string]string{}, nil)
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
	if err := h.accountStore.SaveContactSyncConfig(t.Context(), "owner", account.ID, models.ContactSyncConfig{Enabled: true, Provider: "jmap",
		AddressBooks: []models.ContactAddressBook{{URL: "B1", Name: "Personal", Default: true}, {URL: "B2", Name: "Team"}}}, ""); err != nil {
		t.Fatal(err)
	}
	saved, err := db.SaveContact(t.Context(), "owner", models.Contact{Name: "Anton Brisk", Email: "anton@example.test", GoferSyncEnabled: true, SaveTargets: []string{"account:" + account.ID}})
	if err != nil {
		t.Fatal(err)
	}
	contact, err := db.GetContact(t.Context(), "owner", saved.ID)
	if err != nil || contact == nil {
		t.Fatal(contact, err)
	}
	if err := h.syncContactToAccountTargets(t.Context(), "owner", *contact, nil, ""); err != nil {
		t.Fatal(err)
	}
	source, err := db.GetContactSource(t.Context(), "owner", contact.ID, "jmap", account.ID)
	if err != nil || source == nil || source.RemoteID != "n1" {
		t.Fatalf("source = %+v, %v", source, err)
	}
	card := fake.card("n1")
	if books, _ := card["addressBookIds"].(map[string]any); books["B1"] != true || len(books) != 1 || card["uid"] == "" {
		t.Fatalf("new card = %+v, want it in the default book with a uid", card)
	}

	contact.Phone, contact.PhoneLabel = "+1 555 0100", "mobile"
	if err := h.syncContactToAccountTargets(t.Context(), "owner", *contact, nil, ""); err != nil {
		t.Fatal(err)
	}
	if last := fake.sets[len(fake.sets)-1]; len(last.Update["n1"]) != 1 || last.Update["n1"]["phones"] == nil {
		t.Fatalf("update = %+v, want phones only", last)
	}

	if err := h.deleteContactFromAccounts(t.Context(), "owner", *contact); err != nil {
		t.Fatal(err)
	}
	if fake.card("n1") != nil {
		t.Fatal("the card is still on the server")
	}
	if source, err := db.GetContactSource(t.Context(), "owner", contact.ID, "jmap", account.ID); err != nil || source != nil {
		t.Fatalf("source after delete = %+v, %v", source, err)
	}
}

func TestUserJMAPContactPushUpdatesAndDeletes(t *testing.T) {
	fake := &jmapContactsTestServer{password: "synthetic"}
	fake.set("s1", map[string]string{}, nil)
	f := newUserContactPushFixture(t, "jmap", fake.handler())
	alice := f.accounts["alice"]
	id := seedUserContactPush(t, f, "alice", "jmap")
	if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); err != nil || n != 1 {
		t.Fatal("owned job", n, err)
	}
	remote := func() *storage.ContactSource {
		t.Helper()
		var source *storage.ContactSource
		if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
			var err error
			source, err = db.GetContactSource(t.Context(), "alice", id, "jmap", alice.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return source
	}
	if source := remote(); source == nil || source.RemoteID != "n1" || source.AddressBookID != "alice-book" {
		t.Fatalf("source = %+v", source)
	}
	card := fake.card("n1")
	if name, _ := card["name"].(map[string]any); name["full"] != "alice" || card["addressBookIds"].(map[string]any)["B1"] != true {
		t.Fatalf("card = %+v", card)
	}

	// Saving the same details again sends nothing; a change sends only it.
	sets := len(fake.sets)
	seedUserContactPush(t, f, "alice", "jmap")
	if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); err != nil || n != 1 || len(fake.sets) != sets {
		t.Fatalf("unchanged push = %d, %v, %d sets", n, err, len(fake.sets)-sets)
	}
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		if _, err := db.SaveContact(t.Context(), "alice", models.Contact{ID: id, Email: "same@example.com", Name: "alice", Phone: "alice", PhoneLabel: "work", Title: "Archivist", GoferSyncEnabled: true, SaveTargets: []string{"account:" + alice.ID}}); err != nil {
			return err
		}
		_, err := db.EnqueueContactSyncOperation(t.Context(), "alice", models.Contact{ID: id, Email: "stale@example.com"}, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := f.h.ProcessUserContactSyncOperations(t.Context(), "alice", 1); err != nil || n != 1 {
		t.Fatal("owned update", n, err)
	}
	if last := fake.sets[len(fake.sets)-1]; len(last.Update["n1"]) != 1 || last.Update["n1"]["titles"] == nil {
		t.Fatalf("update = %+v, want titles only", last)
	}

	if _, err := f.h.deleteUserContact(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	if fake.card("n1") != nil || remote() != nil {
		t.Fatal("the delete left the card behind")
	}
}

// A JMAP account's contacts make an address book with AddressBook/set, and
// discovery answers with it listed and named as the new one.
func TestUserJMAPContactSetupCreatesAddressBook(t *testing.T) {
	fake := &jmapContactsTestServer{password: "synthetic"}
	fake.set("s1", map[string]string{}, nil)
	f := newUserContactPushFixture(t, "jmap", fake.handler())
	alice := f.accounts["alice"]
	form := url.Values{"provider": {"jmap"}, "create_name": {"Neighbours"}}
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/"+alice.ID+"/contacts/sync/discover", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", alice.ID)
	req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "alice"}))
	rec := httptest.NewRecorder()
	f.h.handleUserDiscoverAccountContactSync(rec, req)
	var result struct {
		AddressBooks []models.ContactAddressBook `json:"address_books"`
		Created      string                      `json:"created"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("discover = %d %s", rec.Code, rec.Body.String())
	}
	if len(fake.books) != 1 || fake.books[0] != "Neighbours" || result.Created != "NB0" || len(result.AddressBooks) != 3 {
		t.Fatalf("books %v, result %+v", fake.books, result)
	}
}
