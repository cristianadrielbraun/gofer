package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestOutlookContactFromGraphMapsExpandedFields(t *testing.T) {
	contact := outlookContactFromGraph(outlookContact{
		ID:          "contact-1",
		DisplayName: "Jane Doe",
		EmailAddresses: []outlookEmailAddress{
			{Name: "Jane Doe", Address: "jane@example.com"},
			{Name: "Jane Work", Address: "jane@work.example"},
		},
		BusinessPhones: []string{"+1 555 0100"},
		HomePhones:     []string{"+1 555 0101"},
		MobilePhone:    "+1 555 0102",
		CompanyName:    "Example Inc.",
		JobTitle:       "Product Lead",
		PersonalNotes:  "Important contact",
	})

	if contact.Name != "Jane Doe" || contact.Email != "jane@example.com" || contact.Phone != "+1 555 0102" || contact.Organization != "Example Inc." || contact.Title != "Product Lead" || contact.Notes != "Important contact" {
		t.Fatalf("outlookContactFromGraph() = %#v, want expanded fields", contact)
	}
	if contact.EmailLabel != "primary" || len(contact.AdditionalEmails) != 1 || contact.AdditionalEmails[0] != "jane@work.example" || contact.AdditionalEmailLabels[0] != "alternate" {
		t.Fatalf("AdditionalEmails = %#v labels=%#v, want alternate email", contact.AdditionalEmails, contact.AdditionalEmailLabels)
	}
	if contact.PhoneLabel != "mobile" || len(contact.AdditionalPhones) != 2 || contact.AdditionalPhoneLabels[0] != "work" || contact.AdditionalPhoneLabels[1] != "home" {
		t.Fatalf("phones = %q %#v labels=%#v, want mobile/work/home", contact.Phone, contact.AdditionalPhones, contact.AdditionalPhoneLabels)
	}
}

func TestOutlookContactPayloadFromContactMapsExpandedFields(t *testing.T) {
	payload := outlookContactPayloadFromContact(models.Contact{
		Name:                  "Jane Doe",
		Email:                 "jane@example.com",
		AdditionalEmails:      []string{"jane@work.example"},
		Phone:                 "+1 555 0100",
		PhoneLabel:            "mobile",
		AdditionalPhones:      []string{"+1 555 0101", "+1 555 0102"},
		AdditionalPhoneLabels: []string{"home", "work"},
		Organization:          "Example Inc.",
		Title:                 "Product Lead",
		Notes:                 "Important contact",
	})

	if payload.DisplayName != "Jane Doe" || len(payload.EmailAddresses) != 2 || payload.EmailAddresses[1].Address != "jane@work.example" {
		t.Fatalf("payload emails = %#v, want primary and additional", payload.EmailAddresses)
	}
	if payload.MobilePhone != "+1 555 0100" || len(payload.HomePhones) != 1 || payload.HomePhones[0] != "+1 555 0101" || len(payload.BusinessPhones) != 1 || payload.BusinessPhones[0] != "+1 555 0102" {
		t.Fatalf("payload phones = mobile:%q home:%#v business:%#v", payload.MobilePhone, payload.HomePhones, payload.BusinessPhones)
	}
	if payload.CompanyName != "Example Inc." || payload.JobTitle != "Product Lead" || payload.PersonalNotes != "Important contact" {
		t.Fatalf("payload org/title/notes = %#v, want mapped fields", payload)
	}
}

func TestSyncOutlookContactsImportsGraphContacts(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/contacts" {
			t.Fatalf("path = %q, want /me/contacts", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		if !strings.Contains(r.URL.RawQuery, "%24select=") {
			t.Fatalf("query = %q, want selected fields", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"value": [{
				"id": "remote-1",
				"changeKey": "ck-1",
				"displayName": "Jane Doe",
				"emailAddresses": [{"name": "Jane Doe", "address": "jane@example.com"}],
				"businessPhones": ["+1 555 0100"],
				"companyName": "Example Inc.",
				"jobTitle": "Product Lead",
				"personalNotes": "Important contact"
			}]
		}`))
	}))
	defer server.Close()

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	h := &Handler{db: db}
	imported, err := h.syncOutlookContacts(ctx, "default", "outlook_acc", "token")
	if err != nil {
		t.Fatalf("syncOutlookContacts() error = %v", err)
	}
	if imported != 1 {
		t.Fatalf("imported = %d, want 1", imported)
	}

	contact, err := db.SearchContacts(ctx, "default", "jane@example.com", 5)
	if err != nil || len(contact) != 1 {
		t.Fatalf("SearchContacts() = %#v, %v; want one contact", contact, err)
	}
	if contact[0].Name != "Jane Doe" {
		t.Fatalf("contact name = %q, want Jane Doe", contact[0].Name)
	}
	source, err := db.GetContactSourceByRemoteID(ctx, "default", providers.ProviderOutlook, "outlook_acc", "remote-1")
	if err != nil {
		t.Fatalf("GetContactSourceByRemoteID() error = %v", err)
	}
	if source == nil || source.ContactID != contact[0].ID || source.Etag != "ck-1" {
		t.Fatalf("source = %#v, want Outlook source with change key", source)
	}
}
