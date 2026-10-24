package mail

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// JMAPContactFromCard reads the details Gofer keeps from a JSContact card,
// labelling emails and phones the way vCard contacts are. photo is the
// card's photo when the server gave it as a blob to download rather than a
// URI. ok is false for groups and for cards with nothing worth showing.
func JMAPContactFromCard(card jmap.ContactCard) (contact models.Contact, photo jmap.ContactMedia, ok bool) {
	if strings.EqualFold(card.Kind, "group") {
		return models.Contact{}, jmap.ContactMedia{}, false
	}
	emails := sortedByPref(card.Emails, func(e jmap.ContactEmail) int { return e.Pref })
	phones := sortedByPref(card.Phones, func(p jmap.ContactPhone) int { return p.Pref })

	for _, email := range emails {
		address := strings.TrimSpace(email.Address)
		if !strings.Contains(address, "@") {
			continue
		}
		if contact.Email == "" {
			contact.Email, contact.EmailLabel = address, jmapEmailLabel(email, "primary")
			continue
		}
		if !strings.EqualFold(address, contact.Email) && !slices.ContainsFunc(contact.AdditionalEmails, func(seen string) bool { return strings.EqualFold(seen, address) }) {
			contact.AdditionalEmails = append(contact.AdditionalEmails, address)
			contact.AdditionalEmailLabels = append(contact.AdditionalEmailLabels, jmapEmailLabel(email, "alternate"))
		}
	}
	for _, phone := range phones {
		number := strings.TrimSpace(phone.Number)
		if number == "" {
			continue
		}
		if contact.Phone == "" {
			contact.Phone, contact.PhoneLabel = number, jmapPhoneLabel(phone, "primary")
			continue
		}
		if number != contact.Phone && !slices.Contains(contact.AdditionalPhones, number) {
			contact.AdditionalPhones = append(contact.AdditionalPhones, number)
			contact.AdditionalPhoneLabels = append(contact.AdditionalPhoneLabels, jmapPhoneLabel(phone, "alternate"))
		}
	}

	contact.Name = jmapCardName(card.Name)
	if contact.Name == "" {
		contact.Name = contact.Email
	}
	for _, key := range sortedKeys(card.Organizations) {
		if name := strings.TrimSpace(card.Organizations[key].Name); name != "" {
			contact.Organization = name
			break
		}
	}
	for _, key := range sortedKeys(card.Titles) {
		title := card.Titles[key]
		if name := strings.TrimSpace(title.Name); name != "" && !strings.EqualFold(title.Kind, "role") {
			contact.Title = name
			break
		}
	}
	var notes []string
	for _, key := range sortedKeys(card.Notes) {
		if note := strings.TrimSpace(card.Notes[key].Note); note != "" {
			notes = append(notes, note)
		}
	}
	contact.Notes = strings.Join(notes, "\n\n")

	for _, media := range sortedByPref(card.Media, func(m jmap.ContactMedia) int { return m.Pref }) {
		if !strings.EqualFold(media.Kind, "photo") {
			continue
		}
		uri := strings.TrimSpace(media.URI)
		lower := strings.ToLower(uri)
		if strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "data:image/") {
			contact.AvatarURL = uri
			break
		}
		if media.BlobID != "" {
			photo = media
			break
		}
	}
	return contact, photo, contact.HasDetails()
}

// JMAPCardFromContact is a new JSContact card holding a Gofer contact's
// details, without the id, uid or address books the caller adds.
func JMAPCardFromContact(contact models.Contact) jmap.ContactCard {
	card := jmap.ContactCard{Kind: "individual", Name: jmapContactName(contact.Name)}
	card.Emails = jmapContactEmails(contact)
	card.Phones = jmapContactPhones(contact)
	if organization := strings.TrimSpace(contact.Organization); organization != "" {
		card.Organizations = map[string]jmap.ContactOrganization{"o1": {Name: organization}}
	}
	if title := strings.TrimSpace(contact.Title); title != "" {
		card.Titles = map[string]jmap.ContactTitle{"t1": {Name: title, Kind: "title"}}
	}
	if notes := strings.TrimSpace(contact.Notes); notes != "" {
		card.Notes = map[string]jmap.ContactNote{"n1": {Note: notes}}
	}
	return card
}

// JMAPContactPatch changes the remote card to match a Gofer contact. Only the
// properties whose details differ are replaced, so a card's addresses,
// birthdays and anything else Gofer doesn't keep are never touched, and an
// unchanged contact gives an empty patch.
func JMAPContactPatch(remote jmap.ContactCard, contact models.Contact) jmap.PatchObject {
	current, _, _ := JMAPContactFromCard(remote)
	if remote.Name == nil && current.Name == current.Email {
		// A card without a name reads as its address; that is no name to keep.
		current.Name = ""
	}
	want := JMAPCardFromContact(contact)
	patch := jmap.PatchObject{}
	set := func(property string, differs bool, value any, empty bool) {
		if !differs {
			return
		}
		if empty {
			patch[property] = nil
		} else {
			patch[property] = value
		}
	}
	name := strings.TrimSpace(contact.Name)
	set("name", name != strings.TrimSpace(current.Name) && !(name == "" && current.Name == ""), want.Name, want.Name == nil)
	set("emails", !slices.Equal(jmapEmailEntries(current), jmapEmailEntries(contact)), want.Emails, len(want.Emails) == 0)
	set("phones", !slices.Equal(jmapPhoneEntries(current), jmapPhoneEntries(contact)), want.Phones, len(want.Phones) == 0)
	set("organizations", strings.TrimSpace(contact.Organization) != current.Organization, want.Organizations, len(want.Organizations) == 0)
	set("titles", strings.TrimSpace(contact.Title) != current.Title, want.Titles, len(want.Titles) == 0)
	set("notes", strings.TrimSpace(contact.Notes) != current.Notes, want.Notes, len(want.Notes) == 0)
	return patch
}

