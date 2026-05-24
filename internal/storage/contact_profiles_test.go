package storage

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestSaveContactProfileWithCardsAndFields(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	saved, err := db.SaveContactProfile(ctx, "default", models.ContactProfile{
		DisplayName:  "Jane Doe",
		PrimaryEmail: "Jane@Example.COM",
		Cards: []models.ContactCard{
			{Kind: "local"},
			{Kind: "provider", Provider: "carddav", AccountID: "acc", AddressBookID: "book", RemoteID: "https://dav.example/jane.vcf", Etag: `"abc"`},
		},
		Fields: []models.ContactField{
			{Kind: "email", Label: "work", Value: "Jane@Example.COM", IsPrimary: true, Source: "manual"},
			{Kind: "phone", Label: "mobile", Value: "+1 555 0100"},
			{Kind: "organization", Value: "Example Inc."},
		},
	})
	if err != nil {
		t.Fatalf("SaveContactProfile() error = %v", err)
	}
	if saved.ID == "" || saved.UserID != "default" {
		t.Fatalf("saved profile identity = %#v, want id/default", saved)
	}
	if saved.PrimaryEmail != "Jane@Example.COM" {
		t.Fatalf("PrimaryEmail = %q, want original casing", saved.PrimaryEmail)
	}
	if len(saved.Cards) != 2 {
		t.Fatalf("len(Cards) = %d, want 2", len(saved.Cards))
	}
	if len(saved.Fields) != 3 {
		t.Fatalf("len(Fields) = %d, want 3", len(saved.Fields))
	}

	found, err := db.FindContactProfileByIdentity(ctx, "default", "email", "jane@example.com")
	if err != nil {
		t.Fatalf("FindContactProfileByIdentity() error = %v", err)
	}
	if found == nil || found.ID != saved.ID {
		t.Fatalf("FindContactProfileByIdentity() = %#v, want profile %s", found, saved.ID)
	}
}

func TestContactProfileInsightsDetectConflictsAndProvenance(t *testing.T) {
	profile := models.ContactProfile{
		Cards: []models.ContactCard{
			{Kind: "provider", Provider: "gmail", AccountID: "gmail"},
			{Kind: "provider", Provider: "carddav", AccountID: "dav", AddressBookID: "personal"},
		},
		Fields: []models.ContactField{
			{Kind: "email", Value: "Jane@Example.com", NormalizedValue: "jane@example.com", IsPrimary: true, Source: "manual"},
			{Kind: "email", Value: "jane.alt@example.com", NormalizedValue: "jane.alt@example.com", Source: "synced:gmail"},
			{Kind: "organization", Value: "Example Inc.", NormalizedValue: "example inc.", Source: "synced:gmail"},
		},
	}

	insights := ContactProfileInsights(profile)
	assertInsight := func(kind, field string) {
		t.Helper()
		for _, insight := range insights {
			if insight.Kind == kind && insight.Field == field {
				return
			}
		}
		t.Fatalf("missing insight kind=%q field=%q in %#v", kind, field, insights)
	}
	assertInsight("multi_source", "")
	assertInsight("field_conflict", "email")
	assertInsight("manual_override", "email")
	assertInsight("provider_only_field", "organization")
}

func TestContactProfileInsightsDetectObservedOnly(t *testing.T) {
	insights := ContactProfileInsights(models.ContactProfile{
		Cards: []models.ContactCard{{Kind: "observed"}},
		Fields: []models.ContactField{
			{Kind: "email", Value: "seen@example.com", NormalizedValue: "seen@example.com", Source: "observed"},
		},
	})
	if len(insights) == 0 {
		t.Fatalf("ContactProfileInsights() = nil, want observed insight")
	}
	found := false
	for _, insight := range insights {
		if insight.Kind == "observed_only" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing observed_only insight in %#v", insights)
	}
}

func TestPreferContactFieldUpdatesPrimaryWithoutDeletingFields(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	saved, err := db.SaveContactProfile(ctx, "default", models.ContactProfile{
		DisplayName:  "Jane",
		PrimaryEmail: "jane@example.com",
		Fields: []models.ContactField{
			{ID: "manual-email", Kind: "email", Value: "jane@example.com", IsPrimary: true, Source: "manual"},
			{ID: "provider-email", Kind: "email", Value: "jane.alt@example.com", Source: "synced:acc"},
		},
	})
	if err != nil {
		t.Fatalf("SaveContactProfile() error = %v", err)
	}

	updated, err := db.PreferContactField(ctx, "default", saved.ID, "provider-email")
	if err != nil {
		t.Fatalf("PreferContactField() error = %v", err)
	}
	if updated == nil {
		t.Fatalf("PreferContactField() returned nil profile")
	}
	if updated.PrimaryEmail != "jane.alt@example.com" {
		t.Fatalf("PrimaryEmail = %q, want provider email", updated.PrimaryEmail)
	}
	if len(updated.Fields) != 2 {
		t.Fatalf("len(Fields) = %d, want provider and manual fields preserved", len(updated.Fields))
	}
	primary := ""
	for _, field := range updated.Fields {
		if field.Kind == "email" && field.IsPrimary {
			primary = field.ID
		}
	}
	if primary != "provider-email" {
		t.Fatalf("primary email field = %q, want provider-email", primary)
	}
	found, err := db.FindContactProfileByIdentity(ctx, "default", "email", "jane@example.com")
	if err != nil {
		t.Fatalf("FindContactProfileByIdentity(old) error = %v", err)
	}
	if found == nil || found.ID != saved.ID {
		t.Fatalf("old identity = %#v, want preserved profile identity", found)
	}
}

func TestMigrateV39ToV40WipesLegacyContacts(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "gofer.db")
	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("New() initial error = %v", err)
	}
	if _, err := db.Write().Exec(`INSERT INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default') ON CONFLICT(id) DO NOTHING`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.SaveContact(ctx, "default", models.Contact{Name: "Legacy", Email: "legacy@example.com"}); err != nil {
		t.Fatalf("SaveContact() error = %v", err)
	}
	if _, err := db.Write().Exec(`UPDATE schema_version SET version = 39`); err != nil {
		t.Fatalf("downgrade schema marker: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	db, err = New(dbPath)
	if err != nil {
		t.Fatalf("New() after migration error = %v", err)
	}
	defer db.Close()

	var legacyCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM contacts`).Scan(&legacyCount); err != nil {
		t.Fatalf("count legacy contacts: %v", err)
	}
	if legacyCount != 0 {
		t.Fatalf("legacy contacts after v40 migration = %d, want 0", legacyCount)
	}
	var profileTable string
	if err := db.Read().QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'contact_profiles'`).Scan(&profileTable); err != nil {
		t.Fatalf("contact_profiles table missing: %v", err)
	}
}
