package storage

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/keywords"
)

func seedV153KeywordLabels(t *testing.T, db *DB) {
	t.Helper()
	migrationFixtureSQL(t, db, `
		DROP TABLE message_provider_keywords;
		DROP TABLE retired_keyword_labels;
		ALTER TABLE folders DROP COLUMN mailbox_attributes;
		ALTER TABLE folders DROP COLUMN flags_metadata_synced;
		ALTER TABLE message_folder_state DROP COLUMN remote_keywords;
		DELETE FROM schema_version;
		INSERT INTO schema_version(version) VALUES (153);
		UPDATE folders SET uid_validity = 100, highest_modseq = 55 WHERE id = 'alice-folder';
		INSERT INTO labels (id, account_id, name, provider_id, provider_type) VALUES
			('color', 'alice-mail', 'Orange', '$MailFlagBit0', 'imap_keyword'),
			('memo', 'alice-mail', '$HasMemo', '$HasMemo', 'imap_keyword'),
			('custom', 'alice-mail', '$Memo', 'Project', 'imap_keyword'),
			('local', 'alice-mail', '$New', '', 'local');
		INSERT INTO message_labels (message_id, label_id)
			SELECT m.id, l.id FROM messages m JOIN labels l ON l.account_id = m.account_id;
		INSERT INTO message_label_copies (message_id, label_id, remote_message_id)
			SELECT message_id, label_id, 'copy-1' FROM message_labels WHERE label_id = 'memo';`)
}

func assertV153KeywordsPreserved(t *testing.T, db *DB) {
	t.Helper()
	ctx := t.Context()
	var messageID int64
	if err := db.Read().QueryRow(`SELECT id FROM messages WHERE account_id = 'alice-mail'`).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	for copyID, want := range map[string][]string{"": {"$MailFlagBit0"}, "copy-1": {"$HasMemo"}} {
		got, fetched, err := db.GetProviderMessageKeywords(ctx, messageID, copyID)
		if err != nil || fetched || !reflect.DeepEqual(got, want) {
			t.Fatalf("legacy keywords for %q = %#v, fetched=%v, err=%v", copyID, got, fetched, err)
		}
	}
	var kept string
	if err := db.Read().QueryRow(`SELECT group_concat(id) FROM (SELECT id FROM labels ORDER BY id)`).Scan(&kept); err != nil || kept != "custom,local" {
		t.Fatalf("remaining labels = %q, %v", kept, err)
	}
	var retiredName string
	if err := db.Read().QueryRow(`SELECT name FROM retired_keyword_labels WHERE id = 'color' AND account_id = 'alice-mail' AND provider_id = '$MailFlagBit0'`).Scan(&retiredName); err != nil || retiredName != "Orange" {
		t.Fatalf("retired label identity = %q, %v", retiredName, err)
	}
	if got, err := db.GetHighestModSeq(ctx, "alice-folder"); err != nil || got != 0 {
		t.Fatalf("old MODSEQ is still usable: %d, %v", got, err)
	}
	if got, err := db.GetIMAPMessageKeywords(ctx, "alice-folder", 1); err != nil || got != nil {
		t.Fatalf("legacy keyword metadata got assigned to UID 1: %#v, %v", got, err)
	}
}

func TestKeywordMetadataMigrationPreservesLegacyKeywords(t *testing.T) {
	db := sharedMigrationFixture(t)
	seedV153KeywordLabels(t, db)
	for range 2 {
		if err := migrateV153ToV154(db.Write()); err != nil {
			t.Fatal(err)
		}
		assertV153KeywordsPreserved(t, db)
	}
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 154 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
}

