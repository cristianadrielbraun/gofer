package storage

import (
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func seedContactDelete(t *testing.T, db *DB, manual bool) *ContactDeleteSnapshot {
	t.Helper()
	fields := []models.ContactField{{ID: "delete-google-phone", CardID: "delete-google-one", Kind: "phone", Value: "111", Source: "synced:source"}, {ID: "delete-graph-email", CardID: "delete-graph", Kind: "email", Value: "delete@example.com", Source: "synced:target"}}
	if manual {
		fields = append(fields, models.ContactField{ID: "delete-manual-email", Kind: "email", Value: "delete@example.com", Source: "manual"})
	}
	_, err := db.SaveContactProfile(t.Context(), "default", models.ContactProfile{ID: "delete-profile", PrimaryEmail: "delete@example.com", DisplayName: "Delete me", SyncEnabled: true, Fields: fields, Cards: []models.ContactCard{{ID: "delete-local", Kind: "local"}, {ID: "delete-google-one", Kind: "provider", Provider: "gmail", AccountID: "source", RemoteID: "people/one", Etag: "one", RawPayload: "raw-one"}, {ID: "delete-google-two", Kind: "provider", Provider: "gmail", AccountID: "source", RemoteID: "people/two", Etag: "two", RawPayload: "raw-two"}, {ID: "delete-graph", Kind: "provider", Provider: "outlook", AccountID: "target", RemoteID: "graph-contact", Etag: "graph"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceContactSyncMemberships(t.Context(), "default", "delete-profile", []string{"local", "account:source", "account:target"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertObservedContact(t.Context(), "default", "Delete me", "delete@example.com", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"pending", "running"} {
		id, err := db.EnqueueContactSyncOperation(t.Context(), "default", models.Contact{ID: "delete-profile", Email: "delete@example.com", GoferSyncEnabled: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Write().Exec(`UPDATE contact_sync_operations SET status=?,locked_at=CURRENT_TIMESTAMP WHERE id=?`, status, id); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := db.SnapshotUserContactDelete(t.Context(), "default", "delete-profile", inboundTestGuard, nil)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func acknowledgeDeleteSources(t *testing.T, db *DB, s *ContactDeleteSnapshot) *ContactDeleteSnapshot {
	t.Helper()
	for _, card := range s.Sources() {
		var err error
		s, err = db.AcknowledgeUserContactDeleteSource(t.Context(), s, card.ID, inboundTestGuard)
		if err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestUserContactDeleteSourceAcknowledgementIsExactAtomicAndImmutable(t *testing.T) {
	for _, ignored := range []bool{false, true} {
		t.Run(map[bool]string{false: "aborted", true: "ignored"}[ignored], func(t *testing.T) {
			db := newInboundContactFixture(t)
			snapshot := seedContactDelete(t, db, false)
			public := snapshot.Sources()
			public[0].ID = "delete-graph"
			public[0].RemoteID = "injected"
			if snapshot.Sources()[0].ID != "delete-google-one" {
				t.Fatal("public source mutated retained identity")
			}
			before := inboundPublicationState(t, db)
			fault := `SELECT RAISE(ABORT,'synthetic card delete failure');`
			if ignored {
				fault = `SELECT RAISE(IGNORE);`
			}
			if _, err := db.Write().Exec(`CREATE TRIGGER reject_delete_source BEFORE DELETE ON contact_cards WHEN OLD.id='delete-google-one' BEGIN ` + fault + ` END`); err != nil {
				t.Fatal(err)
			}
			if next, err := db.AcknowledgeUserContactDeleteSource(t.Context(), snapshot, "delete-google-one", inboundTestGuard); err == nil || next != nil {
				t.Fatal("failed acknowledgement escaped", next, err)
			}
			if before != inboundPublicationState(t, db) {
				t.Fatal("failed source acknowledgement changed data")
			}
			if _, err := db.Write().Exec(`DROP TRIGGER reject_delete_source`); err != nil {
				t.Fatal(err)
			}
			next, err := db.AcknowledgeUserContactDeleteSource(t.Context(), snapshot, "delete-google-one", inboundTestGuard)
			if err != nil || len(next.Sources()) != 2 {
				t.Fatal("source retry", next, err)
			}
			if err := db.ValidateUserContactDelete(t.Context(), snapshot, inboundTestGuard); !errors.Is(err, ErrContactEditChanged) {
				t.Fatal("old snapshot accepted after ack", err)
			}
			if err := db.ValidateUserContactDelete(t.Context(), next, inboundTestGuard); err != nil {
				t.Fatal(err)
			}
			if _, err := db.FinishUserContactDelete(t.Context(), next, inboundTestGuard); !errors.Is(err, ErrContactEditChanged) {
				t.Fatal("unacknowledged sources finalized", err)
			}
			var remaining, fields int
			var raw string
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_cards WHERE account_id='source'`).Scan(&remaining); err != nil || remaining != 1 {
				t.Fatal("ack deleted entire account's cards", remaining, err)
			}
			if err := db.Read().QueryRow(`SELECT raw_payload FROM contact_cards WHERE id='delete-google-two'`).Scan(&raw); err != nil || raw != "raw-two" {
				t.Fatal("unattempted metadata lost", raw, err)
			}
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_fields WHERE id='delete-google-phone'`).Scan(&fields); err != nil || fields != 0 {
				t.Fatal("card field cascade did not occur", fields, err)
			}
			if _, err := db.Write().Exec(`UPDATE contact_profiles SET avatar_url='manual edit' WHERE id='delete-profile'`); err != nil {
				t.Fatal(err)
			}
			before = inboundPublicationState(t, db)
			if next, err := db.AcknowledgeUserContactDeleteSource(t.Context(), next, "delete-google-two", inboundTestGuard); !errors.Is(err, ErrContactEditChanged) || next != nil {
				t.Fatal("ack adopted intervening edit", err)
			}
			if before != inboundPublicationState(t, db) {
				t.Fatal("stale ack changed data")
			}
		})
	}
}

func TestUserContactDeleteFinalizationAtomicSuppressionQueueAndActivity(t *testing.T) {
	for _, fault := range []string{"tombstone", "ignored-tombstone", "suppression", "queue", "activity"} {
		t.Run(fault, func(t *testing.T) {
			db := newInboundContactFixture(t)
			snapshot := acknowledgeDeleteSources(t, db, seedContactDelete(t, db, false))
			before := inboundPublicationState(t, db)
			events := 0
			db.SetContactActivityHook(func(event ContactActivityNotification) {
				if event.EventType == "contact_deleted" {
					events++
				}
			})
			trigger := map[string]string{"tombstone": `BEFORE UPDATE ON contact_profiles WHEN NEW.is_deleted=1`, "ignored-tombstone": `BEFORE UPDATE ON contact_profiles WHEN NEW.is_deleted=1`, "suppression": `BEFORE UPDATE ON contact_observations WHEN NEW.is_suppressed=1`, "queue": `BEFORE UPDATE ON contact_sync_operations`, "activity": `BEFORE INSERT ON contact_activity_events WHEN NEW.event_type='contact_deleted'`}[fault]
			fail := `SELECT RAISE(ABORT,'synthetic final delete failure');`
			if fault == "ignored-tombstone" {
				fail = `SELECT RAISE(IGNORE);`
			}
			if _, err := db.Write().Exec(`CREATE TRIGGER reject_final_delete ` + trigger + ` BEGIN ` + fail + ` END`); err != nil {
				t.Fatal(err)
			}
			if id, err := db.FinishUserContactDelete(t.Context(), snapshot, inboundTestGuard); err == nil || id != "" || events != 0 {
				t.Fatal("failed deletion returned success/event", id, err, events)
			}
			if before != inboundPublicationState(t, db) {
				t.Fatal("failed deletion left partial queue/tombstone/activity")
			}
			var suppressed int
			if err := db.Read().QueryRow(`SELECT is_suppressed FROM contact_observations WHERE profile_id='delete-profile'`).Scan(&suppressed); err != nil || suppressed != 0 {
				t.Fatal("failed deletion suppressed observation", suppressed, err)
			}
			if _, err := db.Write().Exec(`DROP TRIGGER reject_final_delete`); err != nil {
				t.Fatal(err)
			}
			if id, err := db.FinishUserContactDelete(t.Context(), snapshot, inboundTestGuard); err != nil || id != "delete-profile" || events != 1 {
				t.Fatal("final delete retry", id, err, events)
			}
			var deleted, done int
			if err := db.Read().QueryRow(`SELECT is_deleted FROM contact_profiles WHERE id='delete-profile'`).Scan(&deleted); err != nil || deleted != 1 {
				t.Fatal(deleted, err)
			}
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations WHERE status='done' AND locked_at IS NULL`).Scan(&done); err != nil || done != 2 {
				t.Fatal("queued/running work not canceled", done, err)
			}
			if err := db.Read().QueryRow(`SELECT is_suppressed FROM contact_observations WHERE profile_id='delete-profile'`).Scan(&suppressed); err != nil || suppressed != 1 {
				t.Fatal("missing suppression", suppressed, err)
			}
		})
	}
}

func TestUserContactDeleteManualPolicyAndMissingOwnerBoundaries(t *testing.T) {
	db := newInboundContactFixture(t)
	snapshot := acknowledgeDeleteSources(t, db, seedContactDelete(t, db, true))
	if _, err := db.SnapshotUserContactDelete(t.Context(), "other", "delete-profile", inboundTestGuard, nil); err == nil {
		t.Fatal("foreign profile selected")
	}
	if _, err := db.Write().Exec(`INSERT INTO app_settings(user_id,key,value) VALUES('default','ui_settings','{"contacts_prevent_recreate_deleted":"false"}')`); err != nil {
		t.Fatal(err)
	}
	if id, err := db.FinishUserContactDelete(t.Context(), snapshot, inboundTestGuard); !errors.Is(err, ErrContactEditChanged) || id != "" {
		t.Fatal("changed deletion policy silently accepted", id, err)
	}
	snapshot, err := db.SnapshotUserContactDelete(t.Context(), "default", "delete-profile", inboundTestGuard, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := db.FinishUserContactDelete(t.Context(), snapshot, inboundTestGuard); err != nil || id == "" {
		t.Fatal(id, err)
	}
	var suppressed int
	if err := db.Read().QueryRow(`SELECT is_suppressed FROM contact_observations WHERE profile_id='delete-profile'`).Scan(&suppressed); err != nil || suppressed != 0 {
		t.Fatal("manual contact unexpectedly suppressed", suppressed, err)
	}
	if _, err := db.FinishUserContactDelete(t.Context(), nil, inboundTestGuard); !errors.Is(err, ErrContactEditChanged) {
		t.Fatal("empty delete snapshot accepted")
	}
}

func TestUserContactDeleteManualAndDisabledSuppressionPolicyAreIndependent(t *testing.T) {
	for _, manual := range []bool{true, false} {
		t.Run(map[bool]string{true: "manual", false: "policy-off"}[manual], func(t *testing.T) {
			db := newInboundContactFixture(t)
			snapshot := seedContactDelete(t, db, manual)
			if !manual {
				if err := db.MergeUISettings(t.Context(), "default", map[string]string{"contacts_prevent_recreate_deleted": "false"}); err != nil {
					t.Fatal(err)
				}
				var err error
				snapshot, err = db.SnapshotUserContactDelete(t.Context(), "default", "delete-profile", inboundTestGuard, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			snapshot = acknowledgeDeleteSources(t, db, snapshot)
			if _, err := db.FinishUserContactDelete(t.Context(), snapshot, inboundTestGuard); err != nil {
				t.Fatal(err)
			}
			var suppressed int
			if err := db.Read().QueryRow(`SELECT is_suppressed FROM contact_observations WHERE profile_id='delete-profile'`).Scan(&suppressed); err != nil || suppressed != 0 {
				t.Fatal("unexpected deletion suppression", suppressed, err)
			}
		})
	}
}
