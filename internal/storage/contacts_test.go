package storage

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func newContactsTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return db
}

func TestObservedContactManualNameWins(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if err := db.UpsertObservedContact(ctx, "default", "Jane Observed", "jane@example.com", time.Now()); err != nil {
		t.Fatalf("UpsertObservedContact() error = %v", err)
	}
	contacts, err := db.SearchContacts(ctx, "default", "jane", 10)
	if err != nil {
		t.Fatalf("SearchContacts() error = %v", err)
	}
	if len(contacts) != 1 || contacts[0].Name != "Jane Observed" {
		t.Fatalf("observed contact = %#v, want Jane Observed", contacts)
	}
	if contacts[0].IsManual {
		t.Fatalf("observed contact IsManual = true, want false")
	}
	var legacyCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM contacts WHERE id = ?`, contacts[0].ID).Scan(&legacyCount); err != nil {
		t.Fatalf("legacy contact count query error = %v", err)
	}
	if legacyCount != 0 {
		t.Fatalf("legacy contacts row count = %d, want observed contact stored as profile", legacyCount)
	}

	manual, err := db.SaveContact(ctx, "default", models.Contact{ID: contacts[0].ID, Name: "Janet Manual", Email: "jane@example.com"})
	if err != nil {
		t.Fatalf("SaveContact() error = %v", err)
	}
	if !manual.IsManual {
		t.Fatalf("SaveContact() IsManual = false, want true")
	}

	if err := db.UpsertObservedContact(ctx, "default", "Jane Changed", "jane@example.com", time.Now()); err != nil {
		t.Fatalf("second UpsertObservedContact() error = %v", err)
	}
	got, err := db.GetContact(ctx, "default", manual.ID)
	if err != nil {
		t.Fatalf("GetContact() error = %v", err)
	}
	if got == nil || got.Name != "Janet Manual" {
		t.Fatalf("manual name after observed update = %#v, want Janet Manual", got)
	}
}

func TestDeleteContactSuppressesObservedRecreate(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if err := db.UpsertObservedContact(ctx, "default", "Noisy Sender", "noise@example.com", time.Now()); err != nil {
		t.Fatalf("UpsertObservedContact() error = %v", err)
	}
	contacts, err := db.SearchContacts(ctx, "default", "noise", 10)
	if err != nil || len(contacts) != 1 {
		t.Fatalf("SearchContacts() = %#v, %v; want one contact", contacts, err)
	}
	if err := db.DeleteContact(ctx, "default", contacts[0].ID, true); err != nil {
		t.Fatalf("DeleteContact() error = %v", err)
	}
	if err := db.UpsertObservedContact(ctx, "default", "Noisy Sender", "noise@example.com", time.Now()); err != nil {
		t.Fatalf("second UpsertObservedContact() error = %v", err)
	}
	contacts, err = db.SearchContacts(ctx, "default", "noise", 10)
	if err != nil {
		t.Fatalf("SearchContacts() error = %v", err)
	}
	if len(contacts) != 0 {
		t.Fatalf("contacts after suppressed recreate = %#v, want none", contacts)
	}
}

func TestAutoCreateObservedCanBeDisabled(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	settings := db.GetUISettings(ctx, "default")
	settings["contacts_auto_create_observed"] = "false"
	if err := db.SetUISettings(ctx, "default", settings); err != nil {
		t.Fatalf("SetUISettings() error = %v", err)
	}

	if err := db.UpsertObservedContact(ctx, "default", "Quiet Sender", "quiet@example.com", time.Now()); err != nil {
		t.Fatalf("UpsertObservedContact() error = %v", err)
	}
	contacts, err := db.SearchContacts(ctx, "default", "quiet", 10)
	if err != nil {
		t.Fatalf("SearchContacts() error = %v", err)
	}
	if len(contacts) != 0 {
		t.Fatalf("contacts with auto-create disabled = %#v, want none", contacts)
	}
}

func TestObservedSourcesControlMessageDiscovery(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().Exec(`INSERT INTO accounts (id, user_id, email_address, display_name) VALUES ('acc', 'default', 'me@example.com', 'Me')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	settings := db.GetUISettings(ctx, "default")
	settings["contacts_observed_sources"] = "senders"
	if err := db.SetUISettings(ctx, "default", settings); err != nil {
		t.Fatalf("SetUISettings() error = %v", err)
	}

	db.UpsertObservedContactsForMessage(ctx, "acc", "Sender", "sender@example.com", []Recipient{{Name: "Recipient", Email: "recipient@example.com"}}, nil, nil, time.Now())

	contacts, err := db.SearchContacts(ctx, "default", "sender", 10)
	if err != nil || len(contacts) != 1 {
		t.Fatalf("sender contacts = %#v, %v; want one", contacts, err)
	}
	contacts, err = db.SearchContacts(ctx, "default", "recipient", 10)
	if err != nil {
		t.Fatalf("recipient SearchContacts() error = %v", err)
	}
	if len(contacts) != 0 {
		t.Fatalf("recipient contacts = %#v, want none when recipients disabled", contacts)
	}
}

