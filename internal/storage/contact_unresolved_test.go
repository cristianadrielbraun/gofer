package storage

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
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

	if kept, err := db.KeepContactWithoutDeletedPlaces(ctx, "default", id, ""); err != nil || !kept {
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
	if kept, err := db.KeepContactWithoutDeletedPlaces(ctx, "default", id, ""); err != nil || kept {
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
	if kept, err := db.KeepContactWithoutDeletedPlaces(ctx, "default", id, ""); err != nil || !kept {
		t.Fatal("keep", kept, err)
	}
	if contact, _ := db.GetContact(ctx, "default", id); contact == nil || contact.StoredIn != "account:target" {
		t.Fatal("home", contact)
	}
}

// Kept without its home, a contact can move to a place chosen among Gofer
// Local and the places that still have it. A place that no longer qualifies
// is refused and the contact stays to resolve.
func TestKeptContactWithoutItsHomeMovesToTheChosenPlace(t *testing.T) {
	ctx := context.Background()
	db := newInboundContactFixture(t)
	id := newUnresolvedTestContact(t, db)
	if err := db.RemoveDeletedContactSource(ctx, "default", "gmail", "source", "people/ines"); err != nil {
		t.Fatal(err)
	}
	contacts, err := db.ListUnresolvedContacts(ctx, "default", 10)
	if err != nil || len(contacts) != 1 {
		t.Fatal("to resolve", contacts, err)
	}
	homes, err := db.ContactNewHomes(ctx, "default", contacts)
	if err != nil {
		t.Fatal(err)
	}
	got := homes[id]
	if !slices.Equal(got.Choices, []string{"local", "account:target"}) || got.Default != "account:target" {
		t.Fatalf("new homes = %+v, want Local or the target, the target by default", got)
	}

	if kept, err := db.KeepContactWithoutDeletedPlaces(ctx, "default", id, "account:source"); !errors.Is(err, ErrContactNewHomeGone) || kept {
		t.Fatal("kept in the place that deleted it", kept, err)
	}
	if count, _ := db.CountUnresolvedContacts(ctx, "default"); count != 1 {
		t.Fatal("a refused choice resolved it", count)
	}
	if kept, err := db.KeepContactWithoutDeletedPlaces(ctx, "default", id, "local"); err != nil || !kept {
		t.Fatal("keep", kept, err)
	}
	contact, err := db.GetContact(ctx, "default", id)
	if err != nil || contact == nil || contact.StoredIn != "local" {
		t.Fatal("home", contact, err)
	}
	profile, err := db.GetContactProfile(ctx, "default", id)
	if err != nil || profile == nil {
		t.Fatal(err)
	}
	local := false
	for _, card := range profile.Cards {
		local = local || (card.Kind == "local" && !card.IsDeleted)
	}
	if !local {
		t.Fatal("moved to Gofer Local without a Local copy")
	}
}

// A contact deleted only in a sync location keeps its home: there is
// nothing to choose.
func TestContactDeletedInASyncLocationHasNoNewHome(t *testing.T) {
	ctx := context.Background()
	db := newInboundContactFixture(t)
	id := newUnresolvedTestContact(t, db)
	if err := db.RemoveDeletedContactSource(ctx, "default", "outlook", "target", "ines-target"); err != nil {
		t.Fatal(err)
	}
	contacts, err := db.ListUnresolvedContacts(ctx, "default", 10)
	if err != nil || len(contacts) != 1 {
		t.Fatal("to resolve", contacts, err)
	}
	if homes, err := db.ContactNewHomes(ctx, "default", contacts); err != nil || len(homes) != 0 {
		t.Fatal("new homes", homes, err)
	}
	// A home posted anyway is ignored.
	if kept, err := db.KeepContactWithoutDeletedPlaces(ctx, "default", id, "local"); err != nil || !kept {
		t.Fatal("keep", kept, err)
	}
	if contact, _ := db.GetContact(ctx, "default", id); contact == nil || contact.StoredIn != "account:source" {
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

// A Gmail or Outlook page of changes settles the contacts deleted there,
// as a CardDAV pull does, and keeps its sync token.
func TestInboundContactChangesSettleDeletedContacts(t *testing.T) {
	ctx := context.Background()
	db := newInboundContactFixture(t)
	page := func(provider, account string, changes InboundContactChangePage) {
		t.Helper()
		if _, err := db.PublishInboundContactChanges(ctx, "default", account, provider, changes, inboundTestGuard, nil); err != nil {
			t.Fatal(err)
		}
	}
	page("gmail", "source", InboundContactChangePage{Contacts: []InboundContact{{Contact: models.Contact{Name: "Teo Brandt", Email: "teo@example.com"}, RemoteID: "people/teo"}}, SyncToken: "token-one"})
	found, err := db.SearchContacts(ctx, "default", "teo@example.com", 5)
	if err != nil || len(found) != 1 {
		t.Fatal("published", found, err)
	}
	synced := newUnresolvedTestContact(t, db)

	page("gmail", "source", InboundContactChangePage{DeletedRemoteIDs: []string{"people/teo"}, SyncToken: "token-two"})
	if trashed, _ := db.ContactInTrash(ctx, "default", found[0].ID); !trashed {
		t.Fatal("the contact deleted in Gmail isn't in the Trash")
	}
	var token string
	if err := db.Read().QueryRow(`SELECT last_sync_token FROM account_contact_sync_configs WHERE account_id='source'`).Scan(&token); err != nil || token != "token-two" {
		t.Fatal("token", token, err)
	}
	// A page without a token keeps the last one.
	page("gmail", "source", InboundContactChangePage{})
	if err := db.Read().QueryRow(`SELECT last_sync_token FROM account_contact_sync_configs WHERE account_id='source'`).Scan(&token); err != nil || token != "token-two" {
		t.Fatal("token kept", token, err)
	}

	page("outlook", "target", InboundContactChangePage{DeletedRemoteIDs: []string{"ines-target"}, SyncToken: "delta-one"})
	if deletions, err := db.ContactRemoteDeletions(ctx, "default", synced); err != nil || len(deletions) != 1 || deletions[0].Place != "account:target" {
		t.Fatal("the synced contact deleted in Outlook isn't to resolve", deletions, err)
	}
}
