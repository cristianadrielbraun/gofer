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
		"EMAIL;TYPE=INTERNET:jane@example.com",
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
	if contacts[0].Name != "Jane Doe" || contacts[0].Email != "jane@example.com" {
		t.Fatalf("parseVCardContacts() contact = %#v, want Jane Doe", contacts[0])
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
