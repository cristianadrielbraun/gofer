package storage

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestUserCalendarResponseReservationMigrationPreservesLegacyUncertainty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "response-migration.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// A fresh schema must advertise its actual shape before eviction/reopen.
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != CurrentSchemaVersion {
		t.Fatal("fresh schema marker", version, err)
	}
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('owner','owner','owner');
INSERT INTO accounts(id,user_id,provider,email_address) VALUES('account','owner','gmail','owner@example.com');
INSERT INTO calendar_sources(id,user_id,account_id,provider,remote_id,is_selected) VALUES('source','owner','account','gmail','primary',1);
INSERT INTO calendar_response_requests(user_id,source_id,remote_id,version,response,created_at) VALUES('owner','source','remote','old-version','accepted','2024-10-02 10:00:00');
ALTER TABLE calendar_response_requests DROP COLUMN claim_id;
DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(106)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV106ToV107(db.Write()); err != nil {
			t.Fatal("migration replay", err)
		}
	}
	var owner, source, remote, oldVersion, response, created, claim string
	if err := db.Read().QueryRow(`SELECT user_id,source_id,remote_id,version,response,created_at,claim_id FROM calendar_response_requests`).Scan(&owner, &source, &remote, &oldVersion, &response, &created, &claim); err != nil {
		t.Fatal(err)
	}
	if owner != "owner" || source != "source" || remote != "remote" || oldVersion != "old-version" || response != "accepted" || created != "2024-10-02T10:00:00Z" || claim != "" {
		t.Fatal("legacy reservation changed or adopted", owner, source, remote, oldVersion, response, created, claim)
	}
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 107 {
		t.Fatal("upgraded schema marker", version, err)
	}
	if err := db.BeginCalendarResponse(t.Context(), "owner", "source", "remote", "old-version", "declined"); !errors.Is(err, ErrCalendarResponsePending) {
		t.Fatal("migration lost duplicate-response protection", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New(path)
	if err != nil {
		t.Fatal("upgraded/fresh schema failed reopen", err)
	}
	if err := db.BeginCalendarResponse(t.Context(), "owner", "source", "remote", "old-version", "accepted"); !errors.Is(err, ErrCalendarResponsePending) {
		t.Fatal("restart lost uncertain response", err)
	}
}

func TestUserCalendarResponseReservationMigrationHandlesPartialColumnAndRejectsWrongShape(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "already-present", false: "wrong-shape"}[valid], func(t *testing.T) {
			db, err := New(filepath.Join(t.TempDir(), "partial.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Write().Exec(`DELETE FROM schema_version;INSERT INTO schema_version(version) VALUES(106)`); err != nil {
				t.Fatal(err)
			}
			if !valid {
				if _, err := db.Write().Exec(`ALTER TABLE calendar_response_requests DROP COLUMN claim_id; ALTER TABLE calendar_response_requests ADD COLUMN claim_id INTEGER DEFAULT 0`); err != nil {
					t.Fatal(err)
				}
			}
			err = migrateV106ToV107(db.Write())
			if valid && err != nil {
				t.Fatal("compatible partial schema rejected", err)
			}
			if !valid && err == nil {
				t.Fatal("incompatible claim identity accepted")
			}
			var version int
			if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			want := 106
			if valid {
				want = 107
			}
			if version != want {
				t.Fatal("failed migration advanced schema", version, want)
			}
		})
	}
}
