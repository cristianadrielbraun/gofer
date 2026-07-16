package storage

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
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

func TestMigrateV70SeparatesContactSyncMembershipsFromCards(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gofer.db")
	db, err := New(path)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := db.Write().Exec(`INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO contact_profiles (id, user_id, display_name, primary_email) VALUES ('profile-1', 'default', 'Jane', 'jane@example.com')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO contact_cards (id, user_id, profile_id, kind, account_id) VALUES ('target-1', 'default', 'profile-1', 'target', 'acc')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`DELETE FROM schema_version`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT OR REPLACE INTO schema_version (version) VALUES (70)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New(path)
	if err != nil {
		t.Fatalf("reopen migrated DB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var memberships, targetCards int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_sync_memberships WHERE profile_id = 'profile-1' AND account_id = 'acc' AND enabled = 1`).Scan(&memberships); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_cards WHERE profile_id = 'profile-1' AND kind = 'target'`).Scan(&targetCards); err != nil {
		t.Fatal(err)
	}
	if memberships != 1 || targetCards != 0 {
		t.Fatalf("memberships=%d target cards=%d, want 1 and 0", memberships, targetCards)
	}
}

func TestMigrateV74ConvertsPreferencesToCanonicalFieldsAndDropsPreferenceTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	db, err := New(path)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := db.Write().Exec(`
		INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default');
		INSERT INTO contact_profiles (id, user_id, display_name, primary_email, sync_enabled)
		VALUES ('profile-1', 'default', 'Jane', 'jane@example.com', 1);
		INSERT INTO contact_fields (id, user_id, profile_id, kind, value, normalized_value, is_primary, ordinal, source)
		VALUES
			('email-a', 'default', 'profile-1', 'email', 'jane@example.com', 'jane@example.com', 1, 1, 'manual'),
			('email-b', 'default', 'profile-1', 'email', 'jane.alt@example.com', 'jane.alt@example.com', 1, 1, 'synced:acc');
		CREATE TABLE contact_field_preferences (
			user_id TEXT NOT NULL, profile_id TEXT NOT NULL, field_kind TEXT NOT NULL,
			preferred_normalized_value TEXT NOT NULL DEFAULT '', created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY (user_id, profile_id, field_kind)
		);
		INSERT INTO contact_field_preferences (user_id, profile_id, field_kind, preferred_normalized_value)
		VALUES ('default', 'profile-1', 'email', 'jane.alt@example.com');
		DELETE FROM schema_version;
		INSERT INTO schema_version (version) VALUES (74)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New(path)
	if err != nil {
		t.Fatalf("reopen migrated DB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var preferenceTables int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'contact_field_preferences'`).Scan(&preferenceTables); err != nil {
		t.Fatal(err)
	}
	if preferenceTables != 0 {
		t.Fatalf("contact_field_preferences tables = %d, want removed", preferenceTables)
	}
	var primary string
	if err := db.Read().QueryRow(`SELECT value FROM contact_fields WHERE profile_id = 'profile-1' AND source = 'canonical' AND kind = 'email' AND is_primary = 1`).Scan(&primary); err != nil {
		t.Fatalf("canonical primary email missing after migration: %v", err)
	}
	if primary != "jane.alt@example.com" {
		t.Fatalf("canonical primary email = %q, want persisted bootstrap choice", primary)
	}
}

func TestMigrateV72SeparatesObservedOriginFromLocalTarget(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO contact_profiles (id, user_id, display_name, primary_email, origin) VALUES ('observed-profile', 'default', 'Observed', 'observed@example.com', 'manual')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO contact_cards (id, user_id, profile_id, kind) VALUES ('observed-card', 'default', 'observed-profile', 'observed')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO contact_observations (id, user_id, profile_id, email, normalized_email, observed_name, message_count) VALUES ('observation', 'default', 'observed-profile', 'observed@example.com', 'observed@example.com', 'Observed', 1)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV72ToV73(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migrateV72ToV73() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var origin, kind string
	if err := db.Read().QueryRowContext(ctx, `SELECT origin FROM contact_profiles WHERE id = 'observed-profile'`).Scan(&origin); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(ctx, `SELECT kind FROM contact_cards WHERE id = 'observed-card'`).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if origin != "observed" || kind != "local" {
		t.Fatalf("origin=%q kind=%q, want observed/local", origin, kind)
	}
}

func TestMigrateV73BackfillsExplicitContactSyncEnablement(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO contact_profiles (id, user_id, display_name, primary_email, sync_enabled) VALUES ('synced-profile', 'default', 'Synced', 'synced@example.com', 0), ('local-profile', 'default', 'Local', 'local@example.com', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO contact_sync_memberships (id, user_id, profile_id, account_id, enabled) VALUES ('membership-1', 'default', 'synced-profile', 'account-1', 1)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV73ToV74(tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var syncedEnabled, localEnabled int
	if err := db.Read().QueryRowContext(ctx, `SELECT sync_enabled FROM contact_profiles WHERE id = 'synced-profile'`).Scan(&syncedEnabled); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(ctx, `SELECT sync_enabled FROM contact_profiles WHERE id = 'local-profile'`).Scan(&localEnabled); err != nil {
		t.Fatal(err)
	}
	if syncedEnabled != 1 || localEnabled != 0 {
		t.Fatalf("sync flags = %d/%d, want 1/0", syncedEnabled, localEnabled)
	}
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
	if manual.IsManual || manual.Source != "observed" {
		t.Fatalf("edited observed contact origin = %q/manual:%v, want immutable observed origin", manual.Source, manual.IsManual)
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

func TestSaveContactCanReplaceAndRemoveProfileAvatar(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	dataURL := "data:image/png;base64,iVBORw0KGgo="
	saved, err := db.SaveContact(ctx, "default", models.Contact{Name: "Avatar Contact", Email: "avatar@example.com", AvatarURL: dataURL})
	if err != nil {
		t.Fatalf("SaveContact() error = %v", err)
	}
	if saved.AvatarURL != dataURL {
		t.Fatalf("saved AvatarURL = %q, want custom data URL", saved.AvatarURL)
	}

	preserved, err := db.SaveContact(ctx, "default", models.Contact{ID: saved.ID, Name: saved.Name, Email: saved.Email})
	if err != nil {
		t.Fatalf("SaveContact() preserve error = %v", err)
	}
	if preserved.AvatarURL != dataURL {
		t.Fatalf("preserved AvatarURL = %q, want custom data URL", preserved.AvatarURL)
	}
	if _, _, err := db.UpsertSyncedContactFromContact(ctx, "default", "gmail-a", models.Contact{Name: saved.Name, Email: saved.Email, AvatarURL: "https://photos.example/provider.jpg"}); err != nil {
		t.Fatalf("UpsertSyncedContactFromContact() error = %v", err)
	}
	withProviderSync, err := db.GetContact(ctx, "default", saved.ID)
	if err != nil {
		t.Fatalf("GetContact() after provider sync error = %v", err)
	}
	if withProviderSync == nil || withProviderSync.AvatarURL != dataURL {
		t.Fatalf("avatar after provider sync = %#v, want custom avatar preserved", withProviderSync)
	}

	removed, err := db.SaveContact(ctx, "default", models.Contact{ID: saved.ID, Name: saved.Name, Email: saved.Email, RemoveAvatar: true})
	if err != nil {
		t.Fatalf("SaveContact() remove error = %v", err)
	}
	if removed.AvatarURL != "" {
		t.Fatalf("removed AvatarURL = %q, want empty", removed.AvatarURL)
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
		ID:               saved.ID,
		Name:             "Jane",
		Email:            "jane@example.com",
		GoferSyncEnabled: true,
		SaveTargets:      []string{"account:acc", "local", "account:acc"},
	})
	if err != nil {
		t.Fatalf("SaveContact() update error = %v", err)
	}
	want := []string{"local", "account:acc"}
	if !reflect.DeepEqual(saved.SaveTargets, want) {
		t.Fatalf("updated SaveTargets = %#v, want %#v", saved.SaveTargets, want)
	}
	if !saved.GoferSyncEnabled {
		t.Fatal("updated GoferSyncEnabled = false, want true")
	}
	saved, err = db.SaveContact(ctx, "default", models.Contact{
		ID:          saved.ID,
		Name:        "Jane",
		Email:       "jane@example.com",
		SaveTargets: want,
	})
	if err != nil {
		t.Fatalf("SaveContact() disable sync error = %v", err)
	}
	if saved.GoferSyncEnabled || !reflect.DeepEqual(saved.SaveTargets, want) {
		t.Fatalf("disabled contact = enabled:%v targets:%#v, want disabled with saved targets %#v", saved.GoferSyncEnabled, saved.SaveTargets, want)
	}
}

func TestStoppingContactSyncKeepsExistingProviderCopy(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	saved, err := db.SaveContact(ctx, "default", models.Contact{Name: "Jane", Email: "jane@example.com", SaveTargets: []string{"account:acc"}})
	if err != nil {
		t.Fatalf("SaveContact() error = %v", err)
	}
	if err := db.UpsertContactSource(ctx, ContactSource{ContactID: saved.ID, UserID: "default", Provider: "gmail", AccountID: "acc", RemoteID: "people/jane"}); err != nil {
		t.Fatalf("UpsertContactSource() error = %v", err)
	}
	if err := db.ReplaceContactSyncMemberships(ctx, "default", saved.ID, []string{"local"}); err != nil {
		t.Fatalf("ReplaceContactSyncMemberships() error = %v", err)
	}
	targets, err := db.GetContactSaveTargets(ctx, "default", saved.ID)
	if err != nil || !reflect.DeepEqual(targets, []string{"local"}) {
		t.Fatalf("GetContactSaveTargets() = %#v, %v; want local only", targets, err)
	}
	source, err := db.GetContactSource(ctx, "default", saved.ID, "gmail", "acc")
	if err != nil || source == nil || source.RemoteID != "people/jane" {
		t.Fatalf("provider copy = %#v, %v; want existing remote copy preserved", source, err)
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
	if err != nil || got == nil || got.Source != "synced:acc" || got.IsManual {
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

func TestListContactSyncStatusesIncludesOutlookOAuthAccounts(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, email_address, display_name, auth_method)
		VALUES ('outlook_acc', 'default', 'outlook', 'jane@outlook.com', 'Jane Outlook', 'oauth2')`); err != nil {
		t.Fatalf("insert outlook account: %v", err)
	}

	statuses, err := db.ListContactSyncStatuses(ctx, "default")
	if err != nil {
		t.Fatalf("ListContactSyncStatuses() error = %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %#v, want one Outlook status", statuses)
	}
	if !statuses[0].Enabled || !statuses[0].Capable || statuses[0].Provider != "outlook" {
		t.Fatalf("status = %#v, want enabled Outlook contact sync", statuses[0])
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
		AvatarURL:        "https://photos.example/jane.jpg",
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
	if got.AvatarURL != "https://photos.example/jane.jpg" || got.AvatarSource != "provider_contact" {
		t.Fatalf("synced avatar = %q source=%q, want provider avatar", got.AvatarURL, got.AvatarSource)
	}
	matches, err := db.SearchContacts(ctx, "default", "jane@example.com", 5)
	if err != nil || len(matches) != 1 {
		t.Fatalf("SearchContacts() = %#v, %v; want synced contact", matches, err)
	}
	if matches[0].AvatarURL != "https://photos.example/jane.jpg" || matches[0].AvatarSource != "provider_contact" {
		t.Fatalf("search avatar = %q source=%q, want provider avatar", matches[0].AvatarURL, matches[0].AvatarSource)
	}

	hash := avatarresolver.GravatarHash("jane@example.com")
	if err := db.SaveSenderAvatarFound(ctx, hash, "jane@example.com", "gravatar", "image/png", "", []byte("png"), time.Now().Add(time.Hour), "found", "skipped"); err != nil {
		t.Fatalf("SaveSenderAvatarFound() error = %v", err)
	}
	got, err = db.GetContact(ctx, "default", contactID)
	if err != nil {
		t.Fatalf("GetContact() with sender avatar error = %v", err)
	}
	if got.AvatarURL != "https://photos.example/jane.jpg" || got.AvatarSource != "provider_contact" {
		t.Fatalf("avatar with sender cache = %q source=%q, want provider avatar priority", got.AvatarURL, got.AvatarSource)
	}
	matches, err = db.SearchContacts(ctx, "default", "jane@example.com", 5)
	if err != nil || len(matches) != 1 {
		t.Fatalf("SearchContacts() with sender avatar = %#v, %v; want synced contact", matches, err)
	}
	if matches[0].AvatarURL != "https://photos.example/jane.jpg" || matches[0].AvatarSource != "provider_contact" {
		t.Fatalf("search avatar with sender cache = %q source=%q, want provider avatar priority", matches[0].AvatarURL, matches[0].AvatarSource)
	}
	providerAvatars, err := db.GetProviderContactAvatarsByEmail(ctx, "default", []string{"jane@example.com"})
	if err != nil {
		t.Fatalf("GetProviderContactAvatarsByEmail() error = %v", err)
	}
	if providerAvatars["jane@example.com"] != "https://photos.example/jane.jpg" {
		t.Fatalf("provider avatars = %#v, want synced provider avatar", providerAvatars)
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
	if got.AvatarURL != "https://photos.example/jane.jpg" || got.AvatarSource != "provider_contact" {
		t.Fatalf("avatar after manual edit = %q source=%q, want provider avatar preserved", got.AvatarURL, got.AvatarSource)
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

func TestSyncedProviderChangeBecomesCanonicalOnceAndUnchangedReadbackDoesNotEcho(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	contactID, _, err := db.UpsertSyncedContactFromContact(ctx, "default", "acc-a", models.Contact{Name: "Jane", Email: "jane@example.com", Phone: "+1 555 0100"})
	if err != nil {
		t.Fatalf("first provider upsert: %v", err)
	}
	if _, _, err := db.UpsertSyncedContactFromContact(ctx, "default", "acc-b", models.Contact{Name: "Jane", Email: "jane@example.com", Phone: "+1 555 0200"}); err != nil {
		t.Fatalf("second provider upsert: %v", err)
	}
	profile, err := db.GetContactProfile(ctx, "default", contactID)
	if err != nil {
		t.Fatalf("GetContactProfile() error = %v", err)
	}
	selectedPhoneID := ""
	for _, field := range profile.Fields {
		if field.Source == "synced:acc-b" && field.Kind == "phone" {
			selectedPhoneID = field.ID
		}
	}
	if selectedPhoneID == "" {
		t.Fatal("second provider phone field not found")
	}
	if err := db.InitializeContactCanonicalFields(ctx, "default", contactID, map[string]string{"phone": selectedPhoneID}); err != nil {
		t.Fatalf("InitializeContactCanonicalFields() error = %v", err)
	}
	if err := db.ReplaceContactSyncMemberships(ctx, "default", contactID, []string{"account:acc-a", "account:acc-b"}); err != nil {
		t.Fatalf("ReplaceContactSyncMemberships() error = %v", err)
	}
	if err := db.SetContactProfileSyncEnabled(ctx, "default", contactID, true); err != nil {
		t.Fatalf("SetContactProfileSyncEnabled() error = %v", err)
	}
	changedContact := models.Contact{Name: "Jane Updated", Email: "jane@example.com", Phone: "+1 555 0300"}
	_, _, canonicalChanged, err := db.UpsertSyncedContactFromContactWithChange(ctx, "default", "acc-b", changedContact)
	if err != nil {
		t.Fatalf("provider change: %v", err)
	}
	if !canonicalChanged {
		t.Fatal("provider change did not update canonical contact")
	}
	contact, err := db.GetContact(ctx, "default", contactID)
	if err != nil {
		t.Fatalf("GetContact() after provider change: %v", err)
	}
	if contact == nil || contact.Phone != "+1 555 0300" || contact.Name != "Jane Updated" {
		t.Fatalf("canonical contact = %#v, want provider change", contact)
	}
	_, _, canonicalChanged, err = db.UpsertSyncedContactFromContactWithChange(ctx, "default", "acc-b", changedContact)
	if err != nil {
		t.Fatalf("unchanged provider readback: %v", err)
	}
	if canonicalChanged {
		t.Fatal("unchanged provider readback incorrectly changed canonical contact")
	}
}

func TestContactProfileUsesProviderAvatarFromSameRemoteContact(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users (id, email, name) VALUES ('user_b', 'user-b@example.com', 'User B');
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address, auth_method)
		VALUES
			('gmail_a', 'default', 'gmail', 'google-subject-1', 'owner@example.com', 'oauth2'),
			('gmail_b', 'user_b', 'gmail', 'google-subject-1', 'owner@example.com', 'oauth2')`); err != nil {
		t.Fatalf("insert users/accounts: %v", err)
	}

	avatarURL := "https://lh3.googleusercontent.com/a-/photo=s100"
	contactA, _, err := db.UpsertSyncedContactFromContact(ctx, "default", "gmail_a", models.Contact{Name: "Provider Photo Contact", Email: "photo-contact@example.com", AvatarURL: avatarURL})
	if err != nil {
		t.Fatalf("UpsertSyncedContactFromContact(default) error = %v", err)
	}
	if err := db.UpsertContactSource(ctx, ContactSource{ContactID: contactA, UserID: "default", Provider: "gmail", AccountID: "gmail_a", RemoteID: "people/123"}); err != nil {
		t.Fatalf("UpsertContactSource(default) error = %v", err)
	}

	contactB, _, err := db.UpsertSyncedContactFromContact(ctx, "user_b", "gmail_b", models.Contact{Name: "Provider Photo Contact", Email: "photo-contact@example.com"})
	if err != nil {
		t.Fatalf("UpsertSyncedContactFromContact(user_b) error = %v", err)
	}
	if err := db.UpsertContactSource(ctx, ContactSource{ContactID: contactB, UserID: "user_b", Provider: "gmail", AccountID: "gmail_b", RemoteID: "people/123"}); err != nil {
		t.Fatalf("UpsertContactSource(user_b) error = %v", err)
	}

	got, err := db.GetContact(ctx, "user_b", contactB)
	if err != nil {
		t.Fatalf("GetContact() error = %v", err)
	}
	if got.AvatarURL != avatarURL || got.AvatarSource != "provider_contact" {
		t.Fatalf("fallback avatar = %q source=%q, want provider remote avatar", got.AvatarURL, got.AvatarSource)
	}

	providerAvatars, err := db.GetProviderContactAvatarsByEmail(ctx, "user_b", []string{"photo-contact@example.com"})
	if err != nil {
		t.Fatalf("GetProviderContactAvatarsByEmail() error = %v", err)
	}
	if providerAvatars["photo-contact@example.com"] != avatarURL {
		t.Fatalf("provider avatar fallback = %#v, want provider remote avatar", providerAvatars)
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
	observedMatches, err := db.SearchContacts(ctx, "default", "observed@example.com", 10)
	if err != nil || len(observedMatches) != 1 {
		t.Fatalf("SearchContacts(observed) = %#v, %v", observedMatches, err)
	}
	observed := observedMatches[0]
	observed.Name = "Edited Observed"
	observed.SaveTargets = []string{"local"}
	if _, err := db.SaveContact(ctx, "default", observed); err != nil {
		t.Fatalf("SaveContact(observed edit) error = %v", err)
	}

	contacts, err := db.ListContacts(ctx, "default", models.ContactFilters{Source: "manual"}, 10, 0)
	if err != nil || len(contacts) != 1 || contacts[0].ID != manual.ID {
		t.Fatalf("manual filter = %#v, %v; want manual contact", contacts, err)
	}
	contacts, err = db.ListContacts(ctx, "default", models.ContactFilters{Source: "observed"}, 10, 0)
	if err != nil || len(contacts) != 1 || contacts[0].Email != "observed@example.com" || contacts[0].Source != "observed" {
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

func TestListContactsSortsByNameInBothDirections(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	for _, contact := range []models.Contact{
		{Name: "Zulu", Email: "zulu@example.com"},
		{Name: "Alpha", Email: "alpha@example.com"},
	} {
		if _, err := db.SaveContact(ctx, "default", contact); err != nil {
			t.Fatalf("SaveContact() error = %v", err)
		}
	}

	ascending, err := db.ListContacts(ctx, "default", models.ContactFilters{SortBy: "name", SortOrder: "asc"}, 10, 0)
	if err != nil || len(ascending) != 2 || ascending[0].Name != "Alpha" || ascending[1].Name != "Zulu" {
		t.Fatalf("ascending contacts = %#v, %v", ascending, err)
	}
	descending, err := db.ListContacts(ctx, "default", models.ContactFilters{SortBy: "name", SortOrder: "desc"}, 10, 0)
	if err != nil || len(descending) != 2 || descending[0].Name != "Zulu" || descending[1].Name != "Alpha" {
		t.Fatalf("descending contacts = %#v, %v", descending, err)
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