func TestDeleteObservedContactsKeepsManualContacts(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if err := db.UpsertObservedContact(ctx, "default", "Observed", "observed@example.com", time.Now()); err != nil {
		t.Fatalf("UpsertObservedContact() error = %v", err)
	}
	if _, err := db.SaveContact(ctx, "default", models.Contact{Name: "Manual", Email: "manual@example.com"}); err != nil {
		t.Fatalf("SaveContact() error = %v", err)
	}

	deleted, err := db.DeleteObservedContacts(ctx, "default", false)
	if err != nil {
		t.Fatalf("DeleteObservedContacts() error = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("DeleteObservedContacts() deleted = %d, want 1", deleted)
	}

	contacts, err := db.SearchContacts(ctx, "default", "manual", 10)
	if err != nil || len(contacts) != 1 {
		t.Fatalf("manual contacts = %#v, %v; want one", contacts, err)
	}
	contacts, err = db.SearchContacts(ctx, "default", "observed", 10)
	if err != nil {
		t.Fatalf("observed SearchContacts() error = %v", err)
	}
	if len(contacts) != 0 {
		t.Fatalf("observed contacts = %#v, want none", contacts)
	}
}

func TestSaveContactPersistsSaveTargets(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	saved, err := db.SaveContact(ctx, "default", models.Contact{Name: "Jane", Email: "jane@example.com"})
	if err != nil {
		t.Fatalf("SaveContact() error = %v", err)
	}
	if !reflect.DeepEqual(saved.SaveTargets, []string{"local"}) {
		t.Fatalf("default SaveTargets = %#v, want local", saved.SaveTargets)
	}
	var legacyCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM contacts WHERE id = ?`, saved.ID).Scan(&legacyCount); err != nil {
		t.Fatalf("legacy contact count query error = %v", err)
	}
	if legacyCount != 0 {
		t.Fatalf("legacy contacts row count = %d, want manual contact stored only as profile", legacyCount)
	}
	profile, err := db.GetContactProfile(ctx, "default", saved.ID)
	if err != nil {
		t.Fatalf("GetContactProfile() error = %v", err)
	}
	if profile == nil || profile.PrimaryEmail != "jane@example.com" {
		t.Fatalf("profile = %#v, want saved profile", profile)
	}

	saved, err = db.SaveContact(ctx, "default", models.Contact{
		ID:          saved.ID,
		Name:        "Jane",
		Email:       "jane@example.com",
		SaveTargets: []string{"account:acc", "local", "account:acc"},
	})
	if err != nil {
		t.Fatalf("SaveContact() update error = %v", err)
	}
	want := []string{"local", "account:acc"}
	if !reflect.DeepEqual(saved.SaveTargets, want) {
		t.Fatalf("updated SaveTargets = %#v, want %#v", saved.SaveTargets, want)
	}
}

func TestSaveContactPersistsAdditionalManualFields(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	saved, err := db.SaveContact(ctx, "default", models.Contact{
		Name:                  "Jane",
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
		Notes:                 "Met at the conference",
	})
	if err != nil {
		t.Fatalf("SaveContact() error = %v", err)
	}
	if saved.Phone != "+1 555 0100" || saved.Organization != "Example Inc." || saved.Title != "Product Lead" || saved.Notes != "Met at the conference" {
		t.Fatalf("saved contact fields = %#v, want phone/org/title/notes", saved)
	}
	if len(saved.AdditionalEmails) != 1 || saved.AdditionalEmails[0] != "jane@work.example" {
		t.Fatalf("saved AdditionalEmails = %#v, want work email", saved.AdditionalEmails)
	}
	if saved.EmailLabel != "work" || len(saved.AdditionalEmailLabels) != 1 || saved.AdditionalEmailLabels[0] != "assistant" {
		t.Fatalf("saved email labels = %q %#v, want work/assistant", saved.EmailLabel, saved.AdditionalEmailLabels)
	}
	if len(saved.AdditionalPhones) != 1 || saved.AdditionalPhones[0] != "+1 555 0101" {
		t.Fatalf("saved AdditionalPhones = %#v, want second phone", saved.AdditionalPhones)
	}
	if saved.PhoneLabel != "mobile" || len(saved.AdditionalPhoneLabels) != 1 || saved.AdditionalPhoneLabels[0] != "home" {
		t.Fatalf("saved phone labels = %q %#v, want mobile/home", saved.PhoneLabel, saved.AdditionalPhoneLabels)
	}

	profile, err := db.GetContactProfile(ctx, "default", saved.ID)
	if err != nil {
		t.Fatalf("GetContactProfile() error = %v", err)
	}
	fields := map[string]string{}
	for _, field := range profile.Fields {
		if field.IsPrimary || fields[field.Kind] == "" {
			fields[field.Kind] = field.Value
		}
	}
	for kind, want := range map[string]string{
		"phone":        "+1 555 0100",
		"organization": "Example Inc.",
		"title":        "Product Lead",
		"notes":        "Met at the conference",
	} {
		if got := fields[kind]; got != want {
			t.Fatalf("field %s = %q, want %q", kind, got, want)
		}
	}
	foundAdditionalEmail := false
	foundAdditionalPhone := false
	for _, field := range profile.Fields {
		if field.Kind == "email" && field.Value == "jane@work.example" && field.Label == "assistant" {
			foundAdditionalEmail = true
		}
		if field.Kind == "phone" && field.Value == "+1 555 0101" && field.Label == "home" {
			foundAdditionalPhone = true
		}
	}
	if !foundAdditionalEmail || !foundAdditionalPhone {
		t.Fatalf("profile fields = %#v, want additional email and phone", profile.Fields)
	}
}

func TestSaveContactPreservesSyncedFields(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	profile, err := db.SaveContactProfile(ctx, "default", models.ContactProfile{
		DisplayName:  "Jane",
		PrimaryEmail: "jane@example.com",
		Fields: []models.ContactField{
			{Kind: "email", Value: "jane@example.com", IsPrimary: true, Source: "manual"},
			{ID: "synced-phone", Kind: "phone", Value: "+1 555 9999", Source: "synced:acc"},
		},
	})
	if err != nil {
		t.Fatalf("SaveContactProfile() error = %v", err)
	}
	if _, err := db.SaveContact(ctx, "default", models.Contact{ID: profile.ID, Name: "Jane Local", Email: "jane@example.com", Phone: "+1 555 0100"}); err != nil {
		t.Fatalf("SaveContact() error = %v", err)
	}
	updated, err := db.GetContactProfile(ctx, "default", profile.ID)
	if err != nil {
		t.Fatalf("GetContactProfile() error = %v", err)
	}
	foundSynced := false
	foundManual := false
	for _, field := range updated.Fields {
		if field.ID == "synced-phone" && field.Value == "+1 555 9999" {
			foundSynced = true
		}
		if field.Kind == "phone" && field.Source == "manual" && field.Value == "+1 555 0100" {
			foundManual = true
		}
	}
	if !foundSynced || !foundManual {
		t.Fatalf("fields after SaveContact = %#v, want synced and manual phone preserved", updated.Fields)
	}
}

func TestUpsertSyncedContactPersistsProfileSourceCard(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	contactID, created, err := db.UpsertSyncedContact(ctx, "default", "acc", "Synced Jane", "Jane@Example.com")
	if err != nil {
		t.Fatalf("UpsertSyncedContact() error = %v", err)
	}
	if contactID == "" || !created {
		t.Fatalf("UpsertSyncedContact() = %q, %v; want created profile", contactID, created)
	}
	var legacyCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM contacts WHERE id = ?`, contactID).Scan(&legacyCount); err != nil {
		t.Fatalf("legacy contact count query error = %v", err)
	}
	if legacyCount != 0 {
		t.Fatalf("legacy contacts row count = %d, want synced contact stored only as profile", legacyCount)
	}
	profile, err := db.GetContactProfile(ctx, "default", contactID)
	if err != nil {
		t.Fatalf("GetContactProfile() error = %v", err)
	}
	if profile == nil || profile.DisplayName != "Synced Jane" || profile.PrimaryEmail != "Jane@Example.com" {
		t.Fatalf("profile = %#v, want synced profile", profile)
	}
	contacts, err := db.ListContacts(ctx, "default", models.ContactFilters{Source: "synced"}, 10, 0)
	if err != nil || len(contacts) != 1 || contacts[0].ID != contactID || contacts[0].IsManual {
		t.Fatalf("synced filter = %#v, %v; want non-manual synced contact", contacts, err)
	}
	got, err := db.GetContact(ctx, "default", contactID)
	if err != nil || got == nil || got.Source != "synced" || got.IsManual {
		t.Fatalf("GetContact() = %#v, %v; want synced non-manual contact", got, err)
	}

	if err := db.UpsertContactSource(ctx, ContactSource{
		ContactID:     contactID,
		UserID:        "default",
		Provider:      "carddav",
		AccountID:     "acc",
		AddressBookID: "book",
		RemoteID:      "https://dav.example/jane.vcf",
		Etag:          `"abc"`,
	}); err != nil {
		t.Fatalf("UpsertContactSource() error = %v", err)
	}
	source, err := db.GetContactSourceByRemoteID(ctx, "default", "carddav", "acc", "https://dav.example/jane.vcf")
	if err != nil {
		t.Fatalf("GetContactSourceByRemoteID() error = %v", err)
	}
	if source == nil || source.ContactID != contactID || source.AddressBookID != "book" || source.Etag != `"abc"` {
		t.Fatalf("source = %#v, want provider card source", source)
	}
	var cardCount int
	if err := db.Read().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM contact_cards
		WHERE user_id = 'default' AND profile_id = ? AND kind = 'provider' AND provider = 'carddav'`, contactID).Scan(&cardCount); err != nil {
		t.Fatalf("provider card count query error = %v", err)
	}
	if cardCount != 1 {
		t.Fatalf("provider card count = %d, want 1", cardCount)
	}
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_sources WHERE contact_id = ?`, contactID).Scan(&legacyCount); err != nil {
		t.Fatalf("legacy source count query error = %v", err)
	}
	if legacyCount != 0 {
		t.Fatalf("legacy contact_sources row count = %d, want provider source stored only as card", legacyCount)
	}
}

