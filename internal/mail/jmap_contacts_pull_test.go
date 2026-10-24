package mail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// fakeContactsServer is a JMAP server whose contacts are cards, keyed by id.
// changes answers ContactCard/changes; nil answers cannotCalculateChanges.
type fakeContactsServer struct {
	cards    map[string]string
	changes  map[string]any
	contacts bool
	queries  int
}

func (f *fakeContactsServer) start(t *testing.T) *models.AccountConfig {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "nadia@example.test" || pass != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/.well-known/jmap":
			capabilities := `"urn:ietf:params:jmap:core": {"maxObjectsInGet": 2}, "urn:ietf:params:jmap:mail": {}`
			primary := `"urn:ietf:params:jmap:mail": "a1"`
			if f.contacts {
				capabilities += `, "urn:ietf:params:jmap:contacts": {}`
				primary += `, "urn:ietf:params:jmap:contacts": "a1"`
			}
			_, _ = io.WriteString(w, `{"capabilities": {`+capabilities+`}, "accounts": {"a1": {"name": "nadia"}},
			  "primaryAccounts": {`+primary+`}, "apiUrl": "`+server.URL+`/api/",
			  "downloadUrl": "`+server.URL+`/download/{accountId}/{blobId}/{name}?type={type}", "state": "s"}`)
		case strings.HasPrefix(r.URL.Path, "/download/a1/photo-blob/"):
			_, _ = w.Write([]byte("PNGDATA"))
		case r.URL.Path == "/api/":
			var request struct {
				MethodCalls [][]json.RawMessage `json:"methodCalls"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			var name string
			var args struct {
				IDs      *[]string `json:"ids"`
				Position int       `json:"position"`
				Limit    int       `json:"limit"`
			}
			_ = json.Unmarshal(request.MethodCalls[0][0], &name)
			_ = json.Unmarshal(request.MethodCalls[0][1], &args)
			_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{f.answer(name, args.IDs, args.Position, args.Limit)}, "sessionState": "s"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return &models.AccountConfig{Provider: "jmap", JMAPSessionURL: server.URL + "/.well-known/jmap", Username: "nadia@example.test", JMAPAllowPlaintext: true, JMAPAllowPrivate: true}
}

func (f *fakeContactsServer) answer(name string, ids *[]string, position, limit int) []any {
	ordered := make([]string, 0, len(f.cards))
	for id := range f.cards {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	switch name {
	case "ContactCard/get":
		list := []json.RawMessage{}
		notFound := []string{}
		for _, id := range *ids {
			if card, ok := f.cards[id]; ok {
				list = append(list, json.RawMessage(card))
			} else {
				notFound = append(notFound, id)
			}
		}
		return []any{name, map[string]any{"accountId": "a1", "state": "s9", "list": list, "notFound": notFound}, "0"}
	case "ContactCard/query":
		f.queries++
		end := min(position+limit, len(ordered))
		return []any{name, map[string]any{"accountId": "a1", "ids": ordered[position:end], "position": position, "total": len(ordered)}, "0"}
	case "ContactCard/changes":
		if f.changes == nil {
			return []any{"error", map[string]any{"type": "cannotCalculateChanges"}, "0"}
		}
		return []any{name, f.changes, "0"}
	case "AddressBook/get":
		return []any{name, map[string]any{"accountId": "a1", "state": "b1", "list": []any{
			map[string]any{"id": "book-home", "name": "Personal", "isDefault": true},
			map[string]any{"id": "book-shared", "name": "Shared"},
		}}, "0"}
	}
	return []any{"error", map[string]any{"type": "unknownMethod"}, "0"}
}

func threeCards() map[string]string {
	return map[string]string{
		"c1": `{"id": "c1", "addressBookIds": {"book-home": true}, "name": {"full": "Nadia Ferro"},
		        "emails": {"e": {"address": "nadia.ferro@example.test"}},
		        "media": {"m": {"kind": "photo", "blobId": "photo-blob", "mediaType": "image/png"}}}`,
		"c2": `{"id": "c2", "addressBookIds": {"book-shared": true}, "name": {"full": "Office Plants"}, "emails": {"e": {"address": "plants@example.test"}}}`,
		"c3": `{"id": "c3", "kind": "group", "addressBookIds": {"book-home": true}, "name": {"full": "Choir"}}`,
	}
}

func TestPullJMAPContactsListsTheSyncedBooks(t *testing.T) {
	server := &fakeContactsServer{cards: threeCards(), contacts: true}
	cfg := server.start(t)
	page, err := PullJMAPContacts(context.Background(), cfg, "secret", "", []string{"book-home"})
	if err != nil {
		t.Fatalf("PullJMAPContacts: %v", err)
	}
	if !page.Full || page.State != "s9" || server.queries < 2 {
		t.Errorf("page full=%v state=%q after %d queries", page.Full, page.State, server.queries)
	}
	if len(page.Contacts) != 1 || page.Contacts[0].ID != "c1" || page.Contacts[0].AddressBookID != "book-home" {
		t.Fatalf("contacts = %+v", page.Contacts)
	}
	if got, want := page.Contacts[0].Contact.AvatarURL, "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("PNGDATA")); got != want {
		t.Errorf("photo = %q, want %q", got, want)
	}
	// The group is in a synced book but not a contact; the card in another
	// book counts as removed.
	if !slices.Equal(page.Seen, []string{"c1", "c3"}) || !slices.Equal(page.Removed, []string{"c2"}) {
		t.Errorf("seen %v removed %v", page.Seen, page.Removed)
	}
}

func TestPullJMAPContactsFollowsChanges(t *testing.T) {
	server := &fakeContactsServer{cards: threeCards(), contacts: true, changes: map[string]any{
		"accountId": "a1", "oldState": "s1", "newState": "s2", "hasMoreChanges": false,
		"created": []string{"c1"}, "updated": []string{"c2", "gone"}, "destroyed": []string{"gone", "c7"},
	}}
	cfg := server.start(t)
	page, err := PullJMAPContacts(context.Background(), cfg, "secret", "s1", []string{"book-home"})
	if err != nil {
		t.Fatalf("PullJMAPContacts: %v", err)
	}
	if page.Full || page.State != "s2" || server.queries != 0 {
		t.Errorf("page full=%v state=%q queries=%d, want changes only", page.Full, page.State, server.queries)
	}
	if len(page.Contacts) != 1 || page.Contacts[0].ID != "c1" {
		t.Errorf("contacts = %+v", page.Contacts)
	}
	if removed := slices.Sorted(slices.Values(page.Removed)); !slices.Equal(removed, []string{"c2", "c7", "gone"}) {
		t.Errorf("removed = %v", removed)
	}
}

func TestPullJMAPContactsListsAgainWhenChangesAreLost(t *testing.T) {
	server := &fakeContactsServer{cards: threeCards(), contacts: true}
	cfg := server.start(t)
	page, err := PullJMAPContacts(context.Background(), cfg, "secret", "ancient", []string{"book-home", "book-shared"})
	if err != nil {
		t.Fatalf("PullJMAPContacts: %v", err)
	}
	if !page.Full || len(page.Contacts) != 2 || len(page.Removed) != 0 {
		t.Errorf("page = %+v", page)
	}
}

func TestPullJMAPContactsNeedsTheContactsCapability(t *testing.T) {
	server := &fakeContactsServer{cards: threeCards()}
	cfg := server.start(t)
	if _, err := PullJMAPContacts(context.Background(), cfg, "secret", "", []string{"book-home"}); !errors.Is(err, ErrJMAPNoContacts) {
		t.Errorf("err = %v, want ErrJMAPNoContacts", err)
	}
	if _, err := JMAPAddressBooks(context.Background(), cfg, "secret"); !errors.Is(err, ErrJMAPNoContacts) {
		t.Errorf("address books err = %v, want ErrJMAPNoContacts", err)
	}
}

func TestJMAPAddressBooksListsBooks(t *testing.T) {
	server := &fakeContactsServer{contacts: true}
	cfg := server.start(t)
	books, err := JMAPAddressBooks(context.Background(), cfg, "secret")
	if err != nil || len(books) != 2 || !books[0].IsDefault || books[1].Name != "Shared" {
		t.Errorf("books = %+v, %v", books, err)
	}
}

// TestPullJMAPContactsLiveServer follows a card through a real server's
// changes, and skips unless GOFER_JMAP_TEST_URL, _USER and _PASSWORD are set.
func TestPullJMAPContactsLiveServer(t *testing.T) {
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
	full, err := PullJMAPContacts(ctx, cfg, password, "", []string{book})
	if err != nil || !full.Full || full.State == "" {
		t.Fatalf("full pull = %+v, %v", full, err)
	}

	client, session, account, err := jmapContactsSession(ctx, cfg, password)
	if err != nil {
		t.Fatal(err)
	}
	card := JMAPCardFromContact(models.Contact{Name: "Wren Castellano", Email: "wren.castellano@example.test", EmailLabel: "work", Phone: "+1 555 0177", PhoneLabel: "mobile"})
	card.AddressBookIDs = map[string]bool{book: true}
	created, err := client.SetContactCards(ctx, session, jmap.ContactCardSet{AccountID: account, Create: map[string]jmap.ContactCard{"new": card}})
	if err != nil || created.Created["new"].ID == "" {
		t.Fatalf("create = %+v, %v", created, err)
	}
	id := created.Created["new"].ID
	t.Cleanup(func() { _, _ = client.SetContactCards(ctx, session, jmap.ContactCardSet{AccountID: account, Destroy: []string{id}}) })

	changed, err := PullJMAPContacts(ctx, cfg, password, full.State, []string{book})
	if err != nil || changed.Full {
		t.Fatalf("changes pull = %+v, %v", changed, err)
	}
	var got *JMAPContact
	for i := range changed.Contacts {
		if changed.Contacts[i].ID == id {
			got = &changed.Contacts[i]
		}
	}
	if got == nil || got.Contact.Email != "wren.castellano@example.test" || got.Contact.EmailLabel != "work" || got.Contact.PhoneLabel != "mobile" || got.AddressBookID != book {
		t.Fatalf("pulled card = %+v", got)
	}

	if _, err := client.SetContactCards(ctx, session, jmap.ContactCardSet{AccountID: account, Destroy: []string{id}}); err != nil {
		t.Fatal(err)
	}
	gone, err := PullJMAPContacts(ctx, cfg, password, changed.State, []string{book})
	if err != nil || !slices.Contains(gone.Removed, id) {
		t.Fatalf("pull after destroy = %+v, %v", gone, err)
	}
}