func TestUserStorageCopyPreservesKeywordMetadata(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "current"
		if legacy {
			name = "v153"
		}
		t.Run(name, func(t *testing.T) {
			source := sharedMigrationFixture(t)
			if legacy {
				seedV153KeywordLabels(t, source)
			} else {
				migrationFixtureSQL(t, source, `
					UPDATE folders SET mailbox_attributes='["snoozed"]', highest_modseq=55, flags_metadata_synced=1 WHERE id='alice-folder';
					UPDATE message_folder_state SET remote_keywords='["$MailFlagBit1"]' WHERE folder_id='alice-folder';
					INSERT INTO message_provider_keywords (message_id, remote_message_id, keywords, fetched)
						SELECT id, 'copy-1', '["$notify"]', 1 FROM messages;
					INSERT INTO retired_keyword_labels(id, account_id, name, provider_id) VALUES
						('old-alice-label', 'alice-mail', 'Orange', '$MailFlagBit0'), ('old-bob-label', 'bob-mail', 'Blue', '$MailFlagBit2');`)
			}
			dest := migrationCopyDestination(t, "alice")
			if err := copyUserStorageMigrationRows(t.Context(), source.Path(), dest, migrationCopyReport(t, source), "alice"); err != nil {
				t.Fatal(err)
			}
			path := dest.Path()
			if err := dest.Close(); err != nil {
				t.Fatal(err)
			}
			opened, err := OpenExisting(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = opened.Close() })
			if legacy {
				assertV153KeywordsPreserved(t, opened)
				return
			}
			if got, err := opened.GetMailboxAttributes(t.Context(), "alice-folder"); err != nil || !reflect.DeepEqual(got, []keywords.MailboxAttribute{keywords.MailboxSnoozed}) {
				t.Fatalf("split mailbox attributes = %#v, %v", got, err)
			}
			if got, err := opened.GetIMAPMessageKeywords(t.Context(), "alice-folder", 1); err != nil || !reflect.DeepEqual(got, []string{"$MailFlagBit1"}) {
				t.Fatalf("split IMAP keywords = %#v, %v", got, err)
			}
			if got, err := opened.GetHighestModSeq(t.Context(), "alice-folder"); err != nil || got != 55 {
				t.Fatalf("split MODSEQ = %d, %v", got, err)
			}
			var count int
			if err := opened.Read().QueryRow(`SELECT count(*) FROM message_provider_keywords WHERE keywords = '["$notify"]' AND fetched = 1`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("split copy count = %d, %v", count, err)
			}
			var labels string
			if err := opened.Read().QueryRow(`SELECT group_concat(id) FROM retired_keyword_labels`).Scan(&labels); err != nil || labels != "old-alice-label" {
				t.Fatalf("split retired labels = %q, %v", labels, err)
			}
		})
	}
}

func TestIMAPKeywordSnapshotsFollowFolderAndUID(t *testing.T) {
	db := sharedMigrationFixture(t)
	ctx := t.Context()
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "other", AccountID: "alice-mail", Name: "Other", RemoteID: "Other"}}); err != nil {
		t.Fatal(err)
	}
	first := SyncMessage{AccountID: "alice-mail", FolderID: "alice-folder", MessageID: "<alice@test>", Subject: "Fictional mail", RemoteUID: 1, DateSent: time.Now(), KeywordSnapshot: []string{"$MailFlagBit0", "$Vendor"}}
	second := first
	second.FolderID, second.RemoteUID, second.KeywordSnapshot = "other", 3, []string{"$MailFlagBit1"}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{first, second}); err != nil {
		t.Fatal(err)
	}
	assertSnapshot := func(folder string, uid uint32, want []string) {
		t.Helper()
		if got, err := db.GetIMAPMessageKeywords(ctx, folder, uid); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s/%d keywords = %#v, %v, want %#v", folder, uid, got, err, want)
		}
	}
	second.KeywordSnapshot = nil
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{second}); err != nil {
		t.Fatal(err)
	}
	assertSnapshot("other", 3, []string{"$MailFlagBit1"})
	changed, err := db.BatchUpdateFlags(ctx, "alice-folder", []FlagUpdate{{UID: 1, KeywordSnapshot: []string{"$Vendor", "$MailFlagBit2"}}})
	if err != nil || changed != 1 {
		t.Fatalf("keyword-only change count = %d, %v", changed, err)
	}
	assertSnapshot("alice-folder", 1, []string{"$MailFlagBit2", "$Vendor"})
	assertSnapshot("other", 3, []string{"$MailFlagBit1"})
	if _, err := db.BatchUpdateFlags(ctx, "other", []FlagUpdate{{UID: 3, KeywordSnapshot: []string{}}}); err != nil {
		t.Fatal(err)
	}
	assertSnapshot("other", 3, []string{})
	if err := db.ResetFolderUIDState(ctx, "other", 123); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetIMAPMessageKeywords(ctx, "other", 3); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old UID snapshot survived UIDVALIDITY reset: %v", err)
	}
}