func TestUpsertSyncedContactFromContactPersistsProviderFields(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	contactID, _, err := db.UpsertSyncedContactFromContact(ctx, "default", "acc", models.Contact{
		Name:             "Synced Jane",
		Email:            "jane@example.com",
		AdditionalEmails: []string{"jane@work.example"},
		Phone:            "+1 555 0100",
		AdditionalPhones: []string{"+1 555 0101"},
		Organization:     "Example Inc.",
		Title:            "Product Lead",
		Notes:            "Provider note",
	})
	if err != nil {
		t.Fatalf("UpsertSyncedContactFromContact() error = %v", err)
	}
	profile, err := db.GetContactProfile(ctx, "default", contactID)
	if err != nil {
		t.Fatalf("GetContactProfile() error = %v", err)
	}
	fields := map[string]models.ContactField{}
	for _, field := range profile.Fields {
		if field.Source == "synced:acc" && (field.IsPrimary || fields[field.Kind].Value == "") {
			fields[field.Kind] = field
		}
	}
	for kind, want := range map[string]string{
		"phone":        "+1 555 0100",
		"organization": "Example Inc.",
		"title":        "Product Lead",
		"notes":        "Provider note",
	} {
		if got := fields[kind].Value; got != want {
			t.Fatalf("synced %s = %q, want %q; fields=%#v", kind, got, want, profile.Fields)
		}
	}
	got, err := db.GetContact(ctx, "default", contactID)
	if err != nil {
		t.Fatalf("GetContact() error = %v", err)
	}
	if len(got.AdditionalEmails) != 1 || got.AdditionalEmails[0] != "jane@work.example" {
		t.Fatalf("synced AdditionalEmails = %#v, want work email", got.AdditionalEmails)
	}
	if len(got.AdditionalPhones) != 1 || got.AdditionalPhones[0] != "+1 555 0101" {
		t.Fatalf("synced AdditionalPhones = %#v, want second phone", got.AdditionalPhones)
	}

	if _, err := db.SaveContact(ctx, "default", models.Contact{ID: contactID, Name: "Manual Jane", Email: "jane@example.com", Phone: "+1 555 9999"}); err != nil {
		t.Fatalf("SaveContact() error = %v", err)
	}
	got, err = db.GetContact(ctx, "default", contactID)
	if err != nil {
		t.Fatalf("GetContact() error = %v", err)
	}
	if got.Phone != "+1 555 9999" {
		t.Fatalf("manual Phone = %q, want manual value", got.Phone)
	}
	profile, err = db.GetContactProfile(ctx, "default", contactID)
	if err != nil {
		t.Fatalf("GetContactProfile() after manual error = %v", err)
	}
	foundSynced := false
	for _, field := range profile.Fields {
		if field.Kind == "phone" && field.Source == "synced:acc" && field.Value == "+1 555 0100" {
			foundSynced = true
		}
	}
	if !foundSynced {
		t.Fatalf("synced phone was not preserved after manual edit: %#v", profile.Fields)
	}
}

