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