// jmapCardName is the name a card shows: its full name, or else its name
// components joined in order.
func jmapCardName(name *jmap.ContactName) string {
	if name == nil {
		return ""
	}
	if full := strings.Join(strings.Fields(name.Full), " "); full != "" {
		return full
	}
	var parts []string
	for _, component := range name.Components {
		if value := strings.TrimSpace(component.Value); value != "" && component.Kind != "separator" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " ")
}

// jmapContactName splits a name into given and surname components the way a
// vCard N field is filled in, and keeps the whole name as the full name.
func jmapContactName(name string) *jmap.ContactName {
	parts := strings.Fields(name)
	if len(parts) == 0 {
		return nil
	}
	out := &jmap.ContactName{Full: strings.Join(parts, " ")}
	if len(parts) == 1 {
		out.Components = []jmap.ContactNameComponent{{Kind: "given", Value: parts[0]}}
		return out
	}
	out.Components = []jmap.ContactNameComponent{
		{Kind: "given", Value: strings.Join(parts[:len(parts)-1], " ")},
		{Kind: "surname", Value: parts[len(parts)-1]},
	}
	return out
}

// jmapEmailLabel names an email the way the contact editor does: its own
// label, else "work" or "home" from its contexts, else fallback.
func jmapEmailLabel(email jmap.ContactEmail, fallback string) string {
	if label := strings.ToLower(strings.TrimSpace(email.Label)); label != "" {
		return label
	}
	return jmapContextLabel(email.Contexts, fallback)
}

// jmapPhoneLabel is like jmapEmailLabel, with "mobile" and "fax" lines named
// after their feature first.
func jmapPhoneLabel(phone jmap.ContactPhone, fallback string) string {
	switch {
	case phone.Features["mobile"]:
		return "mobile"
	case phone.Features["fax"]:
		return "fax"
	}
	if label := strings.ToLower(strings.TrimSpace(phone.Label)); label != "" {
		return label
	}
	return jmapContextLabel(phone.Contexts, fallback)
}

func jmapContextLabel(contexts map[string]bool, fallback string) string {
	switch {
	case contexts["work"]:
		return "work"
	case contexts["private"]:
		return "home"
	}
	return fallback
}

func jmapContactEmails(contact models.Contact) map[string]jmap.ContactEmail {
	entries := jmapEmailEntries(contact)
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]jmap.ContactEmail, len(entries))
	for i, entry := range entries {
		email := jmap.ContactEmail{Address: entry.value, Pref: i + 1}
		switch entry.label {
		case "work":
			email.Contexts = map[string]bool{"work": true}
		case "home":
			email.Contexts = map[string]bool{"private": true}
		case "":
		default:
			email.Label = entry.label
		}
		out[fmt.Sprintf("e%d", i+1)] = email
	}
	return out
}

func jmapContactPhones(contact models.Contact) map[string]jmap.ContactPhone {
	entries := jmapPhoneEntries(contact)
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]jmap.ContactPhone, len(entries))
	for i, entry := range entries {
		phone := jmap.ContactPhone{Number: entry.value, Pref: i + 1}
		switch entry.label {
		case "mobile", "fax":
			phone.Features = map[string]bool{entry.label: true}
		case "work":
			phone.Contexts = map[string]bool{"work": true}
		case "home":
			phone.Contexts = map[string]bool{"private": true}
		case "":
		default:
			phone.Label = entry.label
		}
		out[fmt.Sprintf("p%d", i+1)] = phone
	}
	return out
}

// jmapContactEntry is one email or phone with its label, the primary first.
// Labels that only say which entry is primary, and "other", are "", since a
// card has nothing that keeps them.
type jmapContactEntry struct{ value, label string }

func jmapEmailEntries(contact models.Contact) []jmapContactEntry {
	return jmapEntries(contact.Email, contact.EmailLabel, contact.AdditionalEmails, contact.AdditionalEmailLabels, strings.EqualFold)
}

func jmapPhoneEntries(contact models.Contact) []jmapContactEntry {
	return jmapEntries(contact.Phone, contact.PhoneLabel, contact.AdditionalPhones, contact.AdditionalPhoneLabels, func(a, b string) bool { return a == b })
}

func jmapEntries(primary, primaryLabel string, additional, labels []string, same func(a, b string) bool) []jmapContactEntry {
	var out []jmapContactEntry
	add := func(value, label string) {
		value = strings.TrimSpace(value)
		if value == "" || slices.ContainsFunc(out, func(e jmapContactEntry) bool { return same(e.value, value) }) {
			return
		}
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "primary" || label == "alternate" || label == "other" {
			label = ""
		}
		out = append(out, jmapContactEntry{value, label})
	}
	add(primary, primaryLabel)
	for i, value := range additional {
		add(value, contactLabelAt(labels, i))
	}
	return out
}

func contactLabelAt(labels []string, i int) string {
	if i < len(labels) {
		return labels[i]
	}
	return ""
}

// sortedByPref orders a JSContact map by preference, most preferred first,
// with unranked entries last and ties broken by key so the order is stable.
func sortedByPref[T any](entries map[string]T, pref func(T) int) []T {
	keys := sortedKeys(entries)
	rank := func(key string) int {
		if p := pref(entries[key]); p > 0 {
			return p
		}
		return int(^uint(0) >> 1)
	}
	slices.SortStableFunc(keys, func(a, b string) int { return cmp.Compare(rank(a), rank(b)) })
	out := make([]T, len(keys))
	for i, key := range keys {
		out[i] = entries[key]
	}
	return out
}

func sortedKeys[T any](entries map[string]T) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
