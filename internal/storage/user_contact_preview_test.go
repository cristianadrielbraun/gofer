package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func seedContactPreview(t *testing.T, db *DB) (string, []ContactSetupSelection, []ContactSetupCandidateValue) {
	t.Helper()
	id := seedContactSetup(t, db)
	if err := db.ReplaceContactSyncMemberships(t.Context(), "default", id, []string{"local", "account:source", "account:target"}); err != nil {
		t.Fatal(err)
	}
	_, err := db.SaveContactProfile(t.Context(), "default", models.ContactProfile{ID: "donor-id", DisplayName: "Donor", PrimaryEmail: "donor@example.com",
		Cards:  []models.ContactCard{{ID: "donor-card", Kind: "provider", Provider: "gmail", AccountID: "source", RemoteID: "people/donor", Etag: "donor-tag", RawPayload: "original-raw", RawPayloadType: "application/json", SyncStatus: "pending", LastError: "preserved-error"}},
		Fields: []models.ContactField{{ID: "donor-email", CardID: "donor-card", Kind: "email", Value: "donor@example.com", Source: "synced:source"}, {ID: "donor-phone", Kind: "phone", Value: "999", Source: "synced:source"}}})
	if err != nil {
		t.Fatal(err)
	}
	selections := []ContactSetupSelection{{AccountID: "source", Provider: "gmail", RemoteID: "people/donor"}, {AccountID: "target", Provider: "outlook", RemoteID: "remote-id"}}
	values := []ContactSetupCandidateValue{{AccountID: "source", RemoteID: "people/donor", Etag: "google-new", Contact: models.Contact{Name: "Google selected", Email: "google@example.com", Phone: "333"}}, {AccountID: "target", RemoteID: "remote-id", Etag: "graph-new", Contact: models.Contact{Name: "Graph selected", Email: "graph@example.com", Phone: "444"}}}
	return id, selections, values
}

