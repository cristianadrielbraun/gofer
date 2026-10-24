package mail

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestJMAPContactFromCardReadsDetailsAndLabels(t *testing.T) {
	var card jmap.ContactCard
	if err := json.Unmarshal([]byte(`{
	  "id": "c1", "uid": "urn:uuid:1d2e", "kind": "individual", "addressBookIds": {"b1": true},
	  "name": {"components": [{"kind": "given", "value": "Ines"}, {"kind": "separator", "value": " "}, {"kind": "surname", "value": "Varga"}]},
	  "emails": {
	    "home": {"address": "ines@example.test", "contexts": {"private": true}, "pref": 2},
	    "work": {"address": "ines.varga@example.test", "contexts": {"work": true}, "pref": 1},
	    "old": {"address": "iv@example.test", "label": "Assistant"},
	    "bad": {"address": "not an address"}
	  },
	  "phones": {
	    "p2": {"number": "+1 555 0100", "features": {"fax": true}},
	    "p1": {"number": "+1 555 0199", "features": {"mobile": true, "voice": true}, "pref": 1}
	  },
	  "organizations": {"o1": {"name": "Copper Kettle Studio"}},
	  "titles": {"r1": {"name": "Treasurer", "kind": "role"}, "t1": {"name": "Illustrator", "kind": "title"}},
	  "notes": {"n1": {"note": "Prefers mornings"}, "n2": {"note": "Allergic to cats"}},
	  "media": {"m1": {"kind": "logo", "uri": "https://example.test/logo.png"}, "m2": {"kind": "photo", "blobId": "B7", "mediaType": "image/jpeg"}},
	  "addresses": {"a1": {"full": "12 Pier Road"}}
	}`), &card); err != nil {
		t.Fatal(err)
	}
	contact, photo, ok := JMAPContactFromCard(card)
	if !ok {
		t.Fatal("card skipped")
	}
	want := models.Contact{
		Name:                  "Ines Varga",
		Email:                 "ines.varga@example.test",
		EmailLabel:            "work",
		AdditionalEmails:      []string{"ines@example.test", "iv@example.test"},
		AdditionalEmailLabels: []string{"home", "assistant"},
		Phone:                 "+1 555 0199",
		PhoneLabel:            "mobile",
		AdditionalPhones:      []string{"+1 555 0100"},
		AdditionalPhoneLabels: []string{"fax"},
		Organization:          "Copper Kettle Studio",
		Title:                 "Illustrator",
		Notes:                 "Prefers mornings\n\nAllergic to cats",
	}
	if !reflect.DeepEqual(contact, want) {
		t.Errorf("contact =\n%+v\nwant\n%+v", contact, want)
	}
	if photo.BlobID != "B7" || photo.MediaType != "image/jpeg" {
		t.Errorf("photo = %+v", photo)
	}
}

func TestJMAPContactFromCardSkipsGroupsAndEmptyCards(t *testing.T) {
	group := jmap.ContactCard{Kind: "group", Name: &jmap.ContactName{Full: "Book club"}}
	if _, _, ok := JMAPContactFromCard(group); ok {
		t.Error("group read as a contact")
	}
	if _, _, ok := JMAPContactFromCard(jmap.ContactCard{Kind: "individual"}); ok {
		t.Error("empty card read as a contact")
	}
	named := jmap.ContactCard{Emails: map[string]jmap.ContactEmail{"e": {Address: "lone@example.test"}}}
	if contact, _, ok := JMAPContactFromCard(named); !ok || contact.Name != "lone@example.test" || contact.EmailLabel != "primary" {
		t.Errorf("unnamed card = %+v %v", contact, ok)
	}
	withURI := jmap.ContactCard{Name: &jmap.ContactName{Full: "Pia"}, Media: map[string]jmap.ContactMedia{"m": {Kind: "photo", URI: "data:image/png;base64,iVBORw0KGgo="}}}
	if contact, photo, _ := JMAPContactFromCard(withURI); contact.AvatarURL != "data:image/png;base64,iVBORw0KGgo=" || photo.BlobID != "" {
		t.Errorf("photo URI = %q, blob %+v", contact.AvatarURL, photo)
	}
}

