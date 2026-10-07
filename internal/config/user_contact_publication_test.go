package config

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserContactPublicationOwnerBoundAcrossCacheEviction(t *testing.T) {
	system, _, accounts, owners := newServiceAccountFixture(t)
	alice := serviceSnapshotFor(t, accounts, "alice", owners["alice"].ID)
	bob := serviceSnapshotFor(t, accounts, "bob", owners["bob"].ID)
	ids := map[string]string{}
	for _, snapshot := range []*AccountServiceSnapshot{alice, bob} {
		owner := snapshot.OwnerID()
		inputs := []storage.InboundContact{{Contact: models.Contact{ID: "supplied-profile", Email: "same@example.com", Phone: owner}, RemoteID: "same-remote", AddressBookID: owner + "-book", Etag: owner}}
		results, err := accounts.PublishInboundContacts(t.Context(), snapshot, inputs)
		if err != nil || len(results) != 1 || !results[0].Created || results[0].ProfileID == "supplied-profile" {
			t.Fatal("publication", results, err)
		}
		ids[owner] = results[0].ProfileID
	}
	if ids["alice"] == ids["bob"] {
		t.Fatal("cross-owner profile identity")
	}
	for _, owner := range []string{"alice", "bob"} {
		err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
			contact, err := db.GetContact(t.Context(), owner, ids[owner])
			if err != nil {
				return err
			}
			if contact == nil || contact.Phone != owner {
				t.Fatal("wrong local contact", contact)
			}
			source, err := db.GetContactSourceByRemoteID(t.Context(), owner, "carddav", owners[owner].ID, "same-remote")
			if err != nil {
				return err
			}
			if source == nil || source.ContactID != ids[owner] || source.AddressBookID != owner+"-book" || source.Etag != owner {
				t.Fatal("wrong owned source", source)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"contact_profiles", "contact_cards", "contact_fields", "contact_identities", "contact_sync_operations"} {
		var count int
		if err := system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("central %s count=%d %v", table, count, err)
		}
	}
	if results, err := accounts.PublishInboundContacts(t.Context(), alice, []storage.InboundContact{{Contact: models.Contact{Email: "foreign@example.com"}, AddressBookID: "bob-book", RemoteID: "foreign"}}); !errors.Is(err, storage.ErrContactPublication) || results != nil {
		t.Fatal("foreign book accepted", results, err)
	}
	for _, snapshot := range []*AccountServiceSnapshot{nil, {}, {repository: accounts, owner: "alice", id: owners["bob"].ID}} {
		if results, err := accounts.PublishInboundContacts(t.Context(), snapshot, nil); err == nil || results != nil {
			t.Fatal("unbound/foreign snapshot accepted", results, err)
		}
	}
}

func TestUserContactPublicationRejectsStaleIdentityConfigurationAndCursors(t *testing.T) {
	for _, change := range []string{
		`UPDATE accounts SET provider_account_id='changed' WHERE id=?`,
		`UPDATE accounts SET encrypted_password=x'123456' WHERE id=?`,
		`UPDATE account_contact_sync_configs SET base_url='https://changed.test' WHERE account_id=?`,
		`UPDATE account_contact_sync_configs SET enabled=0 WHERE account_id=?`,
		`UPDATE account_contact_sync_configs SET last_sync_token='new-cursor' WHERE account_id=?`,
		`UPDATE account_contact_address_books SET last_sync_token='new-book-cursor' WHERE account_id=?`,
		`DELETE FROM account_contact_address_books WHERE account_id=?`,
	} {
		t.Run(change, func(t *testing.T) {
			_, _, accounts, owners := newServiceAccountFixture(t)
			id := owners["alice"].ID
			snapshot := serviceSnapshotFor(t, accounts, "alice", id)
			if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error { _, err := db.Write().Exec(change, id); return err }); err != nil {
				t.Fatal(err)
			}
			results, err := accounts.PublishInboundContacts(t.Context(), snapshot, []storage.InboundContact{{Contact: models.Contact{Email: "late@example.com"}, RemoteID: "late", AddressBookID: "alice-book"}})
			if !errors.Is(err, ErrAccountServicesChanged) || results != nil {
				t.Fatal("stale result published", results, err)
			}
			assertNoOwnedContactPublication(t, accounts, "alice")
		})
	}
}