func TestListContactsFilters(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	manual, err := db.SaveContact(ctx, "default", models.Contact{Name: "Manual", Email: "manual@example.com", SaveTargets: []string{"account:acc"}})
	if err != nil {
		t.Fatalf("SaveContact() error = %v", err)
	}
	if err := db.UpsertObservedContact(ctx, "default", "Observed", "observed@example.com", time.Now()); err != nil {
		t.Fatalf("UpsertObservedContact() error = %v", err)
	}

	contacts, err := db.ListContacts(ctx, "default", models.ContactFilters{Source: "manual"}, 10, 0)
	if err != nil || len(contacts) != 1 || contacts[0].ID != manual.ID {
		t.Fatalf("manual filter = %#v, %v; want manual contact", contacts, err)
	}
	contacts, err = db.ListContacts(ctx, "default", models.ContactFilters{Source: "observed"}, 10, 0)
	if err != nil || len(contacts) != 1 || contacts[0].Email != "observed@example.com" {
		t.Fatalf("observed filter = %#v, %v; want observed contact", contacts, err)
	}
	contacts, err = db.ListContacts(ctx, "default", models.ContactFilters{SaveTarget: "account:acc"}, 10, 0)
	if err != nil || len(contacts) != 1 || contacts[0].ID != manual.ID {
		t.Fatalf("save target filter = %#v, %v; want manual contact", contacts, err)
	}
	contacts, err = db.ListContacts(ctx, "default", models.ContactFilters{Activity: "seen"}, 10, 0)
	if err != nil || len(contacts) != 1 || contacts[0].Email != "observed@example.com" {
		t.Fatalf("seen filter = %#v, %v; want observed contact", contacts, err)
	}
	contacts, err = db.ListContacts(ctx, "default", models.ContactFilters{Activity: "none"}, 10, 0)
	if err != nil || len(contacts) != 1 || contacts[0].ID != manual.ID {
		t.Fatalf("no messages filter = %#v, %v; want manual contact", contacts, err)
	}
}

