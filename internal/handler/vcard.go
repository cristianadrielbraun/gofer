package handler

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	emersionvcard "github.com/emersion/go-vcard"
)

func renderVCard4(contacts []models.Contact) ([]byte, error) {
	var b bytes.Buffer
	enc := emersionvcard.NewEncoder(&b)
	for _, contact := range contacts {
		card := contactVCard(contact)
		if err := enc.Encode(card); err != nil {
			return nil, err
		}
	}
	return foldVCardOutput(b.String()), nil
}

func contactVCard(contact models.Contact) emersionvcard.Card {
	name := strings.TrimSpace(contact.Name)
	email := strings.TrimSpace(contact.Email)
	if name == "" {
		name = email
	}

	card := emersionvcard.Card{}
	card.SetValue(emersionvcard.FieldVersion, "4.0")
	card.SetValue(emersionvcard.FieldProductID, "-//Gofer//Contacts//EN")
	if contact.ID != "" {
		card.SetValue(emersionvcard.FieldUID, "urn:uuid:"+contact.ID)
	}
	card.SetValue(emersionvcard.FieldFormattedName, name)
	card.SetName(vCardName(name))
	if email != "" {
		card.Set(emersionvcard.FieldEmail, &emersionvcard.Field{
			Value:  email,
			Params: emersionvcard.Params{emersionvcard.ParamType: {"INTERNET"}},
		})
	}
	return card
}

func vCardName(name string) *emersionvcard.Name {
	parts := strings.Fields(strings.TrimSpace(name))
	if len(parts) == 0 {
		return &emersionvcard.Name{}
	}
	if len(parts) == 1 {
		return &emersionvcard.Name{GivenName: parts[0]}
	}
	return &emersionvcard.Name{
		FamilyName: parts[len(parts)-1],
		GivenName:  strings.Join(parts[:len(parts)-1], " "),
	}
}

func foldVCardOutput(raw string) []byte {
	var b strings.Builder
	for raw != "" {
		line := raw
		if idx := strings.Index(raw, "\r\n"); idx >= 0 {
			line = raw[:idx]
			raw = raw[idx+2:]
		} else {
			raw = ""
		}
		writeFoldedVCardLine(&b, line)
	}
	return []byte(b.String())
}

func writeFoldedVCardLine(b *strings.Builder, line string) {
	column := 0
	for _, r := range line {
		width := len(string(r))
		if column > 0 && column+width > 75 {
			b.WriteString("\r\n ")
			column = 1
		}
		b.WriteRune(r)
		column += width
	}
	b.WriteString("\r\n")
}

func parseVCardContacts(r io.Reader, saveTargets []string) ([]models.Contact, error) {
	dec := emersionvcard.NewDecoder(r)
	var contacts []models.Contact
	for {
		card, err := dec.Decode()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid vCard: %w", err)
		}
		contact, ok := contactFromVCard(card, saveTargets)
		if ok {
			contacts = append(contacts, contact)
		}
	}
	if len(contacts) == 0 {
		return nil, fmt.Errorf("no contacts with email addresses found")
	}
	return contacts, nil
}

func contactFromVCard(card emersionvcard.Card, saveTargets []string) (models.Contact, bool) {
	email := strings.TrimSpace(card.PreferredValue(emersionvcard.FieldEmail))
	if email == "" {
		email = strings.TrimSpace(card.Value(emersionvcard.FieldEmail))
	}
	if email == "" {
		return models.Contact{}, false
	}
	name := strings.TrimSpace(card.PreferredValue(emersionvcard.FieldFormattedName))
	if name == "" {
		name = formattedVCardName(card.Name())
	}
	if name == "" {
		name = email
	}
	return models.Contact{Name: cleanVCardText(name), Email: cleanVCardText(email), SaveTargets: saveTargets}, true
}

func formattedVCardName(name *emersionvcard.Name) string {
	if name == nil {
		return ""
	}
	parts := []string{name.HonorificPrefix, name.GivenName, name.AdditionalName, name.FamilyName, name.HonorificSuffix}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(cleanVCardText(part))
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, " ")
}

func cleanVCardText(value string) string {
	value = strings.ReplaceAll(value, `\;`, ";")
	value = strings.ReplaceAll(value, `\N`, "\n")
	return strings.TrimSpace(value)
}

func contactVCardFilename(contact models.Contact) string {
	base := strings.TrimSpace(contact.Name)
	if base == "" {
		email := strings.TrimSpace(contact.Email)
		base = email
	}
	if base == "" {
		base = "contact"
	}
	return safeDownloadFilename(base) + ".vcf"
}

func safeDownloadFilename(base string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(base) {
		if b.Len() >= 80 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	filename := strings.Trim(b.String(), "-")
	if filename == "" {
		filename = "contact"
	}
	return filename
}
