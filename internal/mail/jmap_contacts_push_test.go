package mail

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// cardStore is a JMAP server that keeps the cards it is sent, applies
// patches one property deep, and records each ContactCard/set it gets.
type cardStore struct {
	mu    sync.Mutex
	cards map[string]map[string]any
	sets  []map[string]json.RawMessage
	next  int
	// refuseUID is a uid whose cards search doesn't find, as on a server
	// that hasn't indexed them yet, and whose creation is refused.
	refuseUID string
}

func (f *cardStore) start(t *testing.T) *models.AccountConfig {
	t.Helper()
	if f.cards == nil {
		f.cards = map[string]map[string]any{}
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/jmap" {
			_, _ = io.WriteString(w, `{"capabilities": {"urn:ietf:params:jmap:core": {}, "urn:ietf:params:jmap:contacts": {}}, "accounts": {"a1": {"name": "nadia"}},
			  "primaryAccounts": {"urn:ietf:params:jmap:contacts": "a1"}, "apiUrl": "`+server.URL+`/api/", "state": "s"}`)
			return
		}
		var request struct {
			MethodCalls [][]json.RawMessage `json:"methodCalls"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		var name string
		_ = json.Unmarshal(request.MethodCalls[0][0], &name)
		f.mu.Lock()
		answer := f.answer(name, request.MethodCalls[0][1])
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{name, answer, "0"}}, "sessionState": "s"})
	}))
	t.Cleanup(server.Close)
	return &models.AccountConfig{Provider: "jmap", JMAPSessionURL: server.URL + "/.well-known/jmap", Username: "nadia@example.test", JMAPAllowPlaintext: true, JMAPAllowPrivate: true}
}

func (f *cardStore) answer(name string, raw json.RawMessage) any {
	var args struct {
		IDs    []string                  `json:"ids"`
		Filter map[string]string         `json:"filter"`
		Create map[string]map[string]any `json:"create"`
		Update map[string]map[string]any `json:"update"`
	}
	_ = json.Unmarshal(raw, &args)
	switch name {
	case "ContactCard/get":
		list := []any{}
		for _, id := range args.IDs {
			if card, ok := f.cards[id]; ok {
				list = append(list, card)
			}
		}
		return map[string]any{"accountId": "a1", "state": "s1", "list": list, "notFound": []string{}}
	case "ContactCard/query":
		ids := []string{}
		for id, card := range f.cards {
			if card["uid"] == args.Filter["uid"] && card["uid"] != f.refuseUID {
				ids = append(ids, id)
			}
		}
		return map[string]any{"accountId": "a1", "ids": ids, "position": 0}
	case "ContactCard/set":
		var whole map[string]json.RawMessage
		_ = json.Unmarshal(raw, &whole)
		f.sets = append(f.sets, whole)
		result := map[string]any{"accountId": "a1", "newState": "s2"}
		created, updated, notUpdated, destroyed, notDestroyed := map[string]any{}, map[string]any{}, map[string]any{}, []string{}, map[string]any{}
		notCreated := map[string]any{}
		for creation, card := range args.Create {
			if card["uid"] == f.refuseUID {
				for id, existing := range f.cards {
					if existing["uid"] == f.refuseUID {
						notCreated[creation] = map[string]any{"type": "alreadyExists", "existingId": id}
					}
				}
				continue
			}
			f.next++
			id := "card" + strconv.Itoa(f.next)
			card["id"] = id
			f.cards[id] = card
			created[creation] = map[string]any{"id": id}
		}
		for id, patch := range args.Update {
			card, ok := f.cards[id]
			if !ok {
				notUpdated[id] = map[string]any{"type": "notFound"}
				continue
			}
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
			updated[id] = nil
		}
		var destroy struct {
			Destroy []string `json:"destroy"`
		}
		_ = json.Unmarshal(raw, &destroy)
		for _, id := range destroy.Destroy {
			if _, ok := f.cards[id]; !ok {
				notDestroyed[id] = map[string]any{"type": "notFound"}
				continue
			}
			delete(f.cards, id)
			destroyed = append(destroyed, id)
		}
		result["notCreated"] = notCreated
		result["created"], result["updated"], result["notUpdated"], result["destroyed"], result["notDestroyed"] = created, updated, notUpdated, destroyed, notDestroyed
		return result
	}
	return map[string]any{}
}

func TestPushJMAPContactCreatesThenPatchesOnlyWhatChanged(t *testing.T) {
	store := &cardStore{}
	cfg := store.start(t)
	ctx := context.Background()
	contact := models.Contact{Name: "Iris Vandermeer", Email: "iris@example.test", Phone: "+1 555 0142", PhoneLabel: "mobile"}
	id, err := PushJMAPContact(ctx, cfg, "secret", "book-home", "", "urn:uuid:iris", contact)
	if err != nil || id == "" {
		t.Fatalf("create = %q, %v", id, err)
	}
	card := store.cards[id]
	if card["uid"] != "urn:uuid:iris" || card["@type"] != "Card" || card["addressBookIds"].(map[string]any)["book-home"] != true {
		t.Fatalf("created card = %+v", card)
	}
	// Details Gofer doesn't keep stay on the card.
	card["anniversaries"] = map[string]any{"a": map[string]any{"kind": "birth", "date": "1990-04-02"}}

	contact.Organization = "Lantern Works"
	again, err := PushJMAPContact(ctx, cfg, "secret", "book-home", id, "urn:uuid:iris", contact)
	if err != nil || again != id {
		t.Fatalf("update = %q, %v", again, err)
	}
	var update map[string]map[string]any
	_ = json.Unmarshal(store.sets[len(store.sets)-1]["update"], &update)
	if len(update[id]) != 1 || update[id]["organizations"] == nil {
		t.Fatalf("patch = %+v, want organizations only", update[id])
	}
	if store.cards[id]["anniversaries"] == nil {
		t.Error("the update dropped the birthday")
	}

	sets := len(store.sets)
	if _, err := PushJMAPContact(ctx, cfg, "secret", "book-home", id, "urn:uuid:iris", contact); err != nil || len(store.sets) != sets {
		t.Errorf("an unchanged contact sent %d more /set calls, %v", len(store.sets)-sets, err)
	}
}

func TestPushJMAPContactFindsACardWhoseCreationReplyWasLost(t *testing.T) {
	store := &cardStore{cards: map[string]map[string]any{
		"earlier": {"id": "earlier", "uid": "urn:uuid:omar", "addressBookIds": map[string]any{"book-home": true}, "name": map[string]any{"full": "Omar Lindqvist"}},
	}}
	cfg := store.start(t)
	id, err := PushJMAPContact(context.Background(), cfg, "secret", "book-home", "", "urn:uuid:omar", models.Contact{Name: "Omar Lindqvist", Email: "omar@example.test"})
	if err != nil || id != "earlier" || len(store.cards) != 1 {
		t.Fatalf("push = %q, %v; cards %+v", id, err, store.cards)
	}
}

func TestPushJMAPContactUpdatesTheCardTheServerSaysExists(t *testing.T) {
	store := &cardStore{refuseUID: "urn:uuid:lena", cards: map[string]map[string]any{
		"older": {"id": "older", "uid": "urn:uuid:lena", "addressBookIds": map[string]any{"book-home": true}, "name": map[string]any{"full": "Lena Ostrova"}},
	}}
	cfg := store.start(t)
	id, err := PushJMAPContact(context.Background(), cfg, "secret", "book-home", "", "urn:uuid:lena", models.Contact{Name: "Lena Ostrova", Email: "lena@example.test"})
	if err != nil || id != "older" || len(store.cards) != 1 || store.cards["older"]["emails"] == nil {
		t.Fatalf("push = %q, %v; cards %+v", id, err, store.cards)
	}
}

func TestPushJMAPContactRemakesACardDeletedOnTheServer(t *testing.T) {
	store := &cardStore{}
	cfg := store.start(t)
	id, err := PushJMAPContact(context.Background(), cfg, "secret", "book-home", "vanished", "urn:uuid:pia", models.Contact{Name: "Pia Moreau", Email: "pia@example.test"})
	if err != nil || id == "vanished" || store.cards[id] == nil {
		t.Fatalf("push = %q, %v", id, err)
	}
}

func TestPushJMAPContactPutsTheCardBackInTheBook(t *testing.T) {
	store := &cardStore{cards: map[string]map[string]any{
		"c1": {"id": "c1", "addressBookIds": map[string]any{"book-old": true}, "name": map[string]any{"full": "Tomas Reyes"}, "emails": map[string]any{"e": map[string]any{"address": "tomas@example.test"}}},
	}}
	cfg := store.start(t)
	if _, err := PushJMAPContact(context.Background(), cfg, "secret", "book-home", "c1", "", models.Contact{Name: "Tomas Reyes", Email: "tomas@example.test"}); err != nil {
		t.Fatal(err)
	}
	if books := store.cards["c1"]["addressBookIds"].(map[string]any); books["book-home"] != true || books["book-old"] != true {
		t.Errorf("address books = %+v", books)
	}
}

func TestDeleteJMAPContactAcceptsACardAlreadyGone(t *testing.T) {
	store := &cardStore{cards: map[string]map[string]any{"c1": {"id": "c1"}}}
	cfg := store.start(t)
	ctx := context.Background()
	if err := DeleteJMAPContact(ctx, cfg, "secret", "c1"); err != nil || len(store.cards) != 0 {
		t.Fatalf("delete = %v, cards %+v", err, store.cards)
	}
	if err := DeleteJMAPContact(ctx, cfg, "secret", "c1"); err != nil {
		t.Errorf("deleting again = %v", err)
	}
}

// TestPushJMAPContactLiveServer creates, finds again, updates and deletes a
// card on a real server, and skips unless GOFER_JMAP_TEST_URL, _USER and
// _PASSWORD are set.
func TestPushJMAPContactLiveServer(t *testing.T) {
	sessionURL, user, password := os.Getenv("GOFER_JMAP_TEST_URL"), os.Getenv("GOFER_JMAP_TEST_USER"), os.Getenv("GOFER_JMAP_TEST_PASSWORD")
	if sessionURL == "" || user == "" || password == "" {
		t.Skip("GOFER_JMAP_TEST_URL, GOFER_JMAP_TEST_USER and GOFER_JMAP_TEST_PASSWORD not set")
	}
	ctx := context.Background()
	cfg := &models.AccountConfig{Provider: "jmap", JMAPSessionURL: sessionURL, Username: user, JMAPAllowPlaintext: true, JMAPAllowPrivate: true}
	books, err := JMAPAddressBooks(ctx, cfg, password)
	if err != nil || len(books) == 0 {
		t.Fatalf("books = %+v, %v", books, err)
	}
	book := books[0].ID
	for _, b := range books {
		if b.IsDefault {
			book = b.ID
		}
	}
	uid := "urn:uuid:5f0c1d2e-7a41-4b8e-9c3a-0d6e2f4a8b10"
	contact := models.Contact{Name: "Selma Achterberg", Email: "selma.achterberg@example.test", EmailLabel: "work"}
	id, err := PushJMAPContact(ctx, cfg, password, book, "", uid, contact)
	if err != nil || id == "" {
		t.Fatalf("create = %q, %v", id, err)
	}
	t.Cleanup(func() { _ = DeleteJMAPContact(ctx, cfg, password, id) })

	client, session, account, err := jmapContactsSession(ctx, cfg, password)
	if err != nil {
		t.Fatal(err)
	}
	// Some servers index a new card for search a moment after creating it.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if ids, err := client.FindContactCards(ctx, session, account, uid); err != nil || len(ids) > 0 || time.Now().After(deadline) {
			break
		}
	}
	found, err := PushJMAPContact(ctx, cfg, password, book, "", uid, contact)
	if err != nil || found != id {
		t.Fatalf("push by uid = %q, %v; want %q", found, err, id)
	}
	contact.Phone, contact.PhoneLabel, contact.Title = "+1 555 0190", "mobile", "Cartographer"
	if _, err := PushJMAPContact(ctx, cfg, password, book, id, uid, contact); err != nil {
		t.Fatal(err)
	}
	cards, err := client.GetContactCards(ctx, session, account, []string{id})
	if err != nil || len(cards.List) != 1 {
		t.Fatalf("get = %+v, %v", cards, err)
	}
	got, _, _ := JMAPContactFromCard(cards.List[0])
	if got.Name != contact.Name || got.Email != contact.Email || got.Phone != contact.Phone || got.PhoneLabel != "mobile" || got.Title != "Cartographer" {
		t.Fatalf("card on the server = %+v", got)
	}

	if err := DeleteJMAPContact(ctx, cfg, password, id); err != nil {
		t.Fatal(err)
	}
	if cards, err := client.GetContactCards(ctx, session, account, []string{id}); err != nil || len(cards.List) != 0 {
		t.Fatalf("after delete = %+v, %v", cards, err)
	}
}
