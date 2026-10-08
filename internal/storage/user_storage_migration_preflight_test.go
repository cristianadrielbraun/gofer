package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
)

func sharedMigrationFixture(t *testing.T) *DB {
	t.Helper()
	db := newUserStoreTestSystem(t)
	if _, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,email_address) VALUES('alice-mail','alice','alice@example.com'),('bob-mail','bob','bob@example.com');
 INSERT INTO contact_profiles(id,user_id,display_name) VALUES('alice-contact','alice','Alice'),('bob-contact','bob','Bob');
 INSERT INTO app_settings(user_id,key,value) VALUES('alice','unknown-preserved-setting','alice-value'),('bob','unknown-preserved-setting','bob-value'),('admin','management-setting','admin-value');
 INSERT INTO calendar_sources(id,user_id,account_id,provider,remote_id,name) VALUES('alice-calendar','alice','alice-mail','caldav','https://alice.test/calendar/','Alice'),('bob-calendar','bob','bob-mail','caldav','https://bob.test/calendar/','Bob');
 INSERT INTO calendar_events(id,user_id,source_id,remote_id,start_at,end_at) VALUES('alice-event','alice','alice-calendar','https://alice.test/calendar/event','2026-10-08 10:00:00','2026-10-08 11:00:00'),('bob-event','bob','bob-calendar','https://bob.test/calendar/event','2026-10-08 10:00:00','2026-10-08 11:00:00');
 UPDATE users SET status='disabled' WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		account, folder := owner+"-mail", owner+"-folder"
		if err := db.UpsertFolders(t.Context(), []UpsertFolderInput{{ID: folder, AccountID: account, Name: "Inbox", Selectable: true}}); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertSyncMessages(t.Context(), []SyncMessage{{AccountID: account, FolderID: folder, MessageID: "<" + owner + "@test>", Subject: owner + " searchable", RemoteUID: 1, DateSent: time.Now().UTC()}}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func migrationFixtureSQL(t *testing.T, db *DB, query string) {
	t.Helper()
	if _, err := db.Write().Exec(query); err != nil {
		t.Fatal(err)
	}
}

func TestUserStorageMigrationPreflightReadsPopulatedSharedSourceWithoutChangingIt(t *testing.T) {
	db := sharedMigrationFixture(t)
	migrationFixtureSQL(t, db, `INSERT INTO contact_sync_operations(id,user_id,contact_id,payload_json) VALUES('pending','alice','alice-contact','{"contact":{"ID":"alice-contact","SaveTargets":["account:alice-mail"]},"excluded_account_id":"gone-original-account"}');
 INSERT INTO contact_conflicts(id,user_id,profile_id,account_id) VALUES('historical-conflict','alice','alice-contact','deleted-account');
 UPDATE sqlite_sequence SET seq=10000 WHERE name='messages'`)
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	report, err := InspectUserStorageMigration(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != CurrentSchemaVersion || report.Owners != 3 || report.Accounts != 2 {
		t.Fatal("preflight metadata", report)
	}
	counts := map[string]int64{}
	for _, table := range report.Tables {
		counts[table.Name] = table.Rows
	}
	if counts["messages"] != 2 || counts["contact_sync_operations"] != 1 || counts["app_settings"] != 3 {
		t.Fatal("source counts", counts)
	}
	after, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("source file changed", err)
	}
	if _, err := os.Lstat(path + ".users"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preflight created destination stores", err)
	}
	if _, err := os.Lstat(path + ".layout.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preflight published a layout", err)
	}
	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	var highWater int
	if err := readOnly.Read().QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='messages'`).Scan(&highWater); err != nil || highWater != 10000 {
		t.Fatal("source high-water changed", highWater, err)
	}
	var status string
	if err := readOnly.Read().QueryRow(`SELECT status FROM users WHERE id='bob'`).Scan(&status); err != nil || status != "disabled" {
		t.Fatal("source owner enabled", status, err)
	}
}

func TestUserStorageMigrationPreflightRejectsAmbiguousSchemaAndCrossOwnerData(t *testing.T) {
	cases := map[string]string{
		"unknown-table":                 `CREATE TABLE forgotten_mail_data(id TEXT)`,
		"unknown-column":                `ALTER TABLE accounts ADD COLUMN forgotten_configuration TEXT`,
		"unknown-generated-column":      `ALTER TABLE accounts ADD COLUMN generated_configuration TEXT GENERATED ALWAYS AS(email_address) VIRTUAL`,
		"missing-table":                 `DROP TABLE contact_conflicts`,
		"mixed-layout":                  `CREATE TABLE gofer_user_cleanup_receipts(user_id TEXT PRIMARY KEY,completed_at DATETIME); INSERT INTO gofer_user_cleanup_receipts VALUES('alice',CURRENT_TIMESTAMP)`,
		"no-account-owner":              `INSERT INTO accounts(id,user_id,email_address) VALUES('orphan',NULL,'orphan@example.com')`,
		"upload-owner-alias":            `INSERT INTO users(id,username,username_normalized) VALUES(' alice ','alias','alias')`,
		"message-folder-owner":          `UPDATE message_folder_state SET folder_id='bob-folder',remote_uid=2 WHERE message_id=(SELECT id FROM messages WHERE account_id='alice-mail')`,
		"contact-profile-owner":         `INSERT INTO contact_fields(id,user_id,profile_id,kind) VALUES('foreign-field','alice','bob-contact','phone')`,
		"calendar-source-owner":         `UPDATE calendar_events SET source_id='bob-calendar' WHERE id='alice-event'`,
		"optional-account-owner":        `INSERT INTO contact_conflicts(id,user_id,profile_id,account_id) VALUES('foreign-conflict','alice','alice-contact','bob-mail')`,
		"optional-profile-owner":        `INSERT INTO contact_observations(id,user_id,profile_id,normalized_email) VALUES('foreign-observation','alice','bob-contact','person@example.com')`,
		"optional-event-owner":          `INSERT INTO calendar_create_requests(user_id,request_id,source_id,request_hash,event_id) VALUES('alice','request','alice-calendar','hash','bob-event')`,
		"contact-profile-payload-owner": `INSERT INTO contact_sync_operations(id,user_id,contact_id,payload_json) VALUES('foreign-op','alice','alice-contact','{"contact":{"ID":"bob-contact"}}')`,
		"search-owner":                  `UPDATE message_search SET account_id='bob-mail' WHERE rowid=(SELECT id FROM messages WHERE account_id='alice-mail')`,
		"search-orphan":                 `INSERT INTO message_search(rowid,account_id,subject) VALUES(999999,'alice-mail','orphan search')`,
		"invalid-high-water":            `UPDATE sqlite_sequence SET seq='invalid' WHERE name='messages'`,
		"unknown-high-water":            `INSERT INTO sqlite_sequence(name,seq) VALUES('forgotten_table',999)`,
		"cross-owner-without-source-fk": `DROP TABLE message_labels; CREATE TABLE message_labels(message_id INTEGER,label_id TEXT); INSERT INTO labels(id,account_id,name) VALUES('foreign-label','bob-mail','Foreign'); INSERT INTO message_labels VALUES((SELECT id FROM messages WHERE account_id='alice-mail'),'foreign-label')`,
		"orphan-without-source-fk":      `DROP TABLE message_labels; CREATE TABLE message_labels(message_id INTEGER,label_id TEXT); INSERT INTO labels(id,account_id,name) VALUES('local-label','alice-mail','Local'); INSERT INTO message_labels VALUES(999999,'local-label')`,
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			db := sharedMigrationFixture(t)
			migrationFixtureSQL(t, db, query)
			if _, err := InspectUserStorageMigration(t.Context(), db.Path()); !errors.Is(err, ErrUserStorageMigrationSource) {
				t.Fatal("unsafe source accepted", err)
			}
		})
	}
}

func TestUserStorageMigrationPreflightPreservesHistoricalBlockedPayloadsAndSameOwnerLinks(t *testing.T) {
	db := sharedMigrationFixture(t)
	migrationFixtureSQL(t, db, `ALTER TABLE schema_version DROP COLUMN applied_at;
 INSERT INTO accounts(id,user_id,email_address) VALUES('alice-second','alice','other@example.com');
 INSERT INTO folders(id,account_id,name) VALUES('alice-second-folder','alice-second','Inbox');
 UPDATE message_folder_state SET folder_id='alice-second-folder' WHERE message_id=(SELECT id FROM messages WHERE account_id='alice-mail');
 INSERT INTO contact_sync_operations(id,user_id,contact_id,payload_json) VALUES('ignored-snapshot','alice','alice-contact','{"contact":{"ID":"alice-contact","SaveTargets":["account:bob-mail"]},"previous":{"ID":"bob-contact"},"excluded_account_id":" bob-mail "}'),('malformed','alice','deleted-profile','unreadable historical payload'),('valid-stale','alice','deleted-profile','{"contact":{"ID":"deleted-profile","SaveTargets":["account:deleted-account","book:deleted-book"]}}');
 INSERT INTO outgoing_sends(id,account_id,transport,envelope_from,send_after,status,message_json) VALUES('historical-send','alice-mail','smtp','alice@example.com',CURRENT_TIMESTAMP,'ambiguous','unreadable historical payload');
 INSERT INTO calendar_reply_jobs(id,user_id,source_id,resource_id,remote_id,version,response,payload) VALUES('historical-send','alice','alice-calendar','original-resource','original-remote','original-version','accepted','{"Event":{"ID":"deleted-event","UserID":"alice","SourceID":"deleted-source"},"UserAuthority":{"Owner":"alice","Account":"deleted-account","Source":"deleted-source","Event":"deleted-event","Format":1}}')`)
	if _, err := InspectUserStorageMigration(t.Context(), db.Path()); err != nil {
		t.Fatal("valid retained history rejected", err)
	}
	var payload, remote, version, status string
	if err := db.Read().QueryRow(`SELECT j.payload,j.remote_id,j.version,s.status FROM calendar_reply_jobs j JOIN outgoing_sends s ON s.id=j.id`).Scan(&payload, &remote, &version, &status); err != nil || remote != "original-remote" || version != "original-version" || status != "ambiguous" {
		t.Fatal("original binding changed", remote, version, status, err)
	}
}

func TestUserStorageMigrationPreflightRejectsForeignCalendarSnapshotAndAuthority(t *testing.T) {
	for _, payload := range []string{`{"Event":{"UserID":"bob"}}`, `{"Event":{"UserID":"alice","SourceID":"bob-calendar"}}`, `{"Event":{"UserID":"alice","ID":"bob-event"}}`, `{"UserAuthority":{"Owner":"bob"}}`, `{"UserAuthority":{"Owner":"alice","Account":"bob-mail"}}`} {
		t.Run(payload, func(t *testing.T) {
			db := sharedMigrationFixture(t)
			migrationFixtureSQL(t, db, `INSERT INTO outgoing_sends(id,account_id,transport,envelope_from,send_after,status) VALUES('send','alice-mail','smtp','alice@example.com',CURRENT_TIMESTAMP,'ambiguous')`)
			if _, err := db.Write().Exec(`INSERT INTO calendar_reply_jobs(id,user_id,source_id,resource_id,remote_id,version,response,payload) VALUES('send','alice','alice-calendar','resource','remote','version','accepted',?)`, payload); err != nil {
				t.Fatal(err)
			}
			if _, err := InspectUserStorageMigration(t.Context(), db.Path()); !errors.Is(err, ErrUserStorageMigrationSource) {
				t.Fatal("foreign calendar payload accepted", err)
			}
		})
	}
	for _, notification := range []string{`{"UserID":"bob"}`, `{"UserID":"alice","SourceID":"bob-calendar"}`, `{"UserAuthority":{"Owner":"alice","Source":"bob-calendar"}}`} {
		t.Run(notification, func(t *testing.T) {
			db := sharedMigrationFixture(t)
			if _, err := db.Write().Exec(`INSERT INTO outgoing_sends(id,account_id,transport,envelope_from,send_after,message_json) VALUES('send','alice-mail','smtp','alice@example.com',CURRENT_TIMESTAMP,?)`, `{"calendar_notification":`+notification+`}`); err != nil {
				t.Fatal(err)
			}
			if _, err := InspectUserStorageMigration(t.Context(), db.Path()); !errors.Is(err, ErrUserStorageMigrationSource) {
				t.Fatal("foreign notification payload accepted", err)
			}
		})
	}
}

func TestUserStorageMigrationPreflightHonorsRuntimeLockCancellationAndMissingFiles(t *testing.T) {
	db := sharedMigrationFixture(t)
	lock, err := runtimeguard.Acquire(db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := InspectUserStorageMigration(t.Context(), db.Path()); !errors.Is(err, runtimeguard.ErrAlreadyLocked) {
		t.Fatal("running runtime accepted", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectUserStorageMigration(t.Context(), db.Path()); err != nil {
		t.Fatal("lock retry", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := InspectUserStorageMigration(ctx, db.Path()); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled inspection admitted", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := InspectUserStorageMigration(t.Context(), missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing database created", err)
	}
	for _, suffix := range []string{"", ".lock"} {
		if _, err := os.Lstat(missing + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("missing source created a file", suffix, err)
		}
	}
}

func TestUserStorageMigrationPreflightAcceptsMainSchemaAndKeepsHistoricalClaimsUnchanged(t *testing.T) {
	for _, mode := range []string{"main-105", "main-105-no-push-table", "partial-105-claim", "shared-106", "shared-107"} {
		t.Run(mode, func(t *testing.T) {
			db := sharedMigrationFixture(t)
			version := 107
			if mode != "shared-107" {
				version = 106
				if mode != "shared-106" {
					version = 105
				}
				if mode != "partial-105-claim" {
					migrationFixtureSQL(t, db, `ALTER TABLE calendar_response_requests DROP COLUMN claim_id`)
				}
				if mode == "main-105-no-push-table" {
					migrationFixtureSQL(t, db, `DROP TABLE web_push_subscriptions`)
				} else if version == 105 {
					migrationFixtureSQL(t, db, `ALTER TABLE web_push_subscriptions DROP COLUMN revision`)
				}
				if _, err := db.Write().Exec(`DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(?)`, version); err != nil {
					t.Fatal(err)
				}
			}
			migrationFixtureSQL(t, db, `INSERT INTO calendar_response_requests(user_id,source_id,remote_id,version,response) VALUES('alice','alice-calendar','original-resource','original-etag','accepted')`)
			if mode == "partial-105-claim" || mode == "shared-107" {
				migrationFixtureSQL(t, db, `UPDATE calendar_response_requests SET claim_id='original-nonce'`)
			}
			path := db.Path()
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			report, err := InspectUserStorageMigration(t.Context(), path)
			if err != nil || report.SchemaVersion != version || report.TargetSchemaVersion != CurrentSchemaVersion {
				t.Fatal("supported shared schema rejected", report, err)
			}
			wantClaim := mode == "partial-105-claim" || mode == "shared-107"
			foundResponse := false
			for _, table := range report.Tables {
				if table.Name == "calendar_response_requests" {
					foundResponse = true
					hasClaim := false
					for _, column := range table.Columns {
						if column == "claim_id" {
							hasClaim = true
						}
					}
					if hasClaim != wantClaim || table.Rows != 1 {
						t.Fatal("source claim shape misreported", table)
					}
				}
				if table.Name == "web_push_subscriptions" {
					hasRevision := false
					for _, column := range table.Columns {
						if column == "revision" {
							hasRevision = true
						}
					}
					if hasRevision != (version >= 106) {
						t.Fatal("source push shape misreported", table)
					}
				}
			}
			if !foundResponse {
				t.Fatal("pending response omitted")
			}
			after, err := os.ReadFile(path)
			if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatal("historical shared source upgraded or changed", err)
			}
		})
	}
}

func TestUserStorageMigrationPreflightRejectsUnknownVersionsAndFalseUpgradeMarkers(t *testing.T) {
	for _, query := range []string{
		`DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(104)`,
		`DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(108)`,
		`ALTER TABLE calendar_response_requests DROP COLUMN claim_id`,
		`DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(106); ALTER TABLE web_push_subscriptions DROP COLUMN revision`,
		`DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(106); ALTER TABLE calendar_response_requests DROP COLUMN claim_id; ALTER TABLE calendar_response_requests ADD COLUMN claim_id TEXT`,
	} {
		t.Run(query, func(t *testing.T) {
			db := sharedMigrationFixture(t)
			migrationFixtureSQL(t, db, query)
			if _, err := InspectUserStorageMigration(t.Context(), db.Path()); !errors.Is(err, ErrUserStorageMigrationSource) {
				t.Fatal("unrecognized source accepted", err)
			}
		})
	}
}