func TestJMAPCardRoundTripsAContact(t *testing.T) {
	contact := models.Contact{
		Name:                  "Oskar van der Berg",
		Email:                 "oskar@example.test",
		EmailLabel:            "home",
		AdditionalEmails:      []string{"o.berg@example.test", "oskar.assist@example.test"},
		AdditionalEmailLabels: []string{"work", "assistant"},
		Phone:                 "+31 20 555 0101",
		PhoneLabel:            "mobile",
		AdditionalPhones:      []string{"+31 20 555 0102"},
		AdditionalPhoneLabels: []string{"work"},
		Organization:          "Canal Ferries",
		Title:                 "Dispatcher",
		Notes:                 "Call after 5",
	}
	card := JMAPCardFromContact(contact)
	if card.Name.Full != "Oskar van der Berg" || !slices.Equal(card.Name.Components, []jmap.ContactNameComponent{{Kind: "given", Value: "Oskar van der"}, {Kind: "surname", Value: "Berg"}}) {
		t.Errorf("name = %+v", card.Name)
	}
	back, _, ok := JMAPContactFromCard(card)
	if !ok || !reflect.DeepEqual(back, contact) {
		t.Errorf("round trip =\n%+v\nwant\n%+v", back, contact)
	}
	if patch := JMAPContactPatch(card, contact); len(patch) != 0 {
		t.Errorf("unchanged contact patch = %v", patch)
	}
}

func TestJMAPContactPatchReplacesOnlyChangedProperties(t *testing.T) {
	remote := jmap.ContactCard{
		ID:     "c1",
		Name:   &jmap.ContactName{Full: "Leilani Kahale", Components: []jmap.ContactNameComponent{{Kind: "title", Value: "Dr."}, {Kind: "given", Value: "Leilani"}, {Kind: "surname", Value: "Kahale"}}},
		Emails: map[string]jmap.ContactEmail{"x": {Address: "leilani@example.test"}},
		Phones: map[string]jmap.ContactPhone{"y": {Number: "+1 808 555 0170", Features: map[string]bool{"mobile": true}, Label: "Hawaii cell"}},
		Organizations: map[string]jmap.ContactOrganization{
			"z": {Name: "Reef Survey"},
		},
	}
	contact, _, _ := JMAPContactFromCard(remote)

	if patch := JMAPContactPatch(remote, contact); len(patch) != 0 {
		t.Fatalf("unchanged patch = %v", patch)
	}

	contact.Phone = "+1 808 555 0171"
	contact.Organization = ""
	contact.Notes = "Dive season starts in May"
	patch := JMAPContactPatch(remote, contact)
	keys := make([]string, 0, len(patch))
	for key := range patch {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"notes", "organizations", "phones"}) {
		t.Fatalf("patch keys = %v, want notes, organizations and phones only", keys)
	}
	if patch["organizations"] != nil {
		t.Errorf("cleared organization = %v, want null", patch["organizations"])
	}
	phones := patch["phones"].(map[string]jmap.ContactPhone)
	if len(phones) != 1 || phones["p1"].Number != "+1 808 555 0171" || !phones["p1"].Features["mobile"] {
		t.Errorf("phones = %+v", phones)
	}
}

func TestJMAPContactPatchLeavesAnUnnamedCardUnnamed(t *testing.T) {
	remote := jmap.ContactCard{Emails: map[string]jmap.ContactEmail{"e": {Address: "desk@example.test"}}}
	contact := models.Contact{Email: "desk@example.test"}
	if patch := JMAPContactPatch(remote, contact); len(patch) != 0 {
		t.Errorf("patch = %v", patch)
	}
	contact.Name = "Front Desk"
	if patch := JMAPContactPatch(remote, contact); patch["name"] == nil || len(patch) != 1 {
		t.Errorf("naming patch = %v", patch)
	}
}
