package jmap

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// contactServer answers every request with answer and records the request's
// capabilities and method calls.
func contactServer(t *testing.T, answer string, using *[]string, calls *[][]json.RawMessage) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Using       []string            `json:"using"`
			MethodCalls [][]json.RawMessage `json:"methodCalls"`
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Errorf("request is not JSON: %v", err)
		}
		*using, *calls = request.Using, request.MethodCalls
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestSetContactCardsCreatesCardsAndPatches(t *testing.T) {
	var using []string
	var calls [][]json.RawMessage
	server := contactServer(t, `{"methodResponses": [["ContactCard/set", {
	  "accountId": "a1", "oldState": "s1", "newState": "s2",
	  "created": {"new1": {"id": "c9", "uid": "urn:uuid:5f0c"}},
	  "updated": {"c1": null},
	  "notCreated": {"new2": {"type": "invalidProperties", "properties": ["emails/e1/address"]}}}, "0"]], "sessionState": "x"}`, &using, &calls)

	client := &Client{HTTP: server.Client()}
	session := &Session{APIURL: server.URL + "/jmap/"}
	result, err := client.SetContactCards(context.Background(), session, ContactCardSet{
		AccountID: "a1",
		Create: map[string]ContactCard{"new1": {
			Kind:           "individual",
			AddressBookIDs: map[string]bool{"b1": true},
			Name:           &ContactName{Full: "Tomas Reyes"},
			Emails:         map[string]ContactEmail{"e1": {Address: "tomas@example.test", Pref: 1}},
		}},
		Update: map[string]PatchObject{"c1": {"notes": nil}},
	})
	if err != nil {
		t.Fatalf("SetContactCards: %v", err)
	}
	if !slices.Contains(using, CapabilityContacts) {
		t.Errorf("using = %v, want the contacts capability", using)
	}
	var args map[string]any
	_ = json.Unmarshal(calls[0][1], &args)
	created := args["create"].(map[string]any)["new1"].(map[string]any)
	if created["@type"] != "Card" || created["version"] != "1.0" || created["kind"] != "individual" {
		t.Errorf("new card = %v, want a JSContact Card", created)
	}
	if _, ok := created["id"]; ok {
		t.Errorf("new card sent an id: %v", created)
	}
	if _, ok := created["phones"]; ok {
		t.Errorf("new card sent empty phones: %v", created)
	}
	if update := args["update"].(map[string]any)["c1"].(map[string]any); len(update) != 1 || update["notes"] != nil {
		t.Errorf("update = %v", update)
	}
	if result.Created["new1"].ID != "c9" || result.NotCreated["new2"].Type != "invalidProperties" {
		t.Errorf("result = %+v", result)
	}
}

func TestContactCardChangesUseTheContactsCapability(t *testing.T) {
	var using []string
	var calls [][]json.RawMessage
	server := contactServer(t, `{"methodResponses": [["ContactCard/changes", {
	  "accountId": "a1", "oldState": "s1", "newState": "s2", "hasMoreChanges": false,
	  "created": ["c3"], "updated": ["c1"], "destroyed": ["c2"]}, "0"]], "sessionState": "x"}`, &using, &calls)

	client := &Client{HTTP: server.Client()}
	changes, err := client.GetContactCardChanges(context.Background(), &Session{APIURL: server.URL}, "a1", "s1", 0)
	if err != nil {
		t.Fatalf("GetContactCardChanges: %v", err)
	}
	var name string
	_ = json.Unmarshal(calls[0][0], &name)
	if name != "ContactCard/changes" || !slices.Contains(using, CapabilityContacts) || slices.Contains(using, CapabilityMail) {
		t.Errorf("method %s using %v", name, using)
	}
	if changes.NewState != "s2" || !slices.Equal(changes.Destroyed, []string{"c2"}) {
		t.Errorf("changes = %+v", changes)
	}
}

// TestContactCardsLiveServer creates a card on a real server, reads it back,
// changes it, finds it in the changes since then and destroys it.
func TestContactCardsLiveServer(t *testing.T) {
	client, session := liveSession(t)
	ctx := context.Background()
	account := session.PrimaryAccount(CapabilityContacts)
	if account == "" {
		t.Skip("the server offers no contacts account")
	}
	books, err := client.GetAddressBooks(ctx, session, account)
	if err != nil {
		t.Fatalf("GetAddressBooks: %v", err)
	}
	var book string
	for _, b := range books.List {
		if b.IsDefault || book == "" {
			book = b.ID
		}
	}
	if book == "" {
		t.Fatal("no address book")
	}

	created, err := client.SetContactCards(ctx, session, ContactCardSet{AccountID: account, Create: map[string]ContactCard{"new": {
		Kind:           "individual",
		AddressBookIDs: map[string]bool{book: true},
		Name:           &ContactName{Full: "Marisol Quintero", Components: []ContactNameComponent{{Kind: "given", Value: "Marisol"}, {Kind: "surname", Value: "Quintero"}}},
		Emails:         map[string]ContactEmail{"e1": {Address: "marisol.quintero@example.test", Contexts: map[string]bool{"work": true}, Pref: 1}},
		Phones:         map[string]ContactPhone{"p1": {Number: "+1 555 0142", Features: map[string]bool{"mobile": true}}},
		Notes:          map[string]ContactNote{"n1": {Note: "Met at the library fair"}},
	}}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if failure, ok := created.NotCreated["new"]; ok {
		t.Fatalf("create refused: %v %v", failure.Error(), failure.Properties)
	}
	id := created.Created["new"].ID
	t.Cleanup(func() {
		_, _ = client.SetContactCards(ctx, session, ContactCardSet{AccountID: account, Destroy: []string{id}})
	})

	got, err := client.GetContactCards(ctx, session, account, []string{id})
	if err != nil || len(got.List) != 1 {
		t.Fatalf("get: %v %+v", err, got)
	}
	card := got.List[0]
	if card.Name == nil || card.Emails == nil || card.Phones == nil || !card.AddressBookIDs[book] {
		t.Fatalf("card read back = %+v", card)
	}

	updated, err := client.SetContactCards(ctx, session, ContactCardSet{AccountID: account, Update: map[string]PatchObject{id: {
		"notes":         nil,
		"organizations": map[string]ContactOrganization{"o1": {Name: "Harbor Books"}},
	}}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if failure, ok := updated.NotUpdated[id]; ok {
		t.Fatalf("update refused: %v %v", failure.Error(), failure.Properties)
	}
	changes, err := client.GetContactCardChanges(ctx, session, account, got.State, 0)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if !slices.Contains(changes.Updated, id) && !slices.Contains(changes.Created, id) {
		t.Fatalf("changes since %s = %+v, want %s", got.State, changes, id)
	}

	again, err := client.GetContactCards(ctx, session, account, []string{id})
	if err != nil || len(again.List) != 1 {
		t.Fatalf("get after update: %v", err)
	}
	if again.List[0].Notes != nil || again.List[0].Organizations["o1"].Name != "Harbor Books" || again.List[0].Phones == nil {
		t.Fatalf("card after update = %+v", again.List[0])
	}
	t.Logf("card %s in book %s: %+v", id, book, again.List[0])
}
