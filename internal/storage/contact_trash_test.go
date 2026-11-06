package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestMigrateV123AddsTheContactTrash(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Write().Exec(`DROP INDEX idx_contact_profiles_trash; ALTER TABLE contact_profiles DROP COLUMN trashed_at;
DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(122);`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV122ToV123(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Write().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if present, err := columnExistsTx(tx, "contact_profiles", "trashed_at"); err != nil || !present {
		t.Fatalf("trashed_at present = %v, %v", present, err)
	}
	var version int
	if err := tx.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 123 {
		t.Fatalf("schema version = %d, %v; want 123", version, err)
	}
}

func saveTrashTestContact(t *testing.T, db *DB, name, email string) string {
	t.Helper()
	contact, err := db.SaveContact(context.Background(), "default", models.Contact{Name: name, Email: email})
	if err != nil {
		t.Fatalf("SaveContact(%s) error = %v", name, err)
	}
	return contact.ID
}

func TestTrashedContactsLeaveTheListAndComeBackRestored(t *testing.T) {
	db := newContactsTestDB(t)
	ctx := context.Background()
	kept := saveTrashTestContact(t, db, "Nell Ortiz", "nell@example.com")
	gone := saveTrashTestContact(t, db, "Rufus Hale", "rufus@example.com")

	if count, err := db.TrashContacts(ctx, "default", []string{gone, "no-such-contact"}); err != nil || count != 1 {
		t.Fatalf("TrashContacts() = %d, %v; want 1", count, err)
	}
	if again, err := db.TrashContacts(ctx, "default", []string{gone}); err != nil || again != 0 {
		t.Fatalf("trashing it again = %d, %v; want 0", again, err)
	}
	list := func(trash bool) []string {
		t.Helper()
		contacts, err := db.ListContacts(ctx, "default", models.ContactFilters{Trash: trash, SortBy: "name", SortOrder: "asc"}, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, contact := range contacts {
			ids = append(ids, contact.ID)
			if trash && contact.TrashedAt == "" {
				t.Fatalf("trashed contact %s has no trash date", contact.Name)
			}
		}
		return ids
	}
	if got := list(false); len(got) != 1 || got[0] != kept {
		t.Fatalf("contacts = %v, want only the kept one", got)
	}
	if got := list(true); len(got) != 1 || got[0] != gone {
		t.Fatalf("trash = %v, want the trashed one", got)
	}
	if contact, err := db.GetContact(ctx, "default", gone); err != nil || contact != nil {
		t.Fatalf("GetContact(trashed) = %v, %v; want none", contact, err)
	}
	if contact, err := db.GetContactIncludingTrash(ctx, "default", gone); err != nil || contact == nil || contact.Name != "Rufus Hale" {
		t.Fatalf("GetContactIncludingTrash(trashed) = %v, %v", contact, err)
	}
	if due, err := db.TrashedContactIDs(ctx, "default", time.Now().Add(-ContactTrashRetention), 10); err != nil || len(due) != 0 {
		t.Fatalf("due for purge = %v, %v; want none yet", due, err)
	}
	if all, err := db.TrashedContactIDs(ctx, "default", time.Time{}, 10); err != nil || len(all) != 1 {
		t.Fatalf("all trashed = %v, %v; want the one", all, err)
	}

	if count, err := db.RestoreContacts(ctx, "default", []string{gone, kept}); err != nil || count != 1 {
		t.Fatalf("RestoreContacts() = %d, %v; want 1", count, err)
	}
	if got := list(false); len(got) != 2 {
		t.Fatalf("contacts after restore = %v, want both", got)
	}
}

// New mail from a contact in the Trash counts toward it, but doesn't take it
// out of the Trash.
func TestMailFromATrashedContactLeavesItInTheTrash(t *testing.T) {
	db := newContactsTestDB(t)
	ctx := context.Background()
	syncContactTestMail(t, db, "default", "Ada Moss", "ada@example.com", time.Now().Add(-time.Hour))
	contacts, err := db.ListContacts(ctx, "default", models.ContactFilters{}, 10, 0)
	if err != nil || len(contacts) != 1 {
		t.Fatalf("observed contacts = %v, %v", contacts, err)
	}
	id := contacts[0].ID
	if _, err := db.TrashContacts(ctx, "default", []string{id}); err != nil {
		t.Fatal(err)
	}
	syncContactTestMail(t, db, "default", "Ada Moss", "ada@example.com", time.Now())
	if trashed, err := db.ContactInTrash(ctx, "default", id); err != nil || !trashed {
		t.Fatalf("still in the trash = %v, %v; want true", trashed, err)
	}
	trash, err := db.ListContacts(ctx, "default", models.ContactFilters{Trash: true}, 10, 0)
	if err != nil || len(trash) != 1 || trash[0].MessageCount != 2 {
		t.Fatalf("trash = %#v, %v; want the contact with both messages counted", trash, err)
	}
	if active, _ := db.CountContacts(ctx, "default", models.ContactFilters{}); active != 0 {
		t.Fatalf("active contacts = %d, want 0", active)
	}
}

// An address-book sync finds the trashed contact by its copy there and leaves
// it alone: no duplicate, and the link survives for a restore.
func TestSyncLeavesATrashedContactInTheTrash(t *testing.T) {
	db := newInboundContactFixture(t)
	first := publishTestContact(t, db, InboundContact{RemoteID: "people/c7", Contact: models.Contact{Name: "Mae Linden", Email: "mae@example.com"}})
	if _, err := db.TrashContacts(t.Context(), "default", []string{first.ProfileID}); err != nil {
		t.Fatal(err)
	}
	again := publishTestContact(t, db, InboundContact{RemoteID: "people/c7", Contact: models.Contact{Name: "Mae Linden-Park", Email: "mae@example.com"}})
	if again.ProfileID != "" {
		t.Fatalf("sync result = %#v, want the trashed contact skipped", again)
	}
	var profiles, cards int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_profiles WHERE user_id='default'`).Scan(&profiles); err != nil || profiles != 1 {
		t.Fatalf("profiles = %d, %v; want no duplicate", profiles, err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_cards WHERE profile_id=? AND kind='provider' AND is_deleted=0`, first.ProfileID).Scan(&cards); err != nil || cards != 1 {
		t.Fatalf("provider cards = %d, %v; want the link kept", cards, err)
	}
	if _, err := db.RestoreContacts(t.Context(), "default", []string{first.ProfileID}); err != nil {
		t.Fatal(err)
	}
	if contact, err := db.GetContact(t.Context(), "default", first.ProfileID); err != nil || contact == nil || contact.Name != "Mae Linden" {
		t.Fatalf("restored contact = %#v, %v; want it as it was trashed", contact, err)
	}
}

// Deleting a trashed contact for good leaves it deleted but out of the Trash.
func TestDeletingATrashedContactForGoodEmptiesItFromTheTrash(t *testing.T) {
	db := newContactsTestDB(t)
	ctx := context.Background()
	id := saveTrashTestContact(t, db, "Ivo Brandt", "ivo@example.com")
	if _, err := db.TrashContacts(ctx, "default", []string{id}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteContact(ctx, "default", id, true); err != nil {
		t.Fatal(err)
	}
	if trashed, err := db.ContactInTrash(ctx, "default", id); err != nil || trashed {
		t.Fatalf("in the trash = %v, %v; want false", trashed, err)
	}
	if contact, err := db.GetContactIncludingTrash(ctx, "default", id); err != nil || contact != nil {
		t.Fatalf("deleted contact = %v, %v; want it gone", contact, err)
	}
	if restored, err := db.RestoreContacts(ctx, "default", []string{id}); err != nil || restored != 0 {
		t.Fatalf("restoring a deleted contact = %d, %v; want 0", restored, err)
	}
}

// A contact deleted on the account it lives on goes to the Trash when Gofer
// Sync is off; one with Gofer Sync on, or living elsewhere, only loses that
// copy.
func TestContactDeletedAtHomeGoesToTheTrash(t *testing.T) {
	inTrash := func(t *testing.T, db *DB, id string) bool {
		t.Helper()
		trashed, err := db.ContactInTrash(t.Context(), "default", id)
		if err != nil {
			t.Fatal(err)
		}
		return trashed
	}
	t.Run("shared", func(t *testing.T) {
		db := newInboundContactFixture(t)
		home := publishTestContact(t, db, InboundContact{Contact: models.Contact{Email: "home@example.com"}, RemoteID: "people/home"})
		synced := publishTestContact(t, db, InboundContact{Contact: models.Contact{Email: "synced@example.com"}, RemoteID: "people/synced"})
		if _, err := db.Write().Exec(`UPDATE contact_profiles SET sync_enabled=1 WHERE id=?`, synced.ProfileID); err != nil {
			t.Fatal(err)
		}
		local, err := db.SaveContact(t.Context(), "default", models.Contact{Name: "Local Lou", Email: "local@example.com", StoredIn: "local"})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertContactSource(t.Context(), ContactSource{UserID: "default", ContactID: local.ID, Provider: "gmail", AccountID: "source", RemoteID: "people/local"}); err != nil {
			t.Fatal(err)
		}
		for _, remote := range []string{"people/home", "people/synced", "people/local"} {
			if err := db.RemoveDeletedContactSource(t.Context(), "default", "gmail", "source", remote); err != nil {
				t.Fatal(err)
			}
		}
		if !inTrash(t, db, home.ProfileID) || inTrash(t, db, synced.ProfileID) || inTrash(t, db, local.ID) {
			t.Fatal("trash", inTrash(t, db, home.ProfileID), inTrash(t, db, synced.ProfileID), inTrash(t, db, local.ID))
		}
	})
	t.Run("book", func(t *testing.T) {
		db := newInboundContactFixture(t)
		book := models.ContactAddressBook{ID: "dav-book", URL: "https://dav.test/book/"}
		results, err := db.PublishInboundContactBook(t.Context(), "default", "dav", book, InboundContactBookPage{
			Contacts: []InboundContact{
				{Contact: models.Contact{Email: "home@example.com"}, RemoteID: "https://dav.test/book/home.vcf"},
				{Contact: models.Contact{Email: "synced@example.com"}, RemoteID: "https://dav.test/book/synced.vcf"},
			}, SyncToken: "one",
		}, inboundTestGuard, func(*sql.Tx) error { return nil })
		if err != nil || len(results) != 2 {
			t.Fatal(results, err)
		}
		if _, err := db.Write().Exec(`UPDATE contact_profiles SET sync_enabled=1 WHERE id=?`, results[1].ProfileID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.PublishInboundContactBook(t.Context(), "default", "dav", book, InboundContactBookPage{
			DeletedRemoteIDs: []string{"https://dav.test/book/home.vcf", "https://dav.test/book/synced.vcf"}, SyncToken: "two",
		}, inboundTestGuard, func(*sql.Tx) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if !inTrash(t, db, results[0].ProfileID) || inTrash(t, db, results[1].ProfileID) {
			t.Fatal("trash", inTrash(t, db, results[0].ProfileID), inTrash(t, db, results[1].ProfileID))
		}
		// The synced one waits to be resolved instead.
		if deletions, err := db.ContactRemoteDeletions(t.Context(), "default", results[1].ProfileID); err != nil || len(deletions) != 1 || deletions[0].Place != "book:dav-book" {
			t.Fatal("to resolve", deletions, err)
		}
	})
}

// Restored to another place, a contact lives there: the old home keeps
// nothing of it, and with Gofer Sync on it syncs with the new home.
func TestRestoreContactsToAnotherPlace(t *testing.T) {
	ctx := context.Background()
	place := func(t *testing.T, db *DB, id string) (string, []string) {
		t.Helper()
		contact, err := db.GetContact(ctx, "default", id)
		if err != nil || contact == nil {
			t.Fatal(contact, err)
		}
		return contact.StoredIn, contact.SaveTargets
	}
	t.Run("local to an account", func(t *testing.T) {
		db := newInboundContactFixture(t)
		saved, err := db.SaveContact(ctx, "default", models.Contact{Name: "Lena Vos", Email: "lena@example.com", StoredIn: "local"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.TrashContacts(ctx, "default", []string{saved.ID}); err != nil {
			t.Fatal(err)
		}
		if count, err := db.RestoreContactsTo(ctx, "default", []string{saved.ID}, "account:source"); err != nil || count != 1 {
			t.Fatal(count, err)
		}
		if home, targets := place(t, db, saved.ID); home != "account:source" || len(targets) != 0 {
			t.Fatal("restored", home, targets)
		}
	})
	t.Run("synced to local", func(t *testing.T) {
		db := newInboundContactFixture(t)
		id := newUnresolvedTestContact(t, db)
		if _, err := db.TrashContacts(ctx, "default", []string{id}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.RestoreContactsTo(ctx, "default", []string{id}, "local"); err != nil {
			t.Fatal(err)
		}
		home, targets := place(t, db, id)
		if home != "local" || !slices.Contains(targets, "local") || slices.Contains(targets, "account:source") || !slices.Contains(targets, "account:target") {
			t.Fatal("restored", home, targets)
		}
	})
	t.Run("synced to another account", func(t *testing.T) {
		db := newInboundContactFixture(t)
		id := newUnresolvedTestContact(t, db)
		if _, err := db.Write().Exec(`DELETE FROM contact_sync_memberships WHERE account_id='target'`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.TrashContacts(ctx, "default", []string{id}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.RestoreContactsTo(ctx, "default", []string{id}, "account:target"); err != nil {
			t.Fatal(err)
		}
		if home, targets := place(t, db, id); home != "account:target" || !slices.Contains(targets, "account:target") || slices.Contains(targets, "account:source") {
			t.Fatal("restored", home, targets)
		}
	})
}
