package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// newUnresolvedTestContact is a contact living on the source account with
// Gofer Sync on to the target account, with a copy in each.
func newUnresolvedTestContact(t *testing.T, db *DB) string {
	t.Helper()
	result := publishTestContact(t, db, InboundContact{Contact: models.Contact{Name: "Ines Varga", Email: "ines@example.com"}, RemoteID: "people/ines"})
	enableInboundFanout(t, db, result.ProfileID)
	if err := db.UpsertContactSource(t.Context(), ContactSource{UserID: "default", ContactID: result.ProfileID, Provider: "outlook", AccountID: "target", RemoteID: "ines-target"}); err != nil {
		t.Fatal(err)
	}
	return result.ProfileID
}

func listedContactIDs(t *testing.T, db *DB, filters models.ContactFilters) []string {
	t.Helper()
	contacts, err := db.ListContacts(t.Context(), "default", filters, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, contact := range contacts {
		ids = append(ids, contact.ID)
	}
	return ids
}

// A synced contact deleted in one of its places waits to be resolved:
// marked in the list, out of the Trash, its writes paused, until it is kept
// without that place.
func TestSyncedContactDeletedInAPlaceWaitsToBeResolved(t *testing.T) {
	ctx := context.Background()
	db := newInboundContactFixture(t)
	id := newUnresolvedTestContact(t, db)
	if _, err := db.EnqueueContactSyncOperation(ctx, "default", models.Contact{ID: id, Email: "ines@example.com", GoferSyncEnabled: true}, nil); err != nil {
		t.Fatal(err)
	}

	if err := db.RemoveDeletedContactSource(ctx, "default", "outlook", "target", "ines-target"); err != nil {
		t.Fatal(err)
	}
	// It stays in the list, marked with the place that deleted it.
	listed, err := db.ListContacts(ctx, "default", models.ContactFilters{}, 50, 0)
	if err != nil || len(listed) != 1 || len(listed[0].DeletedFrom) != 1 || listed[0].DeletedFrom[0] != "account:target" {
		t.Fatal("not marked in the list", listed, err)
	}
	if ids := listedContactIDs(t, db, models.ContactFilters{Unresolved: true}); len(ids) != 1 || ids[0] != id {
		t.Fatal("not to resolve", ids)
	}
	if trashed, err := db.ContactInTrash(ctx, "default", id); err != nil || trashed {
		t.Fatal("in the Trash", trashed, err)
	}
	if count, err := db.CountUnresolvedContacts(ctx, "default"); err != nil || count != 1 {
		t.Fatal("count", count, err)
	}
	contact, err := db.GetContact(ctx, "default", id)
	if err != nil || contact == nil || len(contact.DeletedFrom) != 1 || contact.DeletedFrom[0] != "account:target" {
		t.Fatal("deleted from", contact, err)
	}
	if ops, err := db.ClaimContactSyncOperations(ctx, 10, time.Minute); err != nil || len(ops) != 0 {
		t.Fatal("its writes went on", ops, err)
	}

	if kept, err := db.KeepContactWithoutDeletedPlaces(ctx, "default", id); err != nil || !kept {
		t.Fatal("keep", kept, err)
	}
	if listed, _ := db.ListContacts(ctx, "default", models.ContactFilters{}, 50, 0); len(listed) != 1 || len(listed[0].DeletedFrom) != 0 {
		t.Fatal("still marked", listed)
	}
	targets, err := db.GetContactSaveTargets(ctx, "default", id)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if target == "account:target" {
			t.Fatal("still syncs with the place that deleted it", targets)
		}
	}
	if contact, _ := db.GetContact(ctx, "default", id); contact == nil || contact.StoredIn != "account:source" || len(contact.DeletedFrom) != 0 {
		t.Fatal("kept", contact)
	}
	if ops, err := db.ClaimContactSyncOperations(ctx, 10, time.Minute); err != nil || len(ops) != 1 {
		t.Fatal("its writes didn't go on", ops, err)
	}
	if kept, err := db.KeepContactWithoutDeletedPlaces(ctx, "default", id); err != nil || kept {
		t.Fatal("kept twice", kept, err)
	}
}

// Kept without its home, a contact moves to a place that still has it.
func TestKeptContactWithoutItsHomeMovesToAnotherPlace(t *testing.T) {
	ctx := context.Background()
	db := newInboundContactFixture(t)
	id := newUnresolvedTestContact(t, db)
	if err := db.RemoveDeletedContactSource(ctx, "default", "gmail", "source", "people/ines"); err != nil {
		t.Fatal(err)
	}
	if trashed, _ := db.ContactInTrash(ctx, "default", id); trashed {
		t.Fatal("a synced contact went to the Trash")
	}
	if kept, err := db.KeepContactWithoutDeletedPlaces(ctx, "default", id); err != nil || !kept {
		t.Fatal("keep", kept, err)
	}
	if contact, _ := db.GetContact(ctx, "default", id); contact == nil || contact.StoredIn != "account:target" {
		t.Fatal("home", contact)
	}
}

// Deleting it everywhere moves it to the Trash, which resolves it.
func TestTrashingAContactToResolveResolvesIt(t *testing.T) {
	ctx := context.Background()
	db := newInboundContactFixture(t)
	id := newUnresolvedTestContact(t, db)
	if err := db.RemoveDeletedContactSource(ctx, "default", "outlook", "target", "ines-target"); err != nil {
		t.Fatal(err)
	}
	if count, err := db.TrashContacts(ctx, "default", []string{id}); err != nil || count != 1 {
		t.Fatal("trash", count, err)
	}
	if count, err := db.CountUnresolvedContacts(ctx, "default"); err != nil || count != 0 {
		t.Fatal("still to resolve", count, err)
	}
	if _, err := db.RestoreContacts(ctx, "default", []string{id}); err != nil {
		t.Fatal(err)
	}
	if ids := listedContactIDs(t, db, models.ContactFilters{}); len(ids) != 1 {
		t.Fatal("restored contact not in the list", ids)
	}
}

func TestMigrateV138ToV139AddsContactsToResolve(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Write().Exec(`DROP TABLE contact_remote_deletions; DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(138);`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV138ToV139(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Write().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if exists, err := tableExistsTx(tx, contactRemoteDeletionsTable); err != nil || !exists {
		t.Fatal("table", exists, err)
	}
	var version int
	if err := tx.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 139 {
		t.Fatal("version", version, err)
	}
}
