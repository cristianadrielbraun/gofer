package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func seedContactSetup(t *testing.T, db *DB) string {
	t.Helper()
	profile, err := db.SaveContactProfile(t.Context(), "default", models.ContactProfile{ID: "setup-id", DisplayName: "Manual", PrimaryEmail: "manual@example.com", AvatarURL: "manual-avatar",
		Cards: []models.ContactCard{{ID: "setup-local", Kind: "local"}, {ID: "setup-remote", Kind: "provider", Provider: "outlook", AccountID: "target", RemoteID: "remote-id", Etag: "old-tag", RawPayload: "retained-card"}},
		Fields: []models.ContactField{
			{ID: "manual-email", Kind: "email", Value: "manual@example.com", Label: "home", IsPrimary: true, Source: "manual"},
			{ID: "remote-email", Kind: "email", Value: "remote@example.com", Label: "work", IsPrimary: true, Source: "synced:target"},
			{ID: "duplicate-email", Kind: "email", Value: "MANUAL@example.com", Label: "other", Source: "synced:target"},
			{ID: "manual-phone", Kind: "phone", Value: "111", Label: "home", IsPrimary: true, Source: "manual"},
			{ID: "remote-phone", Kind: "phone", Value: "222", Label: "work", IsPrimary: true, Source: "synced:target"},
			{ID: "manual-name", Kind: "name", Value: "Manual", Source: "manual"},
			{ID: "remote-name", Kind: "name", Value: "Provider", Source: "synced:target"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceContactSyncMemberships(t.Context(), "default", profile.ID, []string{"local", "account:target"}); err != nil {
		t.Fatal(err)
	}
	return profile.ID
}

func TestUserContactSetupConfirmationAtomicChoicesQueueAndActivity(t *testing.T) {
	for _, fault := range []string{"canonical", "enabled", "queue", "activity"} {
		t.Run(fault, func(t *testing.T) {
			db := newInboundContactFixture(t)
			id := seedContactSetup(t, db)
			snapshot, err := db.SnapshotUserContactSetup(t.Context(), "default", id, inboundTestGuard, nil)
			if err != nil {
				t.Fatal(err)
			}
			before := inboundPublicationState(t, db)
			var events []ContactActivityNotification
			db.SetContactActivityHook(func(event ContactActivityNotification) { events = append(events, event) })
			trigger := map[string]string{
				"canonical": `BEFORE INSERT ON contact_fields WHEN NEW.source='canonical'`,
				"enabled":   `BEFORE UPDATE ON contact_profiles WHEN NEW.sync_enabled=1`,
				"queue":     `BEFORE INSERT ON contact_sync_operations`,
				"activity":  `BEFORE INSERT ON contact_activity_events`,
			}[fault]
			if _, err := db.Write().Exec(`CREATE TRIGGER reject_setup ` + trigger + ` BEGIN SELECT RAISE(ABORT,'synthetic setup failure'); END`); err != nil {
				t.Fatal(err)
			}
			choices := map[string]string{"email": "remote-email", "phone": "remote-phone", "name": "remote-name"}
			if result, err := db.ConfirmUserContactSetup(t.Context(), snapshot, choices, inboundTestGuard); err == nil || result.Contact.ID != "" || result.OperationID != "" || len(events) != 0 {
				t.Fatal("confirmation escaped rollback", result, err, events)
			}
			if before != inboundPublicationState(t, db) {
				t.Fatal("failed setup changed fields, enabled state, cards or durable work")
			}
			if _, err := db.Write().Exec(`DROP TRIGGER reject_setup`); err != nil {
				t.Fatal(err)
			}
			result, err := db.ConfirmUserContactSetup(t.Context(), snapshot, choices, inboundTestGuard)
			if err != nil || result.Contact.ID != id || !result.Contact.GoferSyncEnabled || result.OperationID == "" || result.Contact.Email != "remote@example.com" || result.Contact.Phone != "222" || result.Contact.Name != "Provider" || result.Contact.AvatarURL != "manual-avatar" || len(events) != 1 {
				t.Fatal("retry did not commit chosen canonical values", result, err, events)
			}
			if result.Contact.EmailLabel != "work" || result.Contact.PhoneLabel != "work" || !reflect.DeepEqual(result.Contact.AdditionalEmails, []string{"manual@example.com"}) || !reflect.DeepEqual(result.Contact.AdditionalEmailLabels, []string{"home"}) || !reflect.DeepEqual(result.Contact.AdditionalPhones, []string{"111"}) || !reflect.DeepEqual(result.Contact.AdditionalPhoneLabels, []string{"home"}) {
				t.Fatal("setup lost primary/secondary labels or duplicate grouping", result.Contact)
			}
			var raw, remote, etag string
			if err := db.Read().QueryRow(`SELECT raw_payload,remote_id,etag FROM contact_cards WHERE id='setup-remote'`).Scan(&raw, &remote, &etag); err != nil || raw != "retained-card" || remote != "remote-id" || etag != "old-tag" {
				t.Fatal("setup replaced provider card identity", err)
			}
			var payload string
			if err := db.Read().QueryRow(`SELECT payload_json FROM contact_sync_operations WHERE id=?`, result.OperationID).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var queued ContactSyncOperationPayload
			if err := json.Unmarshal([]byte(payload), &queued); err != nil || queued.Contact.Email != result.Contact.Email || queued.Contact.Phone != "222" || queued.Previous != nil || !reflect.DeepEqual(queued.Contact.SaveTargets, []string{"account:target"}) {
				t.Fatal("queue differs from committed setup", queued, err)
			}
			if result, err := db.ConfirmUserContactSetup(t.Context(), snapshot, choices, inboundTestGuard); !errors.Is(err, ErrContactEditChanged) || result.Contact.ID != "" {
				t.Fatal("stale confirmation replay accepted", err)
			}
		})
	}
}

func TestUserContactSetupRejectsInvalidChoicesAndChangedBindings(t *testing.T) {
	for _, change := range []string{"invalid-id", "wrong-kind", "field", "card", "membership", "disabled-target", "deleted"} {
		t.Run(change, func(t *testing.T) {
			db := newInboundContactFixture(t)
			id := seedContactSetup(t, db)
			snapshot, err := db.SnapshotUserContactSetup(t.Context(), "default", id, inboundTestGuard, nil)
			if err != nil {
				t.Fatal(err)
			}
			choices := map[string]string{"email": "manual-email"}
			expected := ErrContactEditChanged
			queries := map[string]string{
				"field":           `UPDATE contact_fields SET value='newer@example.com' WHERE id='manual-email'`,
				"card":            `UPDATE contact_cards SET etag='newer' WHERE id='setup-remote'`,
				"membership":      `UPDATE contact_sync_memberships SET enabled=0 WHERE profile_id='setup-id'`,
				"disabled-target": `INSERT INTO account_contact_sync_configs(account_id,user_id,provider,enabled) VALUES('target','default','outlook',0)`,
				"deleted":         `UPDATE contact_profiles SET is_deleted=1 WHERE id='setup-id'`,
			}
			if change == "invalid-id" || change == "wrong-kind" {
				expected = ErrContactSetupInvalid
				choices["email"] = "foreign-field-id"
				if change == "wrong-kind" {
					choices["email"] = "manual-phone"
				}
			} else if _, err := db.Write().Exec(queries[change]); err != nil {
				t.Fatal(err)
			}
			before := inboundPublicationState(t, db)
			if result, err := db.ConfirmUserContactSetup(t.Context(), snapshot, choices, inboundTestGuard); !errors.Is(err, expected) || result.Contact.ID != "" || result.OperationID != "" {
				t.Fatal("invalid or stale setup accepted", change, err)
			}
			if before != inboundPublicationState(t, db) {
				t.Fatal("rejected setup changed durable data")
			}
		})
	}
	db := newInboundContactFixture(t)
	id := seedContactSetup(t, db)
	setup, err := db.SnapshotUserContactSetup(t.Context(), "default", id, inboundTestGuard, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveUserContactEdit(t.Context(), setup, false, inboundTestGuard); !errors.Is(err, ErrContactEditChanged) {
		t.Fatal("setup snapshot used as editor proposal", err)
	}
	if _, err := db.SnapshotUserContactSetup(t.Context(), "other", id, inboundTestGuard, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign profile accepted", err)
	}
	edit, err := db.SnapshotUserContactEdit(t.Context(), "default", models.Contact{ID: id, Email: "manual@example.com", SaveTargets: []string{"local"}}, inboundTestGuard, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConfirmUserContactSetup(t.Context(), edit, nil, inboundTestGuard); !errors.Is(err, ErrContactEditChanged) {
		t.Fatal("editor proposal authorized setup confirmation", err)
	}
	if _, err := db.Write().Exec(`INSERT INTO account_contact_sync_configs(account_id,user_id,provider,enabled) VALUES('target','default','outlook',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SnapshotUserContactSetup(t.Context(), "default", id, inboundTestGuard, nil); !errors.Is(err, ErrContactEditChanged) {
		t.Fatal("setup silently discarded unavailable destination", err)
	}
}

func TestUserContactSetupLocalOnlyNeedsNoQueue(t *testing.T) {
	db := newInboundContactFixture(t)
	contact, err := db.SaveContact(t.Context(), "default", models.Contact{Email: "local@example.com", Phone: "123", SaveTargets: []string{"local"}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := db.SnapshotUserContactSetup(t.Context(), "default", contact.ID, inboundTestGuard, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.ConfirmUserContactSetup(t.Context(), snapshot, nil, inboundTestGuard)
	if err != nil || !result.Contact.GoferSyncEnabled || result.Contact.Phone != "123" || result.OperationID != "" {
		t.Fatal("local confirmation", result, err)
	}
}