func TestSuppressedContactsCanBeCleared(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if err := db.UpsertObservedContact(ctx, "default", "Blocked", "blocked@example.com", time.Now()); err != nil {
		t.Fatalf("UpsertObservedContact() error = %v", err)
	}
	contacts, err := db.SearchContacts(ctx, "default", "blocked", 10)
	if err != nil || len(contacts) != 1 {
		t.Fatalf("SearchContacts() = %#v, %v; want one", contacts, err)
	}
	if err := db.DeleteContact(ctx, "default", contacts[0].ID, true); err != nil {
		t.Fatalf("DeleteContact() error = %v", err)
	}
	count, err := db.CountSuppressedContacts(ctx, "default")
	if err != nil || count != 1 {
		t.Fatalf("CountSuppressedContacts() = %d, %v; want 1", count, err)
	}
	cleared, err := db.ClearSuppressedContacts(ctx, "default")
	if err != nil {
		t.Fatalf("ClearSuppressedContacts() error = %v", err)
	}
	if cleared != 1 {
		t.Fatalf("ClearSuppressedContacts() = %d, want 1", cleared)
	}
	count, err = db.CountSuppressedContacts(ctx, "default")
	if err != nil || count != 0 {
		t.Fatalf("CountSuppressedContacts() after clear = %d, %v; want 0", count, err)
	}
}

