package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
)

func TestGoogleContactFromPersonMapsExpandedFields(t *testing.T) {
	contact := googleContactFromPerson(googlePerson{
		Names:          []googleName{{DisplayName: "Jane Doe"}},
		EmailAddresses: []googleEmail{{Value: "jane@example.com", Type: "work"}, {Value: "jane@home.example", Type: "home"}},
		PhoneNumbers:   []googlePhoneNumber{{Value: "+1 555 0100", Type: "mobile"}},
		Organizations:  []googleOrganization{{Name: "Example Inc.", Title: "Product Lead"}},
		Biographies:    []googleBiography{{Value: "Important contact"}},
		Photos:         []googlePhoto{{URL: "https://photos.example/default.jpg", Default: true}, {URL: "https://photos.example/jane.jpg"}},
	})

	if contact.Name != "Jane Doe" || contact.Email != "jane@example.com" || contact.Phone != "+1 555 0100" || contact.Organization != "Example Inc." || contact.Title != "Product Lead" || contact.Notes != "Important contact" {
		t.Fatalf("googleContactFromPerson() = %#v, want expanded fields", contact)
	}
	if contact.AvatarURL != "https://photos.example/jane.jpg" {
		t.Fatalf("AvatarURL = %q, want non-default People photo", contact.AvatarURL)
	}
	if contact.EmailLabel != "work" || len(contact.AdditionalEmails) != 1 || contact.AdditionalEmails[0] != "jane@home.example" || contact.AdditionalEmailLabels[0] != "home" {
		t.Fatalf("AdditionalEmails = %#v, want work email", contact.AdditionalEmails)
	}
	if contact.PhoneLabel != "mobile" {
		t.Fatalf("PhoneLabel = %q, want mobile", contact.PhoneLabel)
	}
}

func TestGooglePersonFromContactMapsExpandedFields(t *testing.T) {
	person := googlePersonFromContact(models.Contact{
		Name:                  "Jane Doe",
		Email:                 "jane@example.com",
		EmailLabel:            "work",
		AdditionalEmails:      []string{"jane@home.example"},
		AdditionalEmailLabels: []string{"home"},
		Phone:                 "+1 555 0100",
		PhoneLabel:            "mobile",
		AdditionalPhones:      []string{"+1 555 0101"},
		AdditionalPhoneLabels: []string{"home"},
		Organization:          "Example Inc.",
		Title:                 "Product Lead",
		Notes:                 "Important contact",
	}, "people/c1", "etag")

	if len(person.EmailAddresses) != 2 || person.EmailAddresses[0].Type != "work" || person.EmailAddresses[1].Value != "jane@home.example" || person.EmailAddresses[1].Type != "home" {
		t.Fatalf("EmailAddresses = %#v, want primary and additional email", person.EmailAddresses)
	}
	if len(person.PhoneNumbers) != 2 || person.PhoneNumbers[0].Value != "+1 555 0100" || person.PhoneNumbers[0].Type != "mobile" || person.PhoneNumbers[1].Value != "+1 555 0101" || person.PhoneNumbers[1].Type != "home" {
		t.Fatalf("PhoneNumbers = %#v, want phone", person.PhoneNumbers)
	}
	if len(person.Organizations) != 1 || person.Organizations[0].Name != "Example Inc." || person.Organizations[0].Title != "Product Lead" {
		t.Fatalf("Organizations = %#v, want org/title", person.Organizations)
	}
	if len(person.Biographies) != 1 || person.Biographies[0].Value != "Important contact" {
		t.Fatalf("Biographies = %#v, want notes", person.Biographies)
	}
}

func TestGoogleContactPersonFieldsIncludesExpandedFields(t *testing.T) {
	fields, err := url.QueryUnescape(url.QueryEscape(googleContactPersonFields()))
	if err != nil {
		t.Fatalf("QueryUnescape() error = %v", err)
	}
	for _, want := range []string{"phoneNumbers", "organizations", "biographies", "photos"} {
		if !strings.Contains(fields, want) {
			t.Fatalf("googleContactPersonFields() = %q, missing %q", fields, want)
		}
	}
}

