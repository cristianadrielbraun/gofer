package handler

import (
	"bytes"
	"encoding/base64"
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
		card.Add(emersionvcard.FieldEmail, &emersionvcard.Field{
			Value:  email,
			Params: contactVCardTypeParams("INTERNET", contact.EmailLabel),
		})
	}
	for i, additionalEmail := range contact.AdditionalEmails {
		additionalEmail = strings.TrimSpace(additionalEmail)
		if additionalEmail == "" || strings.EqualFold(additionalEmail, email) {
			continue
		}
		card.Add(emersionvcard.FieldEmail, &emersionvcard.Field{
			Value:  additionalEmail,
			Params: contactVCardTypeParams("INTERNET", contactAdditionalLabel(contact.AdditionalEmailLabels, i)),
		})
	}
	if phone := strings.TrimSpace(contact.Phone); phone != "" {
		card.Add(emersionvcard.FieldTelephone, &emersionvcard.Field{Value: phone, Params: contactVCardTypeParams(contact.PhoneLabel)})
	}
	for i, additionalPhone := range contact.AdditionalPhones {
		additionalPhone = strings.TrimSpace(additionalPhone)
		if additionalPhone == "" || strings.EqualFold(additionalPhone, strings.TrimSpace(contact.Phone)) {
			continue
		}
		card.Add(emersionvcard.FieldTelephone, &emersionvcard.Field{Value: additionalPhone, Params: contactVCardTypeParams(contactAdditionalLabel(contact.AdditionalPhoneLabels, i))})
	}
	if organization := strings.TrimSpace(contact.Organization); organization != "" {
		card.SetValue(emersionvcard.FieldOrganization, organization)
	}
	if title := strings.TrimSpace(contact.Title); title != "" {
		card.SetValue(emersionvcard.FieldTitle, title)
	}
	if notes := strings.TrimSpace(contact.Notes); notes != "" {
		card.SetValue(emersionvcard.FieldNote, notes)
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

func contactVCardTypeParams(types ...string) emersionvcard.Params {
	params := emersionvcard.Params{}
	for _, typ := range types {
		typ = strings.TrimSpace(typ)
		normalized := strings.ToLower(typ)
		if normalized == "" || normalized == "primary" || normalized == "alternate" {
			continue
		}
		params.Add(emersionvcard.ParamType, typ)
	}
	return params
}

func contactAdditionalLabel(labels []string, index int) string {
	if index >= 0 && index < len(labels) {
		return labels[index]
	}
	return ""
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
	emails, emailLabels := cleanVCardFields(card[emersionvcard.FieldEmail], email)
	phones, phoneLabels := cleanVCardFields(card[emersionvcard.FieldTelephone], card.PreferredValue(emersionvcard.FieldTelephone))
	name := strings.TrimSpace(card.PreferredValue(emersionvcard.FieldFormattedName))
	if name == "" {
		name = formattedVCardName(card.Name())
	}
	if name == "" {
		name = email
	}
	return models.Contact{
		Name:                  cleanVCardText(name),
		Email:                 cleanVCardText(email),
		EmailLabel:            contactVCardFieldLabel(card.Preferred(emersionvcard.FieldEmail), "primary"),
		AdditionalEmails:      emails,
		AdditionalEmailLabels: emailLabels,
		Phone:                 cleanVCardText(card.PreferredValue(emersionvcard.FieldTelephone)),
		PhoneLabel:            contactVCardFieldLabel(card.Preferred(emersionvcard.FieldTelephone), "primary"),
		AdditionalPhones:      phones,
		AdditionalPhoneLabels: phoneLabels,
		Organization:          cleanVCardText(card.PreferredValue(emersionvcard.FieldOrganization)),
		Title:                 cleanVCardText(card.PreferredValue(emersionvcard.FieldTitle)),
		Notes:                 cleanVCardText(card.PreferredValue(emersionvcard.FieldNote)),
		AvatarURL:             contactVCardPhotoURL(card),
		SaveTargets:           saveTargets,
	}, true
}

func contactVCardPhotoURL(card emersionvcard.Card) string {
	for _, field := range card[emersionvcard.FieldPhoto] {
		if field == nil {
			continue
		}
		value := strings.TrimSpace(field.Value)
		if value == "" {
			continue
		}
		lower := strings.ToLower(value)
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "data:image/") {
			return value
		}
		mediaType := strings.TrimSpace(field.Params.Get(emersionvcard.ParamMediaType))
		if mediaType == "" {
			mediaType = vCardPhotoMediaType(field.Params.Get(emersionvcard.ParamType))
		}
		if mediaType == "" || !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
			continue
		}
		encoding := strings.ToLower(strings.TrimSpace(field.Params.Get("ENCODING")))
		if encoding != "" && encoding != "b" && encoding != "base64" {
			continue
		}
		if _, err := base64.StdEncoding.DecodeString(value); err != nil {
			continue
		}
		return "data:" + mediaType + ";base64," + value
	}
	return ""
}

func vCardPhotoMediaType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "jpeg", "jpg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	default:
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "image/") {
			return strings.ToLower(strings.TrimSpace(value))
		}
		return ""
	}
}

func cleanVCardFields(fields []*emersionvcard.Field, primary string) ([]string, []string) {
	primary = cleanVCardText(primary)
	out := make([]string, 0, len(fields))
	labels := make([]string, 0, len(fields))
	seen := map[string]bool{}
	if primary != "" {
		seen[strings.ToLower(primary)] = true
	}
	for _, field := range fields {
		if field == nil {
			continue
		}
		value := cleanVCardText(field.Value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
		labels = append(labels, contactVCardFieldLabel(field, "alternate"))
	}
	return out, labels
}

func contactVCardFieldLabel(field *emersionvcard.Field, fallback string) string {
	if field == nil {
		return fallback
	}
	for _, typ := range field.Params.Types() {
		typ = strings.ToLower(strings.TrimSpace(typ))
		if typ != "" && typ != "internet" && typ != "pref" {
			return typ
		}
	}
	return fallback
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
