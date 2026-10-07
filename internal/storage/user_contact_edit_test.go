package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestUserContactUnifyCopiesCurrentFieldsAndRejectsInterveningEdit(t *testing.T) {
	db := newInboundContactFixture(t)
	id := seedContactSetup(t, db)
	if _, err := db.Write().Exec(`UPDATE contact_fields SET value='fresh-phone',normalized_value='fresh-phone' WHERE id='manual-phone'`); err != nil {
		t.Fatal(err)
	}
	snapshot, err := db.SnapshotUserContactUnify(t.Context(), "default", id, inboundTestGuard, nil)
	if err != nil || snapshot.Contact().Phone != "fresh-phone" || snapshot.Contact().Email != "manual@example.com" {
		t.Fatal("unify did not derive current values", err)
	}
	if _, err := db.ConfirmUserContactSetup(t.Context(), snapshot, nil, inboundTestGuard); !errors.Is(err, ErrContactEditChanged) {
		t.Fatal("unify snapshot authorized confirmation", err)
	}
	if _, err := db.Write().Exec(`UPDATE contact_fields SET value='newer-phone' WHERE id='manual-phone'`); err != nil {
		t.Fatal(err)
	}
	before := inboundPublicationState(t, db)
	if result, err := db.SaveUserContactEdit(t.Context(), snapshot, false, inboundTestGuard); !errors.Is(err, ErrContactEditChanged) || result.Contact.ID != "" || result.OperationID != "" {
		t.Fatal("unify overwrote an intervening edit", result, err)
	}
	if before != inboundPublicationState(t, db) {
		t.Fatal("rejected unification changed stored state")
	}
	snapshot, err = db.SnapshotUserContactUnify(t.Context(), "default", id, inboundTestGuard, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.SaveUserContactEdit(t.Context(), snapshot, false, inboundTestGuard)
	if err != nil || result.Contact.Phone != "newer-phone" || result.Contact.GoferSyncEnabled || result.OperationID != "" {
		t.Fatal("fresh unification changed sync behavior", result, err)
	}
	var raw, remote, etag string
	if err := db.Read().QueryRow(`SELECT raw_payload,remote_id,etag FROM contact_cards WHERE id='setup-remote'`).Scan(&raw, &remote, &etag); err != nil || raw != "retained-card" || remote != "remote-id" || etag != "old-tag" {
		t.Fatal("unify replaced provider source", raw, remote, etag, err)
	}
}

func TestUserContactEditRollbackAcrossProfileFieldsTargetsQueueAndActivity(t *testing.T) {
	for _, fault := range []string{"profile", "card", "field", "membership", "canonical", "queue", "activity"} {
		t.Run(fault, func(t *testing.T) {
			db, profile, _ := newContactSyncFixture(t)
			request := models.Contact{ID: profile, Name: "Edited", Email: "person@example.com", Phone: "222", PhoneLabel: "home",
				AdditionalEmails: []string{"alternate@example.com"}, AdditionalEmailLabels: []string{"work"}, GoferSyncEnabled: true,
				SaveTargets: []string{"local", "account:target", "book:dav-book"}}
			snapshot, err := db.SnapshotUserContactEdit(t.Context(), "default", request, inboundTestGuard, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Mutating public copies cannot change the retained proposed values.
			copy := snapshot.Contact()
			copy.AdditionalEmails[0] = "wrong@example.com"
			copy.SaveTargets[0] = "account:foreign"
			before := inboundPublicationState(t, db)
			table := map[string]string{"profile": "contact_profiles", "card": "contact_cards", "field": "contact_fields", "membership": "contact_sync_memberships", "canonical": "contact_fields", "queue": "contact_sync_operations", "activity": "contact_activity_events"}[fault]
			when := ""
			if fault == "canonical" {
				when = " WHEN NEW.source='canonical' AND NEW.kind='phone' AND NEW.value='222'"
			}
			if _, err := db.Write().Exec(`CREATE TRIGGER reject_user_edit BEFORE INSERT ON ` + table + when + ` BEGIN SELECT RAISE(ABORT,'synthetic editor failure'); END`); err != nil {
				t.Fatal(err)
			}
			var events []ContactActivityNotification
			db.SetContactActivityHook(func(event ContactActivityNotification) { events = append(events, event) })
			result, err := db.SaveUserContactEdit(t.Context(), snapshot, false, inboundTestGuard)
			if err == nil || result.Contact.ID != "" || result.OperationID != "" || len(events) != 0 {
				t.Fatal("failed commit escaped", result, err, events)
			}
			if after := inboundPublicationState(t, db); before != after {
				t.Fatal("partial editor save escaped rollback")
			}
			if _, err := db.Write().Exec(`DROP TRIGGER reject_user_edit`); err != nil {
				t.Fatal(err)
			}
			result, err = db.SaveUserContactEdit(t.Context(), snapshot, false, inboundTestGuard)
			if err != nil || result.Contact.ID != profile || result.OperationID == "" || result.Contact.Phone != "222" || result.Contact.Name != "Edited" || len(events) != 1 {
				t.Fatal("snapshot retry failed", result, err, events)
			}
			if !reflect.DeepEqual(result.Contact.AdditionalEmails, request.AdditionalEmails) {
				t.Fatal("public mutation affected save")
			}
			var payload string
			if err := db.Read().QueryRow(`SELECT payload_json FROM contact_sync_operations WHERE id=?`, result.OperationID).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var queued ContactSyncOperationPayload
			if err := json.Unmarshal([]byte(payload), &queued); err != nil {
				t.Fatal(err)
			}
			if queued.Contact.Phone != "222" || queued.Previous == nil || queued.Previous.Phone != "111" {
				t.Fatal("queue did not capture committed canonical values", queued)
			}
			var remote, etag string
			if err := db.Read().QueryRow(`SELECT remote_id,etag FROM contact_cards WHERE profile_id=? AND provider='gmail'`, profile).Scan(&remote, &etag); err != nil || remote != "people/original" || etag != "source-tag" {
				t.Fatal("editor replaced provider identity", remote, etag, err)
			}
			if result, err := db.SaveUserContactEdit(t.Context(), snapshot, false, inboundTestGuard); !errors.Is(err, ErrContactEditChanged) || result.Contact.ID != "" {
				t.Fatal("stale snapshot replay accepted", err)
			}
		})
	}
}

func TestUserContactEditRechecksProfileIdentityAndAcceptedTargets(t *testing.T) {
	for _, change := range []string{"field", "card", "membership", "deleted", "disabled-target", "removed-book"} {
		t.Run(change, func(t *testing.T) {
			db, profile, _ := newContactSyncFixture(t)
			snapshot, err := db.SnapshotUserContactEdit(t.Context(), "default", models.Contact{ID: profile, Email: "person@example.com", Phone: "stale", GoferSyncEnabled: true, SaveTargets: []string{"account:target", "book:dav-book"}}, inboundTestGuard, nil)
			if err != nil {
				t.Fatal(err)
			}
			query := map[string]string{
				"field":           `UPDATE contact_fields SET value='newer-phone' WHERE profile_id=? AND kind='phone' AND source='canonical'`,
				"card":            `UPDATE contact_cards SET etag='newer-etag' WHERE profile_id=?`,
				"membership":      `UPDATE contact_sync_memberships SET enabled=0 WHERE profile_id=?`,
				"deleted":         `UPDATE contact_profiles SET is_deleted=1 WHERE id=?`,
				"disabled-target": `INSERT INTO account_contact_sync_configs(account_id,user_id,provider,enabled) VALUES('target','default','outlook',0)`,
				"removed-book":    `DELETE FROM account_contact_address_books WHERE id='dav-book'`,
			}[change]
			var args []any
			if change != "disabled-target" && change != "removed-book" {
				args = []any{profile}
			}
			if _, err := db.Write().Exec(query, args...); err != nil {
				t.Fatal(err)
			}
			before := inboundPublicationState(t, db)
			if result, err := db.SaveUserContactEdit(t.Context(), snapshot, false, inboundTestGuard); !errors.Is(err, ErrContactEditChanged) || result.Contact.ID != "" {
				t.Fatal("late edit accepted", change, err)
			}
			if before != inboundPublicationState(t, db) {
				t.Fatal("rejected edit changed durable data")
			}
		})
	}
	db := newInboundContactFixture(t)
	request := models.Contact{Email: "new@example.com", Name: "New", GoferSyncEnabled: true, SaveTargets: []string{"account:target", "account:foreign", "book:foreign", "unknown"}}
	snapshot, err := db.SnapshotUserContactEdit(t.Context(), "default", request, inboundTestGuard, nil)
	if err != nil || !reflect.DeepEqual(snapshot.Contact().SaveTargets, []string{"account:target"}) {
		t.Fatal("invalid target allowlist", err)
	}
	if _, err := db.SaveContact(t.Context(), "default", models.Contact{Email: request.Email, Name: "Concurrent"}); err != nil {
		t.Fatal(err)
	}
	if result, err := db.SaveUserContactEdit(t.Context(), snapshot, false, inboundTestGuard); !errors.Is(err, ErrContactEditChanged) || result.Contact.ID != "" {
		t.Fatal("concurrent identity creation overwritten", err)
	}
	if _, err := db.SnapshotUserContactEdit(t.Context(), "default", models.Contact{ID: "missing", Email: request.Email}, inboundTestGuard, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("missing edit became creation", err)
	}
	if _, err := db.SnapshotUserContactEdit(t.Context(), "default", models.Contact{Email: ""}, inboundTestGuard, nil); !errors.Is(err, ErrContactEditInvalid) {
		t.Fatal("empty email accepted", err)
	}
}

func TestUserContactEditSetupAndCreationActivityAreAtomic(t *testing.T) {
	db := newInboundContactFixture(t)
	request := models.Contact{Name: "Created", Email: "created@example.com", Phone: "123", GoferSyncEnabled: true, SaveTargets: []string{"local", "account:target"}}
	snapshot, err := db.SnapshotUserContactEdit(t.Context(), "default", request, inboundTestGuard, nil)
	if err != nil {
		t.Fatal(err)
	}
	var events []ContactActivityNotification
	db.SetContactActivityHook(func(event ContactActivityNotification) { events = append(events, event) })
	if _, err := db.Write().Exec(`CREATE TRIGGER reject_editor_activity BEFORE INSERT ON contact_activity_events BEGIN SELECT RAISE(ABORT,'synthetic creation failure'); END`); err != nil {
		t.Fatal(err)
	}
	before := inboundPublicationState(t, db)
	if result, err := db.SaveUserContactEdit(t.Context(), snapshot, true, inboundTestGuard); err == nil || result.Contact.ID != "" || len(events) != 0 {
		t.Fatal("creation escaped failed activity", err)
	}
	if before != inboundPublicationState(t, db) {
		t.Fatal("creation partially committed")
	}
	if _, err := db.Write().Exec(`DROP TRIGGER reject_editor_activity`); err != nil {
		t.Fatal(err)
	}
	result, err := db.SaveUserContactEdit(t.Context(), snapshot, true, inboundTestGuard)
	if err != nil || result.Contact.ID == "" || !result.SetupDeferred || result.Contact.GoferSyncEnabled || result.OperationID != "" || len(events) != 1 || events[0].EventType != "manual_contact_added" {
		t.Fatal("setup creation", result, err, events)
	}
	if !reflect.DeepEqual(result.Contact.SaveTargets, request.SaveTargets) {
		t.Fatal("setup lost requested destinations")
	}
	var queued int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&queued); err != nil || queued != 0 {
		t.Fatal("setup prematurely queued provider work", queued, err)
	}
}