func TestSyncGooglePeopleConnectionsUsesPeopleAPIAndStoresSource(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)

	var sawConnections bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if r.Method != http.MethodGet || r.URL.Path != "/people/me/connections" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		if got := r.URL.Query().Get("pageSize"); got != "1000" {
			t.Fatalf("pageSize = %q, want 1000", got)
		}
		if fields := r.URL.Query().Get("personFields"); !strings.Contains(fields, "emailAddresses") || !strings.Contains(fields, "metadata") || !strings.Contains(fields, "photos") {
			t.Fatalf("personFields = %q, want People API contact fields", fields)
		}
		sawConnections = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"connections": []map[string]any{{
				"resourceName": "people/c1",
				"etag":         "etag-1",
				"names":        []map[string]string{{"displayName": "Jane People"}},
				"emailAddresses": []map[string]string{{
					"value": "jane@example.com",
					"type":  "work",
				}},
				"photos": []map[string]any{{
					"url":     "https://photos.example/default.jpg",
					"default": true,
				}, {
					"url": "https://photos.example/jane.jpg",
				}},
			}},
		})
	}))
	defer server.Close()
	previousBase := googlePeopleAPIBaseURL
	googlePeopleAPIBaseURL = server.URL
	t.Cleanup(func() { googlePeopleAPIBaseURL = previousBase })

	imported, err := h.syncGooglePeopleConnections(ctx, "default", "acc", "gmail-token")
	if err != nil {
		t.Fatalf("syncGooglePeopleConnections() error = %v", err)
	}
	if imported != 1 || !sawConnections {
		t.Fatalf("imported=%d sawConnections=%v, want one People API import", imported, sawConnections)
	}
	contacts, err := db.SearchContacts(ctx, "default", "jane", 10)
	if err != nil || len(contacts) != 1 {
		t.Fatalf("SearchContacts() = %#v, %v; want one imported contact", contacts, err)
	}
	if contacts[0].AvatarURL != "https://photos.example/jane.jpg" {
		t.Fatalf("AvatarURL = %q, want imported People photo", contacts[0].AvatarURL)
	}
	source, err := db.GetContactSource(ctx, "default", contacts[0].ID, providers.ProviderGmail, "acc")
	if err != nil || source == nil {
		t.Fatalf("GetContactSource() = %#v, %v; want Gmail People source", source, err)
	}
	if source.RemoteID != "people/c1" || source.Etag != "etag-1" {
		t.Fatalf("source = %#v, want People resource and etag", source)
	}
}

func TestPushContactToGmailAccountUsesPeopleAPIAndStoresSource(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	contact, err := db.SaveContact(ctx, "default", models.Contact{
		Name:        "Jane Push",
		Email:       "jane@example.com",
		Phone:       "+1 555 0100",
		SaveTargets: []string{"account:acc"},
	})
	if err != nil {
		t.Fatalf("SaveContact() error = %v", err)
	}

	var sawCreate bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if r.Method == http.MethodGet && r.URL.Path == "/people:searchContacts" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"results":[]}`))
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/people:createContact" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		if fields := r.URL.Query().Get("personFields"); !strings.Contains(fields, "emailAddresses") || !strings.Contains(fields, "phoneNumbers") {
			t.Fatalf("personFields = %q, want People API contact fields", fields)
		}
		var payload googlePerson
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode create payload: %v", err)
		}
		if len(payload.EmailAddresses) != 1 || payload.EmailAddresses[0].Value != "jane@example.com" {
			t.Fatalf("payload email addresses = %#v, want Jane email", payload.EmailAddresses)
		}
		if len(payload.PhoneNumbers) != 1 || payload.PhoneNumbers[0].Value != "+1 555 0100" {
			t.Fatalf("payload phone numbers = %#v, want Jane phone", payload.PhoneNumbers)
		}
		sawCreate = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resourceName": "people/new-contact",
			"etag":         "etag-new",
		})
	}))
	defer server.Close()
	previousBase := googlePeopleAPIBaseURL
	googlePeopleAPIBaseURL = server.URL
	t.Cleanup(func() { googlePeopleAPIBaseURL = previousBase })

	if err := h.pushContactToGmailAccount(ctx, "default", contact, "acc"); err != nil {
		t.Fatalf("pushContactToGmailAccount() error = %v", err)
	}
	if !sawCreate {
		t.Fatal("People createContact was not observed")
	}
	source, err := db.GetContactSource(ctx, "default", contact.ID, providers.ProviderGmail, "acc")
	if err != nil || source == nil {
		t.Fatalf("GetContactSource() = %#v, %v; want Gmail People source", source, err)
	}
	if source.RemoteID != "people/new-contact" || source.Etag != "etag-new" {
		t.Fatalf("source = %#v, want created People source", source)
	}
}

func TestContactSyncRequiresExplicitEnablement(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	contact, err := db.SaveContact(ctx, "default", models.Contact{
		Name:        "Jane Disabled",
		Email:       "jane@example.com",
		SaveTargets: []string{"account:acc"},
	})
	if err != nil {
		t.Fatalf("SaveContact() error = %v", err)
	}
	if h.contactNeedsAccountSync(ctx, "default", contact, nil) {
		t.Fatal("disabled contact unexpectedly requested provider sync")
	}
	if err := h.preflightNewContactSyncTargets(ctx, "default", contact, nil); err != nil {
		t.Fatalf("disabled contact ran provider preflight: %v", err)
	}
	contact.GoferSyncEnabled = true
	if !h.contactNeedsAccountSync(ctx, "default", contact, nil) {
		t.Fatal("enabled contact with an account target did not request provider sync")
	}
}