func TestUserContactSetupStoredCandidatesPageBoundaryAndLatestCard(t *testing.T) {
	db := newInboundContactFixture(t)
	id, _, _ := seedContactPreview(t, db)
	tx, err := db.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 1; i <= 205; i++ {
		profile := fmt.Sprintf("zz-%03d", i)
		if _, err := tx.Exec(`INSERT INTO contact_profiles(id,user_id,display_name,primary_email,is_deleted) VALUES(?,'default',?,?,?)`, profile, profile, profile+"@example.com", boolInt(i == 204)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO contact_cards(id,profile_id,user_id,kind,provider,account_id,remote_id,is_deleted) VALUES(?,?,'default','provider','gmail','source',?,?)`, "card-"+profile, profile, "people/"+profile, boolInt(i == 205)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO contact_cards(id,profile_id,user_id,kind,provider,account_id,remote_id,updated_at) VALUES('latest-card','zz-001','default','provider','gmail','source','people/latest','2099-01-01 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO contact_cards(id,profile_id,user_id,kind,provider,account_id,remote_id) VALUES('other-account','zz-002','default','provider','outlook','target','other')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	setup, err := db.SnapshotUserContactSetup(t.Context(), "default", id, inboundTestGuard, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, err := db.ReadUserContactSetupCandidates(t.Context(), setup, "source", "gmail", "zz-", inboundTestGuard)
	if err != nil || len(page) != 200 || page[0].Contact.ID != "zz-001" || page[0].RemoteID != "people/latest" || page[199].Contact.ID != "zz-200" {
		t.Fatal("first bounded/deduplicated page", len(page), err)
	}
	page[0].Contact.Email = "mutated@example.com"
	next, err := db.ReadUserContactSetupCandidates(t.Context(), setup, "source", "gmail", page[199].Contact.ID, inboundTestGuard)
	if err != nil || len(next) != 3 || next[0].Contact.ID != "zz-201" || next[2].Contact.ID != "zz-203" {
		t.Fatal("keyset omitted/duplicated rows or included deleted sources", len(next), err)
	}
	if _, err := db.ReadUserContactSetupCandidates(t.Context(), setup, "foreign", "gmail", "", inboundTestGuard); !errors.Is(err, ErrContactSetupInvalid) {
		t.Fatal("unapproved account read", err)
	}
	if _, err := db.Write().Exec(`UPDATE contact_profiles SET avatar_url='newer' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if stale, err := db.ReadUserContactSetupCandidates(t.Context(), setup, "source", "gmail", "zz-200", inboundTestGuard); !errors.Is(err, ErrContactEditChanged) || len(stale) != 0 {
		t.Fatal("stale setup kept browsing candidates", stale, err)
	}
}

func previewSnapshotFor(t *testing.T, db *DB, id string, selections []ContactSetupSelection) *ContactPreviewSnapshot {
	t.Helper()
	snapshot, err := db.SnapshotUserContactPreview(t.Context(), "default", id, inboundTestGuard, func(*sql.Tx, []string) ([]ContactSetupSelection, error) { return selections, nil })
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestUserContactPreviewAtomicAllSelectionsAndCardMetadata(t *testing.T) {
	for _, fault := range []string{"second-field", "second-card", "ignored-card"} {
		t.Run(fault, func(t *testing.T) {
			db := newInboundContactFixture(t)
			id, selections, values := seedContactPreview(t, db)
			snapshot := previewSnapshotFor(t, db, id, selections)
			before := inboundPublicationState(t, db)
			trigger := `BEFORE UPDATE ON contact_cards WHEN NEW.account_id='target'`
			failure := `SELECT RAISE(ABORT,'synthetic second candidate failure');`
			if fault == "second-field" {
				trigger = `BEFORE INSERT ON contact_fields WHEN NEW.source='synced:target'`
			}
			if fault == "ignored-card" {
				failure = `SELECT RAISE(IGNORE);`
			}
			if _, err := db.Write().Exec(`CREATE TRIGGER reject_preview ` + trigger + ` BEGIN ` + failure + ` END`); err != nil {
				t.Fatal(err)
			}
			if result, err := db.PublishUserContactPreview(t.Context(), snapshot, values, inboundTestGuard); err == nil || result.Contact.ID != "" || len(result.Fields) != 0 {
				t.Fatal("partial preview returned result", result, err)
			}
			if before != inboundPublicationState(t, db) {
				t.Fatal("later failure left earlier fields/card transfer")
			}
			if _, err := db.Write().Exec(`DROP TRIGGER reject_preview`); err != nil {
				t.Fatal(err)
			}
			result, err := db.PublishUserContactPreview(t.Context(), snapshot, values, inboundTestGuard)
			if err != nil || result.Contact.ID != id || result.Contact.GoferSyncEnabled {
				t.Fatal("retry preview", result, err)
			}
			var profile, raw, kind, status, lastError, etag string
			if err := db.Read().QueryRow(`SELECT profile_id,raw_payload,raw_payload_type,sync_status,last_error,etag FROM contact_cards WHERE id='donor-card'`).Scan(&profile, &raw, &kind, &status, &lastError, &etag); err != nil || profile != id || raw != "original-raw" || kind != "application/json" || status != "pending" || lastError != "preserved-error" || etag != "google-new" {
				t.Fatal("source card identity/metadata lost", profile, raw, kind, status, lastError, etag, err)
			}
			var card sql.NullString
			if err := db.Read().QueryRow(`SELECT card_id FROM contact_fields WHERE id='donor-email'`).Scan(&card); err != nil || card.Valid {
				t.Fatal("donor fields kept a card bound to another profile", card, err)
			}
			for _, value := range values {
				var phone string
				if err := db.Read().QueryRow(`SELECT value FROM contact_fields WHERE profile_id=? AND source=? AND kind='phone'`, id, "synced:"+value.AccountID).Scan(&phone); err != nil || phone != value.Contact.Phone {
					t.Fatal("wrong selected fields", value.AccountID, phone, err)
				}
			}
			var queued, canonical int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&queued); err != nil {
				t.Fatal(err)
			}
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_fields WHERE source='canonical'`).Scan(&canonical); err != nil {
				t.Fatal(err)
			}
			if queued != 0 || canonical != 0 {
				t.Fatal("preview enabled or queued sync prematurely", queued, canonical)
			}
		})
	}
}

func TestUserContactPreviewRejectsChangedDonorRecipientAndAbsentLink(t *testing.T) {
	for _, change := range []string{"donor-field", "donor-card", "recipient", "new-link", "wrong-remote"} {
		t.Run(change, func(t *testing.T) {
			db := newInboundContactFixture(t)
			id, selections, values := seedContactPreview(t, db)
			if change == "new-link" {
				selections[0].RemoteID = "people/absent"
				values[0].RemoteID = "people/absent"
			}
			snapshot := previewSnapshotFor(t, db, id, selections)
			queries := map[string]string{"donor-field": `UPDATE contact_fields SET value='newer' WHERE id='donor-phone'`, "donor-card": `UPDATE contact_cards SET etag='newer' WHERE id='donor-card'`, "recipient": `UPDATE contact_profiles SET avatar_url='newer-avatar' WHERE id='setup-id'`, "new-link": `UPDATE contact_cards SET remote_id='people/absent' WHERE id='donor-card'`}
			expected := ErrContactEditChanged
			if change == "wrong-remote" {
				values[1].RemoteID = "another-remote"
				expected = ErrContactSetupInvalid
			} else if _, err := db.Write().Exec(queries[change]); err != nil {
				t.Fatal(err)
			}
			before := inboundPublicationState(t, db)
			if result, err := db.PublishUserContactPreview(t.Context(), snapshot, values, inboundTestGuard); !errors.Is(err, expected) || result.Contact.ID != "" {
				t.Fatal("stale candidate published", change, err)
			}
			if before != inboundPublicationState(t, db) {
				t.Fatal("rejected candidate changed data")
			}
		})
	}
}

func TestUserContactPreviewStoredValuesArePrivateAndOwnerBound(t *testing.T) {
	db := newInboundContactFixture(t)
	id, selections, _ := seedContactPreview(t, db)
	selections = selections[:1]
	selections[0].RemoteID = ""
	selections[0].StoredProfileID = "donor-id"
	snapshot := previewSnapshotFor(t, db, id, selections)
	public := snapshot.Selections()
	public[0].RemoteID = "injected"
	copy := snapshot.StoredContact("source")
	copy.Email = "injected@example.com"
	result, err := db.PublishUserContactPreview(t.Context(), snapshot, []ContactSetupCandidateValue{{AccountID: "source", RemoteID: "people/donor", Etag: "ignored", Contact: models.Contact{Email: "injected@example.com", Phone: "injected"}}}, inboundTestGuard)
	if err != nil || result.Contact.ID != id {
		t.Fatal(result, err)
	}
	var email string
	if err := db.Read().QueryRow(`SELECT value FROM contact_fields WHERE profile_id=? AND source='synced:source' AND kind='email'`, id).Scan(&email); err != nil || email != "donor@example.com" {
		t.Fatal("stored preview used caller fields", email, err)
	}
	for _, selection := range []ContactSetupSelection{{AccountID: "foreign", Provider: "gmail", RemoteID: "people/x"}, {AccountID: "source", Provider: "outlook", RemoteID: "remote"}, {AccountID: "source", Provider: "gmail", StoredProfileID: "foreign-profile"}} {
		if _, err := db.SnapshotUserContactPreview(t.Context(), "default", id, inboundTestGuard, func(*sql.Tx, []string) ([]ContactSetupSelection, error) {
			return []ContactSetupSelection{selection}, nil
		}); err == nil {
			t.Fatal("foreign/unbound candidate accepted", selection)
		}
	}
}