func TestContactSyncOperationsLifecycle(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	contact, err := db.SaveContact(ctx, "default", models.Contact{Name: "Queued", Email: "queued@example.com"})
	if err != nil {
		t.Fatalf("SaveContact() error = %v", err)
	}
	previous := contact
	previous.SaveTargets = []string{"local"}
	contact.SaveTargets = []string{"local", "account:acc"}

	opID, err := db.EnqueueContactSyncOperation(ctx, "default", contact, &previous)
	if err != nil {
		t.Fatalf("EnqueueContactSyncOperation() error = %v", err)
	}
	if opID == "" {
		t.Fatalf("EnqueueContactSyncOperation() returned empty id")
	}

	ops, err := db.ClaimContactSyncOperations(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("ClaimContactSyncOperations() error = %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("ClaimContactSyncOperations() len = %d, want 1", len(ops))
	}
	if ops[0].Status != "running" || ops[0].AttemptCount != 1 {
		t.Fatalf("claimed op status=%q attempts=%d, want running/1", ops[0].Status, ops[0].AttemptCount)
	}
	if ops[0].Payload.Contact.ID != contact.ID || ops[0].Payload.Previous == nil || ops[0].Payload.Previous.ID != previous.ID {
		t.Fatalf("claimed payload = %#v, want contact and previous", ops[0].Payload)
	}

	ops, err = db.ClaimContactSyncOperations(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("second ClaimContactSyncOperations() error = %v", err)
	}
	if len(ops) != 0 {
		t.Fatalf("second ClaimContactSyncOperations() len = %d, want locked op skipped", len(ops))
	}

	if err := db.MarkContactSyncOperationError(ctx, opID, "temporary failure", false); err != nil {
		t.Fatalf("MarkContactSyncOperationError() error = %v", err)
	}
	latest, err := db.LatestContactSyncOperationForContact(ctx, "default", contact.ID)
	if err != nil {
		t.Fatalf("LatestContactSyncOperationForContact() error = %v", err)
	}
	if latest == nil || latest.Status != "error" || latest.LastError != "temporary failure" {
		t.Fatalf("latest after error = %#v, want error state", latest)
	}
	got, err := db.GetContact(ctx, "default", contact.ID)
	if err != nil {
		t.Fatalf("GetContact() error = %v", err)
	}
	if got == nil || got.SyncStatus != "error" || got.SyncError != "temporary failure" {
		t.Fatalf("hydrated contact sync state = %#v, want error", got)
	}

	if err := db.MarkContactSyncOperationSuccess(ctx, opID); err != nil {
		t.Fatalf("MarkContactSyncOperationSuccess() error = %v", err)
	}
	latest, err = db.LatestContactSyncOperationForContact(ctx, "default", contact.ID)
	if err != nil {
		t.Fatalf("LatestContactSyncOperationForContact() after success error = %v", err)
	}
	if latest == nil || latest.Status != "done" || latest.LastError != "" {
		t.Fatalf("latest after success = %#v, want done without error", latest)
	}
}
