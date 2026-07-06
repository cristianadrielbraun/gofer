package handler

import (
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestRenderVCard4(t *testing.T) {
	dataBytes, err := renderVCard4([]models.Contact{{
		ID:    "6d2b5072-a9b3-4cad-bfa5-2e7912d80b8c",
		Name:  "Jane Doe",
		Email: "jane@example.com",
	}})
	if err != nil {
		t.Fatalf("renderVCard4() error = %v", err)
	}
	data := string(dataBytes)

	want := strings.Join([]string{
		"BEGIN:VCARD",
		"VERSION:4.0",
		"EMAIL;TYPE=INTERNET:jane@example.com",
		"FN:Jane Doe",
		"N:Doe;Jane;;;",
		"PRODID:-//Gofer//Contacts//EN",
		"UID:urn:uuid:6d2b5072-a9b3-4cad-bfa5-2e7912d80b8c",
		"END:VCARD",
		"",
	}, "\r\n")
	if data != want {
		t.Fatalf("renderVCard4() = %q, want %q", data, want)
	}
}

func TestRenderVCard4EscapesText(t *testing.T) {
	dataBytes, err := renderVCard4([]models.Contact{{
		Name:  "Doe; Jane, Jr.\\Ops",
		Email: "jane@example.com",
	}})
	if err != nil {
		t.Fatalf("renderVCard4() error = %v", err)
	}
	data := string(dataBytes)

	if !strings.Contains(data, `FN:Doe; Jane\, Jr.\\Ops`) {
		t.Fatalf("renderVCard4() did not escape FN: %q", data)
	}
}

func TestRenderVCard4AdditionalFields(t *testing.T) {
	dataBytes, err := renderVCard4([]models.Contact{{
		Name:                  "Jane Doe",
		Email:                 "jane@example.com",
		EmailLabel:            "work",
		AdditionalEmails:      []string{"jane@work.example"},
		AdditionalEmailLabels: []string{"assistant"},
		Phone:                 "+1 555 0100",
		PhoneLabel:            "mobile",
		AdditionalPhones:      []string{"+1 555 0101"},
		AdditionalPhoneLabels: []string{"home"},
		Organization:          "Example Inc.",
		Title:                 "Product Lead",
		Notes:                 "Important contact",
	}})
	if err != nil {
		t.Fatalf("renderVCard4() error = %v", err)
	}
	data := string(dataBytes)
	for _, want := range []string{"EMAIL;TYPE=INTERNET;TYPE=work:jane@example.com", "EMAIL;TYPE=INTERNET;TYPE=assistant:jane@work.example", "TEL;TYPE=mobile:+1 555 0100", "TEL;TYPE=home:+1 555 0101", "ORG:Example Inc.", "TITLE:Product Lead", "NOTE:Important contact"} {
		if !strings.Contains(data, want) {
			t.Fatalf("renderVCard4() missing %q in %q", want, data)
		}
	}
}

func TestWriteFoldedVCardLineFoldsLongLines(t *testing.T) {
	var b strings.Builder
	writeFoldedVCardLine(&b, "FN:"+strings.Repeat("a", 90))
	got := b.String()
	if !strings.Contains(got, "\r\n ") {
		t.Fatalf("writeFoldedVCardLine() = %q, want folded line", got)
	}
}

func TestParseVCardContacts(t *testing.T) {
	input := strings.Join([]string{
		"BEGIN:VCARD",
		"VERSION:4.0",
		"FN:Jane Doe",
		"N:Doe;Jane;;;",
		"EMAIL;TYPE=INTERNET;TYPE=work:jane@example.com",
		"EMAIL;TYPE=INTERNET;TYPE=assistant:jane@work.example",
		"TEL;TYPE=mobile:+1 555 0100",
		"TEL;TYPE=home:+1 555 0101",
		"ORG:Example Inc.",
		"TITLE:Product Lead",
		"NOTE:Important contact",
		"PHOTO;MEDIATYPE=image/png:iVBORw0KGgo=",
		"END:VCARD",
		"BEGIN:VCARD",
		"VERSION:4.0",
		"FN:No Email",
		"END:VCARD",
		"",
	}, "\r\n")

	contacts, err := parseVCardContacts(strings.NewReader(input), []string{"local", "account:acc"})
	if err != nil {
		t.Fatalf("parseVCardContacts() error = %v", err)
	}
	if len(contacts) != 1 {
		t.Fatalf("parseVCardContacts() len = %d, want 1", len(contacts))
	}
	if contacts[0].Name != "Jane Doe" || contacts[0].Email != "jane@example.com" || contacts[0].Phone != "+1 555 0100" || contacts[0].Organization != "Example Inc." || contacts[0].Title != "Product Lead" || contacts[0].Notes != "Important contact" {
		t.Fatalf("parseVCardContacts() contact = %#v, want extra fields", contacts[0])
	}
	if contacts[0].AvatarURL != "data:image/png;base64,iVBORw0KGgo=" {
		t.Fatalf("AvatarURL = %q, want vCard PHOTO data URL", contacts[0].AvatarURL)
	}
	if len(contacts[0].AdditionalEmails) != 1 || contacts[0].AdditionalEmails[0] != "jane@work.example" {
		t.Fatalf("AdditionalEmails = %#v, want work email", contacts[0].AdditionalEmails)
	}
	if contacts[0].EmailLabel != "work" || len(contacts[0].AdditionalEmailLabels) != 1 || contacts[0].AdditionalEmailLabels[0] != "assistant" {
		t.Fatalf("email labels = %q %#v, want work/assistant", contacts[0].EmailLabel, contacts[0].AdditionalEmailLabels)
	}
	if len(contacts[0].AdditionalPhones) != 1 || contacts[0].AdditionalPhones[0] != "+1 555 0101" {
		t.Fatalf("AdditionalPhones = %#v, want second phone", contacts[0].AdditionalPhones)
	}
	if contacts[0].PhoneLabel != "mobile" || len(contacts[0].AdditionalPhoneLabels) != 1 || contacts[0].AdditionalPhoneLabels[0] != "home" {
		t.Fatalf("phone labels = %q %#v, want mobile/home", contacts[0].PhoneLabel, contacts[0].AdditionalPhoneLabels)
	}
	if got := strings.Join(contacts[0].SaveTargets, ","); got != "local,account:acc" {
		t.Fatalf("SaveTargets = %q, want local,account:acc", got)
	}
}

func TestParseVCardContactsFallsBackToStructuredName(t *testing.T) {
	input := strings.Join([]string{
		"BEGIN:VCARD",
		"VERSION:4.0",
		"N:Doe;Jane;;;",
		"EMAIL:jane@example.com",
		"END:VCARD",
		"",
	}, "\r\n")

	contacts, err := parseVCardContacts(strings.NewReader(input), []string{"local"})
	if err != nil {
		t.Fatalf("parseVCardContacts() error = %v", err)
	}
	if contacts[0].Name != "Jane Doe" {
		t.Fatalf("Name = %q, want Jane Doe", contacts[0].Name)
	}
}