func assertNoOwnedContactPublication(t *testing.T, accounts *UserAccountStore, owner string) {
	t.Helper()
	if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
		for _, table := range []string{"contact_profiles", "contact_cards", "contact_fields", "contact_identities", "contact_sync_operations", "contact_activity_events"} {
			var count int
			if err := db.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Fatalf("late %s count=%d", table, count)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserContactPublicationChecksGuardAfterWriterWait(t *testing.T) {
	for _, change := range []string{"configuration", "disabled-owner", "deleting-account"} {
		t.Run(change, func(t *testing.T) {
			system, routing, accounts, owners := newServiceAccountFixture(t)
			id := owners["alice"].ID
			snapshot := serviceSnapshotFor(t, accounts, "alice", id)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			err := accounts.WithAccountForUser(ctx, "alice", id, func(_ *AccountStore, db *storage.DB) error {
				tx, err := db.Write().BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				before := db.Write().Stats().WaitCount
				go func() {
					results, err := accounts.PublishInboundContacts(ctx, snapshot, []storage.InboundContact{{Contact: models.Contact{Email: "late@example.com"}, RemoteID: "late", AddressBookID: "alice-book"}})
					if len(results) > 0 && err != nil {
						done <- errors.New("failed publication returned candidates")
						return
					}
					done <- err
				}()
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for db.Write().Stats().WaitCount == before {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-tick.C:
					}
				}
				switch change {
				case "configuration":
					_, err = tx.ExecContext(ctx, `UPDATE account_contact_address_books SET last_sync_token='new-cursor' WHERE account_id=?`, id)
				case "disabled-owner":
					_, err = system.Write().ExecContext(ctx, `UPDATE users SET status='disabled' WHERE id='alice'`)
				case "deleting-account":
					err = routing.RequestAccountDeletion(ctx, "alice", id)
				}
				if err != nil {
					return err
				}
				return tx.Commit()
			})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("publication overtook changed lifecycle")
				}
				if change == "configuration" && !errors.Is(err, ErrAccountServicesChanged) {
					t.Fatal("wrong stale error", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if change == "disabled-owner" {
				if _, err := system.Write().Exec(`UPDATE users SET status='active' WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			}
			assertNoOwnedContactPublication(t, accounts, "alice")
		})
	}
}

func TestUserContactPublicationRollbackAllowsSameSnapshotRetry(t *testing.T) {
	_, _, accounts, owners := newServiceAccountFixture(t)
	id := owners["alice"].ID
	snapshot := serviceSnapshotFor(t, accounts, "alice", id)
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER fail_owned_card BEFORE INSERT ON contact_cards BEGIN SELECT RAISE(ABORT,'card failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	inputs := []storage.InboundContact{{Contact: models.Contact{Email: "retry@example.com"}, RemoteID: "retry", AddressBookID: "alice-book"}}
	if results, err := accounts.PublishInboundContacts(t.Context(), snapshot, inputs); err == nil || results != nil {
		t.Fatal("failed local commit returned candidates", results, err)
	}
	assertNoOwnedContactPublication(t, accounts, "alice")
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`DROP TRIGGER fail_owned_card`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	results, err := accounts.PublishInboundContacts(t.Context(), snapshot, inputs)
	if err != nil || len(results) != 1 || !results[0].Created {
		t.Fatal("same snapshot retry failed", results, err)
	}
	// A successful result does not alter endpoint/cursor identity, so other
	// pages from the same request can commit against the same copied snapshot.
	inputs[0].RemoteID = "next"
	inputs[0].Contact.Email = "next@example.com"
	results, err = accounts.PublishInboundContacts(t.Context(), snapshot, inputs)
	if err != nil || len(results) != 1 || !results[0].Created {
		t.Fatal("next provider page rejected", results, err)
	}
	if after := serviceSnapshotFor(t, accounts, "alice", id); after.contacts != snapshot.contacts {
		t.Fatal("import changed service configuration")
	}
}

func TestUserContactBookPublicationDeltaFullPruningAndSnapshotChain(t *testing.T) {
	_, _, accounts, owners := newServiceAccountFixture(t)
	id := owners["alice"].ID
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`INSERT INTO account_contact_address_books(user_id,account_id,id,url,name) VALUES('alice',?,'second-book','https://alice.test/second/','Second')`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := serviceSnapshotFor(t, accounts, "alice", id)
	inputs := []storage.InboundContact{
		{Contact: models.Contact{Email: "keep@example.com"}, RemoteID: "https://alice.test/book/keep.vcf", Etag: "keep"},
		{Contact: models.Contact{Email: "remove@example.com"}, RemoteID: "https://alice.test/book/remove.vcf", Etag: "remove"},
		{Contact: models.Contact{Email: "unusable@example.com"}, RemoteID: "https://alice.test/book/unusable.vcf", Etag: "unusable"},
	}
	results, next, err := accounts.PublishInboundContactBook(t.Context(), snapshot, "alice-book", storage.InboundContactBookPage{Contacts: inputs, SyncToken: "cursor-one"})
	if err != nil || len(results) != 3 || next == nil || next.contacts == snapshot.contacts {
		t.Fatal("first book checkpoint", results, next, err)
	}
	if _, _, err := accounts.PublishInboundContactBook(t.Context(), snapshot, "alice-book", storage.InboundContactBookPage{SyncToken: "stale"}); !errors.Is(err, ErrAccountServicesChanged) {
		t.Fatal("old snapshot overwrote cursor", err)
	}
	_, next, err = accounts.PublishInboundContactBook(t.Context(), next, "second-book", storage.InboundContactBookPage{Contacts: []storage.InboundContact{{Contact: models.Contact{Email: "isolated@example.com"}, RemoteID: "https://alice.test/second/isolated.vcf"}}, SyncToken: "second-cursor"})
	if err != nil {
		t.Fatal(err)
	}
	_, next, err = accounts.PublishInboundContactBook(t.Context(), next, "alice-book", storage.InboundContactBookPage{
		DeletedRemoteIDs: []string{inputs[1].RemoteID}, SyncToken: "cursor-two"})
	if err != nil {
		t.Fatal(err)
	}
	_, next, err = accounts.PublishInboundContactBook(t.Context(), next, "alice-book", storage.InboundContactBookPage{
		Full: true, SeenRemoteIDs: []string{inputs[2].RemoteID}, SyncToken: "cursor-three"})
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		for _, check := range []struct {
			remote string
			exists bool
		}{{inputs[0].RemoteID, false}, {inputs[1].RemoteID, false}, {inputs[2].RemoteID, true}, {"https://alice.test/second/isolated.vcf", true}} {
			source, err := db.GetContactSourceByRemoteID(t.Context(), "alice", "carddav", id, check.remote)
			if err != nil {
				return err
			}
			if (source != nil) != check.exists {
				t.Fatal("wrong book pruning", check.remote, source)
			}
		}
		// Remote deletion removes provider cards, retaining local canonical/manual history.
		for _, result := range results {
			profile, err := db.GetContactProfile(t.Context(), "alice", result.ProfileID)
			if err != nil {
				return err
			}
			if profile == nil || profile.IsDeleted {
				t.Fatal("provider deletion removed profile", profile)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, book := range next.ContactConfig().AddressBooks {
		want := "cursor-three"
		if book.ID == "second-book" {
			want = "second-cursor"
		}
		if book.LastSyncToken != want {
			t.Fatal("wrong refreshed book cursor", book)
		}
	}
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE account_contact_sync_configs SET username='replacement' WHERE account_id=?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := accounts.PublishInboundContactBook(t.Context(), next, "second-book", storage.InboundContactBookPage{SyncToken: "late"}); !errors.Is(err, ErrAccountServicesChanged) || fresh != nil {
		t.Fatal("next book accepted changed endpoint", fresh, err)
	}
}

func TestUserContactBookPublicationCursorFailureRollsBackFanoutImportsAndDeletion(t *testing.T) {
	_, _, accounts, owners := newServiceAccountFixture(t)
	id := owners["alice"].ID
	target, err := accounts.CreateAccount(t.Context(), "alice", secureAccountStoreTestRequest("target@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := serviceSnapshotFor(t, accounts, "alice", id)
	input := storage.InboundContact{Contact: models.Contact{Email: "sync@example.com", Phone: "old"}, RemoteID: "https://alice.test/book/sync.vcf", Etag: "old"}
	stale := storage.InboundContact{Contact: models.Contact{Email: "removed@example.com"}, RemoteID: "https://alice.test/book/removed.vcf"}
	results, next, err := accounts.PublishInboundContactBook(t.Context(), snapshot, "alice-book", storage.InboundContactBookPage{Contacts: []storage.InboundContact{input, stale}, SyncToken: "old-cursor"})
	if err != nil {
		t.Fatal(err)
	}
	profile := results[0].ProfileID
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		if _, err := db.Write().Exec(`UPDATE accounts SET provider='outlook' WHERE id=?`, target.ID); err != nil {
			return err
		}
		if err := db.InitializeContactCanonicalFields(t.Context(), "alice", profile, nil); err != nil {
			return err
		}
		if err := db.ReplaceContactSyncMemberships(t.Context(), "alice", profile, []string{"book:alice-book", "account:" + target.ID}); err != nil {
			return err
		}
		if err := db.SetContactProfileSyncEnabled(t.Context(), "alice", profile, true); err != nil {
			return err
		}
		_, err := db.Write().Exec(`CREATE TRIGGER fail_book_cursor BEFORE UPDATE ON account_contact_address_books BEGIN SELECT RAISE(ABORT,'cursor failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	input.Contact.Phone = "new"
	input.Etag = "new"
	page := storage.InboundContactBookPage{Contacts: []storage.InboundContact{input, {Contact: models.Contact{Email: "new@example.com"}, RemoteID: "https://alice.test/book/new.vcf"}}, DeletedRemoteIDs: []string{stale.RemoteID}, SyncToken: "new-cursor"}
	if results, fresh, err := accounts.PublishInboundContactBook(t.Context(), next, "alice-book", page); err == nil || results != nil || fresh != nil {
		t.Fatal("cursor failure returned candidates", results, fresh, err)
	}
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		contact, err := db.GetContact(t.Context(), "alice", profile)
		if err != nil {
			return err
		}
		if contact == nil || contact.Phone != "old" {
			t.Fatal("canonical update escaped", contact)
		}
		source, err := db.GetContactSourceByRemoteID(t.Context(), "alice", "carddav", id, input.RemoteID)
		if err != nil {
			return err
		}
		if source == nil || source.Etag != "old" {
			t.Fatal("etag escaped", source)
		}
		source, err = db.GetContactSourceByRemoteID(t.Context(), "alice", "carddav", id, stale.RemoteID)
		if err != nil {
			return err
		}
		if source == nil {
			t.Fatal("remote deletion escaped")
		}
		var profiles, queue, events int
		if err := db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM contact_profiles),(SELECT COUNT(*) FROM contact_sync_operations),(SELECT COUNT(*) FROM contact_activity_events)`).Scan(&profiles, &queue, &events); err != nil {
			return err
		}
		if profiles != 2 || queue != 0 || events != 0 {
			t.Fatal("partial import/fanout escaped", profiles, queue, events)
		}
		_, err = db.Write().Exec(`DROP TRIGGER fail_book_cursor`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	after := serviceSnapshotFor(t, accounts, "alice", id)
	if after.contacts != next.contacts {
		t.Fatal("failed checkpoint changed cursor")
	}
	results, after, err = accounts.PublishInboundContactBook(t.Context(), next, "alice-book", page)
	if err != nil || len(results) != 2 || !results[0].CanonicalChanged || results[0].FanoutOperationID == "" || !results[1].Created || after == nil {
		t.Fatal("book retry lost fanout", results, after, err)
	}
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		source, err := db.GetContactSourceByRemoteID(t.Context(), "alice", "carddav", id, stale.RemoteID)
		if source != nil {
			t.Fatal("retry failed to delete stale card")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserContactBookPublicationLegacyCursorAndScope(t *testing.T) {
	_, _, accounts, owners := newServiceAccountFixture(t)
	id := owners["alice"].ID
	if err := accounts.WithAccountForUser(t.Context(), "alice", id, func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`DELETE FROM account_contact_address_books WHERE account_id=?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := serviceSnapshotFor(t, accounts, "alice", id)
	if books := snapshot.ContactConfig().AddressBooks; len(books) != 1 || books[0].ID != "" {
		t.Fatal("legacy book fixture", books)
	}
	input := storage.InboundContact{Contact: models.Contact{Email: "legacy@example.com"}, RemoteID: "https://alice.test/book/legacy.vcf", AddressBookID: "supplied-foreign-book"}
	results, next, err := accounts.PublishInboundContactBook(t.Context(), snapshot, "", storage.InboundContactBookPage{Contacts: []storage.InboundContact{input}, SyncToken: "legacy-cursor"})
	if err != nil || len(results) != 1 || next.ContactConfig().LastSyncToken != "legacy-cursor" || next.ContactConfig().AddressBooks[0].LastSyncToken != "legacy-cursor" {
		t.Fatal("legacy checkpoint lost", results, next, err)
	}
	for _, page := range []storage.InboundContactBookPage{
		{Contacts: []storage.InboundContact{{Contact: models.Contact{Email: "foreign@example.com"}, RemoteID: "https://bob.test/book/foreign.vcf"}}},
		{DeletedRemoteIDs: []string{"https://alice.test/other/foreign.vcf"}},
		{SeenRemoteIDs: []string{"https://alice.test/book-other/foreign.vcf"}, Full: true},
		{Contacts: []storage.InboundContact{input}, DeletedRemoteIDs: []string{input.RemoteID}},
	} {
		if results, fresh, err := accounts.PublishInboundContactBook(t.Context(), next, "", page); !errors.Is(err, storage.ErrContactPublication) || results != nil || fresh != nil {
			t.Fatal("out-of-book/contradictory result accepted", results, fresh, err)
		}
	}
	if _, fresh, err := accounts.PublishInboundContactBook(t.Context(), next, "bob-book", storage.InboundContactBookPage{}); !errors.Is(err, storage.ErrContactPublication) || fresh != nil {
		t.Fatal("foreign selected book accepted", fresh, err)
	}
	if after := serviceSnapshotFor(t, accounts, "alice", id); after.contacts != next.contacts {
		t.Fatal("rejected book result changed cursor")
	}
}
