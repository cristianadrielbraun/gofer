package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func seedObservedContactDeletion(t *testing.T, db *DB, owner string) {
	t.Helper()
	for _, id := range []string{"observed-one", "observed-two", "manual-contact", "already-suppressed"} {
		source := "observed"
		if id == "manual-contact" {
			source = "manual"
		}
		email := owner + "-" + id + "@example.com"
		if _, err := db.SaveContactProfile(t.Context(), owner, models.ContactProfile{ID: owner + "-" + id, PrimaryEmail: email, Origin: "observed", Fields: []models.ContactField{{Kind: "email", Value: email, Source: source}}, Cards: []models.ContactCard{{Kind: "local"}}}); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertObservedContact(t.Context(), owner, id, email, time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := db.EnqueueContactSyncOperation(t.Context(), owner, models.Contact{ID: owner + "-" + id, Email: email}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Write().Exec(`UPDATE contact_observations SET is_suppressed=1,suppress_auto_create=1 WHERE profile_id=?`, owner+"-already-suppressed"); err != nil {
		t.Fatal(err)
	}
}

func TestUserContactSuppressionBulkAtomicPolicyQueueActivityAndManualExclusion(t *testing.T) {
	for _, prevent := range []bool{true, false} {
		t.Run(map[bool]string{true: "suppress", false: "forget"}[prevent], func(t *testing.T) {
			db := newInboundContactFixture(t)
			seedObservedContactDeletion(t, db, "default")
			seedObservedContactDeletion(t, db, "other")
			if !prevent {
				if err := db.MergeUISettings(t.Context(), "default", map[string]string{"contacts_prevent_recreate_deleted": "false"}); err != nil {
					t.Fatal(err)
				}
			}
			events := 0
			db.SetContactActivityHook(func(e ContactActivityNotification) {
				if e.EventType == "observed_contacts_deleted" {
					events++
					if e.Count != 2 {
						t.Error("wrong deleted event count", e.Count)
					}
				}
			})
			if count, err := db.DeleteUserObservedContacts(t.Context(), "default", inboundTestGuard); err != nil || count != 2 || events != 1 {
				t.Fatal("bulk delete", count, err, events)
			}
			var deleted, done, foreign, suppressed, observations int
			for query, dest := range map[string]*int{
				`SELECT COUNT(*) FROM contact_profiles WHERE user_id='default' AND is_deleted=1`:         &deleted,
				`SELECT COUNT(*) FROM contact_sync_operations WHERE user_id='default' AND status='done'`: &done,
				`SELECT COUNT(*) FROM contact_profiles WHERE user_id='other' AND is_deleted=1`:           &foreign,
				`SELECT COUNT(*) FROM contact_observations WHERE user_id='default' AND is_suppressed=1`:  &suppressed,
				`SELECT COUNT(*) FROM contact_observations WHERE user_id='default'`:                      &observations,
			} {
				if err := db.Read().QueryRow(query).Scan(dest); err != nil {
					t.Fatal(err)
				}
			}
			wantSuppressed, wantObservations := 3, 4
			if !prevent {
				wantSuppressed, wantObservations = 1, 2
			}
			if deleted != 2 || done != 2 || foreign != 0 || suppressed != wantSuppressed || observations != wantObservations {
				t.Fatal("bulk crossed manual,owner,policy,queue", deleted, done, foreign, suppressed, observations)
			}
			if count, err := db.DeleteUserObservedContacts(t.Context(), "default", inboundTestGuard); err != nil || count != 0 || events != 1 {
				t.Fatal("empty bulk replay", count, err, events)
			}
		})
	}
}

func TestUserContactSuppressionBulkRollbackAndGuard(t *testing.T) {
	for _, fault := range []string{"second-profile", "suppression", "queue", "activity", "owner"} {
		t.Run(fault, func(t *testing.T) {
			db := newInboundContactFixture(t)
			seedObservedContactDeletion(t, db, "default")
			before := inboundPublicationState(t, db)
			var observationState string
			if err := db.Read().QueryRow(`SELECT GROUP_CONCAT(id||':'||is_suppressed) FROM (SELECT id,is_suppressed FROM contact_observations ORDER BY id)`).Scan(&observationState); err != nil {
				t.Fatal(err)
			}
			events := 0
			db.SetContactActivityHook(func(ContactActivityNotification) { events++ })
			guard := inboundTestGuard
			if fault == "owner" {
				guard = func(*sql.Tx) error { return ErrUserStoreOwner }
			} else {
				trigger := map[string]string{"second-profile": `BEFORE UPDATE ON contact_profiles WHEN NEW.id='default-observed-two' AND NEW.is_deleted=1`, "suppression": `BEFORE UPDATE ON contact_observations WHEN NEW.is_suppressed=1`, "queue": `BEFORE UPDATE ON contact_sync_operations`, "activity": `BEFORE INSERT ON contact_activity_events WHEN NEW.event_type='observed_contacts_deleted'`}[fault]
				if _, err := db.Write().Exec(`CREATE TRIGGER reject_observed_delete ` + trigger + ` BEGIN SELECT RAISE(ABORT,'synthetic observed delete failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if count, err := db.DeleteUserObservedContacts(t.Context(), "default", guard); err == nil || count != 0 || events != 0 {
				t.Fatal("failed bulk claimed success", count, err, events)
			}
			if before != inboundPublicationState(t, db) {
				t.Fatal("partial bulk tombstone,queue,activity")
			}
			var after string
			if err := db.Read().QueryRow(`SELECT GROUP_CONCAT(id||':'||is_suppressed) FROM (SELECT id,is_suppressed FROM contact_observations ORDER BY id)`).Scan(&after); err != nil || after != observationState {
				t.Fatal("partial suppression", after, err)
			}
		})
	}
}

func TestUserContactSuppressionClearOwnedMissingAndRead(t *testing.T) {
	db := newInboundContactFixture(t)
	seedObservedContactDeletion(t, db, "default")
	seedObservedContactDeletion(t, db, "other")
	if _, err := db.DeleteUserObservedContacts(t.Context(), "default", inboundTestGuard); err != nil {
		t.Fatal(err)
	}
	contacts, count, err := db.ReadUserSuppressedContacts(t.Context(), "default", inboundTestGuard)
	if err != nil || len(contacts) != 3 || count != 3 {
		t.Fatal("suppression snapshot", len(contacts), count, err)
	}
	foreign := "other-already-suppressed"
	for _, id := range []string{"", foreign, "missing"} {
		if n, err := db.ClearUserContactSuppression(t.Context(), "default", &id, inboundTestGuard); !errors.Is(err, sql.ErrNoRows) || n != 0 {
			t.Fatal("foreign/missing suppression cleared", n, err)
		}
	}
	id := "default-observed-one"
	if count, err := db.ClearUserContactSuppression(t.Context(), "default", &id, inboundTestGuard); err != nil || count != 1 {
		t.Fatal("clear one", count, err)
	}
	if count, err := db.ClearUserContactSuppression(t.Context(), "default", nil, inboundTestGuard); err != nil || count != 2 {
		t.Fatal("clear all", count, err)
	}
	var other int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_observations WHERE user_id='other' AND is_suppressed=1`).Scan(&other); err != nil || other != 1 {
		t.Fatal("clear crossed owner", other, err)
	}
}

func TestUserContactSuppressionReadCapsRowsAndKeepsFullTotal(t *testing.T) {
	db := newInboundContactFixture(t)
	tx, err := db.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 205; i++ {
		id := fmt.Sprintf("suppressed-%03d", i)
		if _, err := tx.Exec(`INSERT INTO contact_observations(id,user_id,email,normalized_email,is_suppressed,suppress_auto_create) VALUES(?,'default',?,?,1,1)`, id, id+"@example.com", id+"@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	contacts, count, err := db.ReadUserSuppressedContacts(t.Context(), "default", inboundTestGuard)
	if err != nil || len(contacts) != 200 || count != 205 {
		t.Fatal("bounded suppression list lost total", len(contacts), count, err)
	}
}
