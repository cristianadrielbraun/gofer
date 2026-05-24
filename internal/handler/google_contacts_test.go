package handler

import (
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestGoogleContactFromPersonMapsExpandedFields(t *testing.T) {
	contact := googleContactFromPerson(googlePerson{
		Names:          []googleName{{DisplayName: "Jane Doe"}},
		EmailAddresses: []googleEmail{{Value: "jane@example.com", Type: "work"}, {Value: "jane@home.example", Type: "home"}},
		PhoneNumbers:   []googlePhoneNumber{{Value: "+1 555 0100", Type: "mobile"}},
		Organizations:  []googleOrganization{{Name: "Example Inc.", Title: "Product Lead"}},
		Biographies:    []googleBiography{{Value: "Important contact"}},
	})

	if contact.Name != "Jane Doe" || contact.Email != "jane@example.com" || contact.Phone != "+1 555 0100" || contact.Organization != "Example Inc." || contact.Title != "Product Lead" || contact.Notes != "Important contact" {
		t.Fatalf("googleContactFromPerson() = %#v, want expanded fields", contact)
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
	for _, want := range []string{"phoneNumbers", "organizations", "biographies"} {
		if !strings.Contains(fields, want) {
			t.Fatalf("googleContactPersonFields() = %q, missing %q", fields, want)
		}
	}
}
