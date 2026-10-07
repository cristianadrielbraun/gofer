package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func newInboundContactFixture(t *testing.T) *DB {
	t.Helper()
	db := newContactsTestDB(t)
	_, err := db.Write().Exec(`
 INSERT INTO users(id,username,username_normalized) VALUES('other','other','other');
 INSERT INTO accounts(id,user_id,provider,email_address) VALUES
 ('source','default','gmail','source@example.com'),('target','default','outlook','target@example.com'),
 ('dav','default','imap','dav@example.com'),('foreign','other','gmail','foreign@example.com');
 INSERT INTO account_contact_sync_configs(account_id,user_id,provider,enabled,addressbook_url) VALUES('dav','default','carddav',1,'https://dav.test/book/');
 INSERT INTO account_contact_address_books(account_id,user_id,id,url) VALUES('dav','default','dav-book','https://dav.test/book/');`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func inboundTestGuard(*sql.Tx) error { return nil }

func publishTestContact(t *testing.T, db *DB, input InboundContact) InboundContactResult {
	t.Helper()
	results, err := db.PublishInboundContacts(t.Context(), "default", "source", "gmail", []InboundContact{input}, inboundTestGuard)
	if err != nil || len(results) != 1 {
		t.Fatalf("publication: %+v %v", results, err)
	}
	return results[0]
}

func enableInboundFanout(t *testing.T, db *DB, profile string) {
	t.Helper()
	if err := db.InitializeContactCanonicalFields(t.Context(), "default", profile, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceContactSyncMemberships(t.Context(), "default", profile, []string{"account:source", "account:target"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetContactProfileSyncEnabled(t.Context(), "default", profile, true); err != nil {
		t.Fatal(err)
	}
}

func inboundPublicationState(t *testing.T, db *DB) string {
	t.Helper()
	state := map[string][][]any{}
	for _, table := range []string{"contact_profiles", "contact_cards", "contact_fields", "contact_identities", "contact_sync_memberships", "contact_sync_operations", "contact_activity_events", "account_contact_sync_configs", "account_contact_address_books"} {
		rows, err := db.Read().Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			state[table] = append(state[table], values)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestInboundContactPublicationStableRemoteIdentityAndManualFields(t *testing.T) {
	db := newInboundContactFixture(t)
	profile, err := db.SaveContactProfile(t.Context(), "default", models.ContactProfile{DisplayName: "Manual name", PrimaryEmail: "old@example.com", AvatarURL: "manual-avatar",
		Fields: []models.ContactField{{Kind: "email", Value: "old@example.com", Source: "manual", IsPrimary: true}, {Kind: "phone", Value: "manual-phone", Source: "manual"}, {Kind: "avatar", Value: "manual-avatar", Source: "manual"}}})
	if err != nil {
		t.Fatal(err)
	}
	first := publishTestContact(t, db, InboundContact{Contact: models.Contact{ID: "untrusted-profile", Name: "Provider name", Email: "old@example.com", Phone: "provider-phone", AvatarURL: "provider-avatar"}, RemoteID: "people/1", Etag: "old"})
	if first.ProfileID != profile.ID || first.Created {
		t.Fatalf("manual identity changed: %+v", first)
	}
	second := publishTestContact(t, db, InboundContact{Contact: models.Contact{Name: "Changed provider name", Email: "new@example.com", Phone: "new-provider-phone", AvatarURL: "new-avatar"}, RemoteID: "people/1", Etag: "new"})
	if second.ProfileID != profile.ID || second.Created || second.CanonicalChanged || second.FanoutOperationID != "" {
		t.Fatalf("remote link lost: %+v", second)
	}
	after, err := db.GetContactProfile(t.Context(), "default", profile.ID)
	if err != nil || after.DisplayName != "Manual name" || after.AvatarURL != "manual-avatar" {
		t.Fatalf("manual profile overwritten: %+v %v", after, err)
	}
	var manualPhone, providerPhone string
	if err := db.Read().QueryRow(`SELECT value FROM contact_fields WHERE profile_id=? AND source='manual' AND kind='phone'`, profile.ID).Scan(&manualPhone); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT value FROM contact_fields WHERE profile_id=? AND source='synced:source' AND kind='phone'`, profile.ID).Scan(&providerPhone); err != nil {
		t.Fatal(err)
	}
	if manualPhone != "manual-phone" || providerPhone != "new-provider-phone" {
		t.Fatal("manual/source fields merged incorrectly", manualPhone, providerPhone)
	}
	source, err := db.GetContactSourceByRemoteID(t.Context(), "default", "gmail", "source", "people/1")
	if err != nil || source == nil || source.ContactID != profile.ID || source.Etag != "new" {
		t.Fatalf("source %+v %v", source, err)
	}
	linked, err := db.FindContactProfileByIdentity(t.Context(), "default", "email", "new@example.com")
	if err != nil || linked == nil || linked.ID != profile.ID {
		t.Fatalf("new email identity %+v %v", linked, err)
	}
}

func TestInboundContactPublicationAtomicFanoutAndNoEcho(t *testing.T) {
	db := newInboundContactFixture(t)
	input := InboundContact{Contact: models.Contact{Name: "Jane", Email: "jane@example.com", Phone: "old-phone"}, RemoteID: "people/1", Etag: "old"}
	first := publishTestContact(t, db, input)
	enableInboundFanout(t, db, first.ProfileID)
	input.Contact.Phone = "new-phone"
	input.Etag = "new"
	changed := publishTestContact(t, db, input)
	if changed.ProfileID != first.ProfileID || !changed.CanonicalChanged || changed.FanoutOperationID == "" {
		t.Fatalf("fanout missing %+v", changed)
	}
	var payload string
	if err := db.Read().QueryRow(`SELECT payload_json FROM contact_sync_operations WHERE id=?`, changed.FanoutOperationID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var queued ContactSyncOperationPayload
	if err := json.Unmarshal([]byte(payload), &queued); err != nil {
		t.Fatal(err)
	}
	if queued.ExcludedAccountID != "source" || queued.Contact.ID != first.ProfileID || !queued.Contact.GoferSyncEnabled {
		t.Fatalf("wrong queue %+v", queued)
	}
	current, err := db.GetContact(t.Context(), "default", first.ProfileID)
	if err != nil || current == nil || current.Phone != "new-phone" {
		t.Fatalf("canonical %+v %v", current, err)
	}
	repeated := publishTestContact(t, db, input)
	if repeated.CanonicalChanged || repeated.FanoutOperationID != "" {
		t.Fatalf("unchanged readback echoed %+v", repeated)
	}
	var count int
	for _, table := range []string{"contact_sync_operations", "contact_activity_events"} {
		if err := db.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
}

func TestInboundContactPublicationRollbackAndRetry(t *testing.T) {
	for _, table := range []string{"contact_profiles", "contact_fields", "contact_cards", "contact_sync_operations", "contact_activity_events"} {
		t.Run(table, func(t *testing.T) {
			db := newInboundContactFixture(t)
			input := InboundContact{Contact: models.Contact{Name: "Jane", Email: "jane@example.com", Phone: "old-phone"}, RemoteID: "people/1", Etag: "old"}
			first := publishTestContact(t, db, input)
			enableInboundFanout(t, db, first.ProfileID)
			before := inboundPublicationState(t, db)
			verb := "INSERT"
			if table == "contact_profiles" {
				verb = "UPDATE"
			}
			if _, err := db.Write().Exec("CREATE TRIGGER fail_inbound BEFORE " + verb + " ON " + table + " BEGIN SELECT RAISE(ABORT,'injected publication failure'); END"); err != nil {
				t.Fatal(err)
			}
			input.Contact.Phone = "new-phone"
			input.Etag = "new"
			results, err := db.PublishInboundContacts(t.Context(), "default", "source", "gmail", []InboundContact{input, {Contact: models.Contact{Email: "second@example.com"}, RemoteID: "people/2"}}, inboundTestGuard)
			if err == nil || results != nil {
				t.Fatal("failed transaction returned candidates", results, err)
			}
			if after := inboundPublicationState(t, db); after != before {
				t.Fatal("partial profile/source/queue escaped rollback")
			}
			if _, err := db.Write().Exec(`DROP TRIGGER fail_inbound`); err != nil {
				t.Fatal(err)
			}
			retry := publishTestContact(t, db, input)
			if !retry.CanonicalChanged || retry.FanoutOperationID == "" {
				t.Fatalf("retry lost intent %+v", retry)
			}
		})
	}
}

func TestInboundContactPublicationBatchRollbackAtLaterResult(t *testing.T) {
	db := newInboundContactFixture(t)
	before := inboundPublicationState(t, db)
	if _, err := db.Write().Exec(`CREATE TRIGGER fail_second BEFORE INSERT ON contact_cards WHEN NEW.remote_id='people/2' BEGIN SELECT RAISE(ABORT,'second card failure'); END`); err != nil {
		t.Fatal(err)
	}
	inputs := []InboundContact{{Contact: models.Contact{Email: "one@example.com"}, RemoteID: "people/1"}, {Contact: models.Contact{Email: "two@example.com"}, RemoteID: "people/2"}}
	results, err := db.PublishInboundContacts(t.Context(), "default", "source", "gmail", inputs, inboundTestGuard)
	if err == nil || results != nil || inboundPublicationState(t, db) != before {
		t.Fatal("earlier contact committed before later failure", results, err)
	}
	if _, err := db.Write().Exec(`DROP TRIGGER fail_second`); err != nil {
		t.Fatal(err)
	}
	results, err = db.PublishInboundContacts(t.Context(), "default", "source", "gmail", inputs, inboundTestGuard)
	if err != nil || len(results) != 2 || !results[0].Created || !results[1].Created {
		t.Fatal("batch retry failed", results, err)
	}
}

func TestInboundContactPublicationOwnerProviderBookAndLifecycleGuard(t *testing.T) {
	for _, tc := range []struct{ name, owner, account, provider, book, change string }{
		{"foreign-owner", "default", "foreign", "gmail", "", ""},
		{"wrong-provider", "default", "source", "outlook", "", ""},
		{"builtin-book", "default", "source", "gmail", "dav-book", ""},
		{"foreign-book", "default", "dav", "carddav", "wrong-book", ""},
		{"omitted-book", "default", "dav", "carddav", "", ""},
		{"disabled-contacts", "default", "source", "gmail", "", `INSERT INTO account_contact_sync_configs(account_id,user_id,provider,enabled) VALUES('source','default','gmail',0)`},
		{"deleting-account", "default", "source", "gmail", "", `UPDATE accounts SET is_deleting=1 WHERE id='source'`},
		{"disabled-owner", "default", "source", "gmail", "", `UPDATE users SET status='disabled' WHERE id='default'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newInboundContactFixture(t)
			if tc.change != "" {
				if _, err := db.Write().Exec(tc.change); err != nil {
					t.Fatal(err)
				}
			}
			before := inboundPublicationState(t, db)
			results, err := db.PublishInboundContacts(t.Context(), tc.owner, tc.account, tc.provider, []InboundContact{{Contact: models.Contact{Email: "new@example.com"}, RemoteID: "remote", AddressBookID: tc.book}}, inboundTestGuard)
			if !errors.Is(err, ErrContactPublication) || results != nil || inboundPublicationState(t, db) != before {
				t.Fatal("invalid destination published", results, err)
			}
		})
	}
	db := newInboundContactFixture(t)
	if _, err := db.PublishInboundContacts(t.Context(), "default", "source", "gmail", nil, nil); !errors.Is(err, ErrContactPublication) {
		t.Fatal("missing snapshot guard accepted", err)
	}
	sentinel := errors.New("stale snapshot")
	if _, err := db.PublishInboundContacts(t.Context(), "default", "source", "gmail", nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO contact_profiles(id,user_id) VALUES('guard-candidate','default')`); err != nil {
			return err
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatal("guard not applied", err)
	}
	var count int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_profiles`).Scan(&count); err != nil || count != 0 {
		t.Fatal("guard write escaped rollback", count, err)
	}
}

func TestInboundContactPublicationCardResurrectionAndLegacyMissingRemote(t *testing.T) {
	db := newInboundContactFixture(t)
	input := InboundContact{Contact: models.Contact{Email: "dav-person@example.com"}, AddressBookID: "dav-book", RemoteID: "https://dav.test/book/person.vcf", Etag: "first"}
	results, err := db.PublishInboundContacts(t.Context(), "default", "dav", "carddav", []InboundContact{input}, inboundTestGuard)
	if err != nil {
		t.Fatal(err)
	}
	var cardID string
	if err := db.Read().QueryRow(`SELECT id FROM contact_cards WHERE profile_id=?`, results[0].ProfileID).Scan(&cardID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`UPDATE contact_cards SET is_deleted=1 WHERE id=?`, cardID); err != nil {
		t.Fatal(err)
	}
	input.Etag = "second"
	results, err = db.PublishInboundContacts(t.Context(), "default", "dav", "carddav", []InboundContact{input}, inboundTestGuard)
	if err != nil || results[0].Created {
		t.Fatal("source resurrection failed", results, err)
	}
	var actualID, book, etag string
	var deleted int
	if err := db.Read().QueryRow(`SELECT id,address_book_id,etag,is_deleted FROM contact_cards WHERE profile_id=?`, results[0].ProfileID).Scan(&actualID, &book, &etag, &deleted); err != nil {
		t.Fatal(err)
	}
	if actualID != cardID || book != "dav-book" || etag != "second" || deleted != 0 {
		t.Fatal("resurrected card identity changed", actualID, book, etag, deleted)
	}
	legacy := publishTestContact(t, db, InboundContact{Contact: models.Contact{Email: "legacy@example.com"}})
	if legacy.ProfileID == "" {
		t.Fatal("legacy missing remote import dropped")
	}
	var count int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_cards WHERE profile_id=?`, legacy.ProfileID).Scan(&count); err != nil || count != 0 {
		t.Fatal("invented missing remote card", count, err)
	}
	empty := publishTestContact(t, db, InboundContact{Contact: models.Contact{Name: "No email"}, RemoteID: "people/no-email"})
	if empty.ProfileID != "" || empty.Created {
		t.Fatal("missing-email behavior changed", empty)
	}
	// A legacy DAV configuration without a book row still imports with an empty ID.
	if _, err := db.Write().Exec(`DELETE FROM account_contact_address_books WHERE account_id='dav'`); err != nil {
		t.Fatal(err)
	}
	input.AddressBookID = ""
	input.Contact.Email = "legacy-dav@example.com"
	input.RemoteID = "https://dav.test/book/legacy.vcf"
	if _, err := db.PublishInboundContacts(context.Background(), "default", "dav", "carddav", []InboundContact{input}, inboundTestGuard); err != nil {
		t.Fatal("legacy DAV book rejected", err)
	}
}

func TestInboundContactPublicationRejectAmbiguousRemoteLink(t *testing.T) {
	db := newInboundContactFixture(t)
	first := publishTestContact(t, db, InboundContact{Contact: models.Contact{Email: "first@example.com"}, RemoteID: "people/1"})
	second := publishTestContact(t, db, InboundContact{Contact: models.Contact{Email: "second@example.com"}, RemoteID: "people/2"})
	if _, err := db.Write().Exec(`UPDATE contact_cards SET remote_id='people/1' WHERE profile_id=?`, second.ProfileID); err != nil {
		t.Fatal(err)
	}
	before := inboundPublicationState(t, db)
	results, err := db.PublishInboundContacts(t.Context(), "default", "source", "gmail", []InboundContact{{Contact: models.Contact{Email: "first@example.com", Phone: "wrong"}, RemoteID: "people/1"}}, inboundTestGuard)
	if !errors.Is(err, ErrContactPublication) || results != nil || inboundPublicationState(t, db) != before {
		t.Fatal("ambiguous link published", results, err)
	}
	if first.ProfileID == second.ProfileID {
		t.Fatal("fixture profiles collapsed")
	}
}

func TestInboundContactPublicationFanoutUsesCurrentOwnedEnabledTargets(t *testing.T) {
	for _, change := range []string{
		`UPDATE contact_sync_memberships SET account_id='foreign' WHERE account_id='target'`,
		`UPDATE contact_sync_memberships SET enabled=0 WHERE account_id='target'`,
		`UPDATE contact_sync_memberships SET address_book_id='dav-book' WHERE account_id='target'`,
		`UPDATE accounts SET is_deleting=1 WHERE id='target'`,
		`INSERT INTO account_contact_sync_configs(account_id,user_id,provider,enabled) VALUES('target','default','outlook',0)`,
		`UPDATE contact_profiles SET sync_enabled=0`,
		`UPDATE contact_sync_memberships SET enabled=0 WHERE account_id='source'`,
	} {
		t.Run(change, func(t *testing.T) {
			db := newInboundContactFixture(t)
			input := InboundContact{Contact: models.Contact{Email: "person@example.com", Phone: "old"}, RemoteID: "people/1"}
			first := publishTestContact(t, db, input)
			enableInboundFanout(t, db, first.ProfileID)
			if _, err := db.Write().Exec(change); err != nil {
				t.Fatal(err)
			}
			input.Contact.Phone = "new"
			result := publishTestContact(t, db, input)
			if result.FanoutOperationID != "" {
				t.Fatal("unavailable target queued", result)
			}
			var count int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&count); err != nil || count != 0 {
				t.Fatal("unexpected queue", count, err)
			}
		})
	}
}

func TestInboundContactBookPublicationCommitFailureReturnsNoCandidates(t *testing.T) {
	db := newInboundContactFixture(t)
	before := inboundPublicationState(t, db)
	checkpointCalled := false
	results, err := db.PublishInboundContactBook(t.Context(), "default", "dav", models.ContactAddressBook{ID: "dav-book", URL: "https://dav.test/book/"}, InboundContactBookPage{
		Contacts: []InboundContact{{Contact: models.Contact{Email: "candidate@example.com"}, RemoteID: "https://dav.test/book/candidate.vcf"}}, SyncToken: "candidate-cursor",
	}, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `PRAGMA defer_foreign_keys=ON`)
		return err
	}, func(tx *sql.Tx) error {
		checkpointCalled = true
		// A deferred FK error occurs on COMMIT, after all writes and the
		// checkpoint callback have succeeded. No candidates may escape.
		_, err := tx.ExecContext(t.Context(), `INSERT INTO contact_cards(id,user_id,profile_id) VALUES('invalid-fk','default','missing-profile')`)
		return err
	})
	if err == nil || results != nil || !checkpointCalled {
		t.Fatal("commit failure returned candidates", results, err, checkpointCalled)
	}
	if after := inboundPublicationState(t, db); after != before {
		t.Fatal("failed commit retained cursor/import candidates")
	}
}
