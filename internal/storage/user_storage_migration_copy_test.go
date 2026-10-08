package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func migrationCopyDestination(t *testing.T, owner string) *DB {
	t.Helper()
	db, err := New(filepath.Join(t.TempDir(), "destination.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if owner != "" {
		username := owner
		if owner == "../../outside" {
			username = "path-user"
		}
		if err := initializeUserStore(t.Context(), db, userStoreOwner{id: owner, username: username, normalized: username}); err != nil {
			t.Fatal(err)
		}
		if err := ensureUserMailDeliverySchema(t.Context(), db); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func migrationCopyReport(t *testing.T, source *DB) UserStorageMigrationPreflight {
	t.Helper()
	report, err := inspectUserStorageMigration(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestUserStorageMigrationCopyPreservesPartitionedRowsSearchAndPendingWork(t *testing.T) {
	source := sharedMigrationFixture(t)
	tx, err := source.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureUserProviderDraftSchema(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	migrationFixtureSQL(t, source, `
 INSERT INTO password_credentials(user_id,password_hash) VALUES('alice','private-hash');
 UPDATE users SET name='Preserved Bob profile',avatar_url='/original/avatar',deletion_pending=1,deletion_started_by='admin' WHERE id='bob';
 UPDATE accounts SET encrypted_password=x'000102FF',is_deleting=1 WHERE id='bob-mail';
 UPDATE messages SET from_email='sender@example.com',date_sent='2026-10-08T09:10:11.123400+03:30',raw_path='data/original/raw.eml' WHERE account_id='alice-mail';
 INSERT INTO remote_content_messages SELECT id FROM messages;
 INSERT INTO remote_content_senders(sender_email) VALUES('sender@example.com'),('unattributable@example.com');
 INSERT INTO contact_sync_operations(id,user_id,contact_id,payload_json,status,attempt_count,locked_at) VALUES('pending','alice','alice-contact','{"contact":{"ID":"alice-contact","SaveTargets":["account:bob-mail"]},"previous":{"ID":"bob-contact"}}','running',4,'2026-10-08T09:10:11.123400+03:30');
 INSERT INTO outgoing_sends(id,account_id,transport,envelope_from,send_after,status,mime_data,message_json) VALUES('reply','alice-mail','smtp','alice@example.com','2026-10-08T09:10:11.123400+03:30','ambiguous',x'000102FF','{"original":"snapshot"}');
 INSERT INTO calendar_reply_jobs(id,user_id,source_id,resource_id,remote_id,version,response,payload,state) VALUES('reply','alice','alice-calendar','event','original-resource','original-version','accepted','{"event":{"UserID":"alice","ID":"alice-event","SourceID":"alice-calendar"}}','conflict');
 INSERT INTO calendar_response_requests(user_id,source_id,remote_id,version,response,claim_id) VALUES('alice','alice-calendar','original-resource','original-version','accepted','original-claim');
 INSERT INTO gofer_provider_draft_states(account_id,draft_key,provider,mailbox_subject,local_message_id,folder_id,remote_id,remote_revision) SELECT 'alice-mail','draft','gmail','old-original-principal',id,'alice-folder','original-draft','original-etag' FROM messages WHERE account_id='alice-mail';
 INSERT INTO gofer_provider_draft_operations(id,account_id,draft_key,kind,revision_token,mime_data,status,create_attempted,candidate_id) VALUES(42,'alice-mail','draft','upsert','original-revision',x'000102FF','ambiguous',1,'original-candidate');
 UPDATE sqlite_sequence SET seq=10000 WHERE name IN ('messages','gofer_provider_draft_operations');`)
	report := migrationCopyReport(t, source)
	path := source.Path()
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	central := migrationCopyDestination(t, "")
	if err := copyUserStorageMigrationRows(t.Context(), path, central, report, ""); err != nil {
		t.Fatal(err)
	}
	var status, hash string
	var pending, accounts, centralContacts int
	if err := central.Read().QueryRow(`SELECT status,deletion_pending FROM users WHERE id='bob'`).Scan(&status, &pending); err != nil || status != "disabled" || pending != 1 {
		t.Fatal("owner lifecycle changed", status, pending, err)
	}
	if err := central.Read().QueryRow(`SELECT password_hash FROM password_credentials WHERE user_id='alice'`).Scan(&hash); err != nil || hash != "private-hash" {
		t.Fatal("authentication not retained", hash, err)
	}
	if err := central.Read().QueryRow(`SELECT (SELECT count(*) FROM accounts),(SELECT count(*) FROM contact_profiles)`).Scan(&accounts, &centralContacts); err != nil || accounts != 0 || centralContacts != 0 {
		t.Fatal("private content in central store", accounts, centralContacts, err)
	}
	for _, owner := range []string{"alice", "bob", "../../outside"} {
		local := migrationCopyDestination(t, owner)
		if err := copyUserStorageMigrationRows(t.Context(), path, local, report, owner); err != nil {
			t.Fatal(owner, err)
		}
		var foreign, authentication, messages int
		if err := local.Read().QueryRow(`SELECT (SELECT count(*) FROM accounts WHERE user_id<>?), (SELECT count(*) FROM password_credentials),(SELECT count(*) FROM messages)`, owner).Scan(&foreign, &authentication, &messages); err != nil || foreign != 0 || authentication != 0 {
			t.Fatal("partition isolation", owner, foreign, authentication, err)
		}
		if owner == "../../outside" {
			if messages != 0 {
				t.Fatal("unused owner received mail")
			}
			continue
		}
		if messages != 1 {
			t.Fatal("missing mail", owner, messages)
		}
		if owner == "bob" {
			var name, avatar string
			if err := local.Read().QueryRow(`SELECT name,avatar_url FROM users WHERE id='bob'`).Scan(&name, &avatar); err != nil || name != "Preserved Bob profile" || avatar != "/original/avatar" {
				t.Fatal("owner profile stub lost", name, avatar, err)
			}
		}
		var found, other int
		if err := local.Read().QueryRow(`SELECT count(*) FROM message_search WHERE message_search MATCH ?`, owner).Scan(&found); err != nil || found != 1 {
			t.Fatal("rebuilt search", owner, found, err)
		}
		foreignOwner := "alice"
		if owner == "alice" {
			foreignOwner = "bob"
		}
		if err := local.Read().QueryRow(`SELECT count(*) FROM message_search WHERE message_search MATCH ?`, foreignOwner).Scan(&other); err != nil || other != 0 {
			t.Fatal("foreign search result", owner, other, err)
		}
		var sequence int64
		if err := local.Read().QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='messages'`).Scan(&sequence); err != nil || sequence != 10000 {
			t.Fatal("installation ID gaps lost", owner, sequence, err)
		}
		if owner == "alice" {
			var timestamp, raw, state, payload, binding, mime, claim string
			if err := local.Read().QueryRow(`SELECT CAST(date_sent AS TEXT),raw_path FROM messages`).Scan(&timestamp, &raw); err != nil || timestamp != "2026-10-08T09:10:11.123400+03:30" || raw != "data/original/raw.eml" {
				t.Fatal("timestamp/blob path rewritten", timestamp, raw, err)
			}
			if err := local.Read().QueryRow(`SELECT status,payload_json FROM contact_sync_operations`).Scan(&state, &payload); err != nil || state != "running" || !strings.Contains(payload, "account:bob-mail") {
				t.Fatal("historic pending snapshot rewritten", state, payload, err)
			}
			if err := local.Read().QueryRow(`SELECT mailbox_subject,hex(mime_data) FROM gofer_provider_draft_states JOIN gofer_provider_draft_operations USING(account_id,draft_key)`).Scan(&binding, &mime); err != nil || binding != "old-original-principal" || mime != "000102FF" {
				t.Fatal("provider draft binding/blob changed", binding, mime, err)
			}
			if err := local.Read().QueryRow(`SELECT claim_id FROM calendar_response_requests`).Scan(&claim); err != nil || claim != "original-claim" {
				t.Fatal("Calendar claim changed", claim, err)
			}
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("source mutated by copier", err)
	}
}

func TestUserStorageMigrationCopyLegacySenderPreservesOriginalPermission(t *testing.T) {
	for _, multiOwner := range []bool{false, true} {
		t.Run(map[bool]string{false: "single-owner", true: "multiple-owners"}[multiOwner], func(t *testing.T) {
			source := sharedMigrationFixture(t)
			if !multiOwner {
				migrationFixtureSQL(t, source, `DELETE FROM message_search WHERE account_id='bob-mail'; DELETE FROM accounts WHERE user_id='bob'`)
			}
			migrationFixtureSQL(t, source, `UPDATE messages SET from_email='sender@example.com'; INSERT INTO remote_content_messages SELECT id FROM messages; INSERT INTO remote_content_senders(sender_email) VALUES('sender@example.com'),('unapproved@example.com')`)
			before := source.IsRemoteContentAllowedForSenderForUser(t.Context(), "sender@example.com", "alice")
			report := migrationCopyReport(t, source)
			local := migrationCopyDestination(t, "alice")
			if err := copyUserStorageMigrationRows(t.Context(), source.Path(), local, report, "alice"); err != nil {
				t.Fatal(err)
			}
			if got := local.IsRemoteContentAllowedForSenderForUser(t.Context(), "sender@example.com", "alice"); got != before || got == multiOwner {
				t.Fatal("legacy permission expanded", got, before, multiOwner)
			}
			if local.IsRemoteContentAllowedForSenderForUser(t.Context(), "unapproved@example.com", "alice") {
				t.Fatal("unattributable sender granted")
			}
		})
	}
}

func TestUserStorageMigrationCopyRefusesDirtyDestinationsAndRollsBackCorruption(t *testing.T) {
	source := sharedMigrationFixture(t)
	report := migrationCopyReport(t, source)
	for name, query := range map[string]string{
		"dirty":                     `INSERT INTO app_settings(user_id,key,value) VALUES('alice','existing','keep')`,
		"corrupt-content":           `CREATE TRIGGER corrupt_migrated_message AFTER INSERT ON messages BEGIN UPDATE messages SET subject='corrupted' WHERE id=NEW.id; END`,
		"later-table-corruption":    `CREATE TRIGGER corrupt_prior_copy AFTER INSERT ON messages BEGIN UPDATE accounts SET display_name='corrupted'; END`,
		"missing-source-fk":         `CREATE TRIGGER break_migrated_link AFTER INSERT ON messages BEGIN UPDATE messages SET thread_parent_id=99999 WHERE id=NEW.id; END`,
		"wrong-destination-version": `UPDATE schema_version SET version=106`,
		"unexpected-central-rows":   `CREATE TRIGGER inject_central_data AFTER INSERT ON messages BEGIN INSERT INTO avatar_attempt_logs(email_hash,email,provider,status) VALUES('private','private@example.com','gravatar','failed'); END`,
	} {
		t.Run(name, func(t *testing.T) {
			local := migrationCopyDestination(t, "alice")
			migrationFixtureSQL(t, local, query)
			if err := copyUserStorageMigrationRows(t.Context(), source.Path(), local, report, "alice"); err == nil {
				t.Fatal("unsafe destination accepted")
			}
			var count int
			if err := local.Read().QueryRow(`SELECT count(*) FROM accounts`).Scan(&count); err != nil || count != 0 {
				t.Fatal("failed copy did not roll back", count, err)
			}
			if err := local.Write().QueryRow(`SELECT count(*) FROM pragma_database_list WHERE name='migration_source'`).Scan(&count); err != nil || count != 0 {
				t.Fatal("source returned attached to pool", count, err)
			}
		})
	}
}

func TestUserStorageMigrationCopyCancellationDetachesAndRetryUsesFreshTransaction(t *testing.T) {
	source := sharedMigrationFixture(t)
	report := migrationCopyReport(t, source)
	local := migrationCopyDestination(t, "alice")
	ctx, cancel := context.WithCancel(t.Context())
	err := withUserStorageMigrationSource(ctx, source.Path(), local, func(conn *sql.Conn) error {
		var read int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM migration_source.accounts`).Scan(&read); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
	if err := copyUserStorageMigrationRows(t.Context(), source.Path(), local, report, "alice"); err != nil {
		t.Fatal("detached retry failed", err)
	}
	if err := copyUserStorageMigrationRows(t.Context(), source.Path(), local, report, "alice"); err == nil {
		t.Fatal("populated destination silently overwritten")
	}
}

func TestUserStorageMigrationCopyRefusesSourceAlias(t *testing.T) {
	source := sharedMigrationFixture(t)
	report := migrationCopyReport(t, source)
	if err := copyUserStorageMigrationRows(t.Context(), source.Path(), source, report, ""); err == nil {
		t.Fatal("source accepted as destination")
	}
	alias := filepath.Join(t.TempDir(), "alias.db")
	if err := os.Link(source.Path(), alias); err != nil {
		t.Fatal(err)
	}
	// No writable open of the alias; the identity check precedes the attach.
	destination := &DB{path: alias, write: source.write}
	if err := copyUserStorageMigrationRows(t.Context(), source.Path(), destination, report, ""); err == nil {
		t.Fatal("hardlink source accepted as destination")
	}
}

func TestUserStorageMigrationCopyRecognizesHistoricalColumnsWithoutUpgradingSource(t *testing.T) {
	for _, version := range []int{105, 106, 107} {
		t.Run(map[int]string{105: "105", 106: "106", 107: "107"}[version], func(t *testing.T) {
			source := sharedMigrationFixture(t)
			migrationFixtureSQL(t, source, `INSERT INTO web_push_subscriptions(endpoint,user_id,p256dh,auth,revision) VALUES('https://push.test/sub','alice','public','auth','original-revision');
 INSERT INTO calendar_response_requests(user_id,source_id,remote_id,version,response,claim_id) VALUES('alice','alice-calendar','old-event','old-version','accepted','original-nonce')`)
			if version < 107 {
				migrationFixtureSQL(t, source, `ALTER TABLE calendar_response_requests DROP COLUMN claim_id`)
			}
			if version == 105 {
				migrationFixtureSQL(t, source, `ALTER TABLE web_push_subscriptions DROP COLUMN revision`)
			}
			if _, err := source.Write().Exec(`UPDATE schema_version SET version=?`, version); err != nil {
				t.Fatal(err)
			}
			report := migrationCopyReport(t, source)
			path := source.Path()
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			central := migrationCopyDestination(t, "")
			local := migrationCopyDestination(t, "alice")
			for _, target := range []struct {
				db    *DB
				owner string
			}{{central, ""}, {local, "alice"}} {
				if err := copyUserStorageMigrationRows(t.Context(), path, target.db, report, target.owner); err != nil {
					t.Fatal(err)
				}
			}
			var revision, claim string
			if err := central.Read().QueryRow(`SELECT revision FROM web_push_subscriptions`).Scan(&revision); err != nil || revision == "" || (version != 105 && revision != "original-revision") {
				t.Fatal("push registration identity", revision, err)
			}
			if err := local.Read().QueryRow(`SELECT claim_id FROM calendar_response_requests`).Scan(&claim); err != nil || (version == 107 && claim != "original-nonce") || (version < 107 && claim != "") {
				t.Fatal("legacy reservation adopted or existing claim changed", claim, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatal("source upgraded by copy", err)
			}
		})
	}
}

func TestUserStorageMigrationCopyHighWaterIncludesOtherOwnersWhenSourceSequenceWasReset(t *testing.T) {
	source := sharedMigrationFixture(t)
	migrationFixtureSQL(t, source, `UPDATE sqlite_sequence SET seq=0 WHERE name='messages'`)
	report := migrationCopyReport(t, source)
	local := migrationCopyDestination(t, "alice")
	if err := copyUserStorageMigrationRows(t.Context(), source.Path(), local, report, "alice"); err != nil {
		t.Fatal(err)
	}
	result, err := local.Write().Exec(`INSERT INTO messages(account_id,subject) VALUES('alice-mail','new mail')`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil || id != 3 {
		t.Fatal("reused another owner's global message ID", id, err)
	}
}

func TestUserStorageMigrationCopyAttachedSourceIsReadOnlyAndRestoresPoolSettings(t *testing.T) {
	source := sharedMigrationFixture(t)
	local := migrationCopyDestination(t, "alice")
	if err := withUserStorageMigrationSource(t.Context(), source.Path(), local, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(t.Context(), `UPDATE migration_source.accounts SET display_name='must-not-write'`)
		if err == nil {
			t.Fatal("attached source accepted a write")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var setting, attachments int
	if err := local.Write().QueryRow(`PRAGMA temp_store`).Scan(&setting); err != nil || setting != 2 {
		t.Fatal("writer settings not restored", setting, err)
	}
	if err := local.Write().QueryRow(`SELECT count(*) FROM pragma_database_list WHERE name='migration_source'`).Scan(&attachments); err != nil || attachments != 0 {
		t.Fatal("attached source returned to pool", attachments, err)
	}
}
