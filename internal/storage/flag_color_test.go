package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/keywords"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestIMAPFlagColorsFollowTheSelectedCopyAndPendingIntent(t *testing.T) {
	db, info := seedUserLabelTest(t, "imap")
	ctx := t.Context()
	if _, err := db.Write().Exec(`INSERT INTO message_folder_state(message_id, folder_id, remote_uid, is_starred, remote_keywords)
		VALUES(1, 'junk', 4, 1, '["$MailFlagBit0"]')`); err != nil {
		t.Fatal(err)
	}
	syncFlags := func(starred bool, snapshot ...string) {
		t.Helper()
		if _, err := db.BatchUpdateFlags(ctx, "inbox", []FlagUpdate{{UID: 2, IsStarred: starred, KeywordSnapshot: snapshot}}); err != nil {
			t.Fatal(err)
		}
	}
	assertCopy := func(folder string, starred bool, color keywords.FlagColor) {
		t.Helper()
		email, err := db.GetEmailByIDForFolderForUser(ctx, "1", folder, "alice")
		if err != nil || email == nil || email.IsStarred != starred || email.FlagColor != color {
			t.Fatalf("%s = %+v (%v), want starred %t and color %s", folder, email, err, starred, color.Key())
		}
	}
	for bits, want := range []keywords.FlagColor{
		0, keywords.FlagColorOrange, keywords.FlagColorYellow, keywords.FlagColorGreen,
		keywords.FlagColorBlue, keywords.FlagColorPurple, keywords.FlagColorGray, 0,
	} {
		snapshot := []string{"$vendor"}
		for i, name := range []string{"$MailFlagBit0", "$MAILFLAGBIT1", "$mailflagbit2"} {
			if bits&(1<<i) != 0 {
				snapshot = append(snapshot, name)
			}
		}
		syncFlags(true, snapshot...)
		assertCopy("inbox", true, want)
	}
	assertCopy("junk", true, keywords.FlagColorOrange)
	syncFlags(false, "$mailflagbit2", "$vendor")
	assertCopy("inbox", false, 0)
	if err := db.SetMessageFlagColorForUser(ctx, "bob", []ThreadMessageMutationInfo{info}, keywords.FlagColorPurple, true); err == nil {
		t.Fatal("changed a message belonging to another user")
	}
	if err := db.SetMessageFlagColorForUser(ctx, "alice", []ThreadMessageMutationInfo{info}, keywords.FlagColorPurple, true); err != nil {
		t.Fatal(err)
	}
	assertCopy("inbox", true, keywords.FlagColorPurple)
	assertCopy("junk", true, keywords.FlagColorOrange)
	claim := func() MessageMutation {
		t.Helper()
		mutations, err := db.ClaimDueMessageMutations(ctx, time.Now().Add(time.Minute), 10)
		if err != nil || len(mutations) != 1 {
			t.Fatalf("claim = %+v (%v)", mutations, err)
		}
		return mutations[0]
	}
	first := claim()
	if first.TargetFlagColor != keywords.FlagColorPurple || first.SourceUIDValidity != 42 {
		t.Fatalf("queued color or identity differs: %+v", first)
	}
	if err := db.CompleteMessageMutation(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	// An IMAP STORE can have set Flagged and only one of the desired bits.
	syncFlags(true, "$mailflagbit2", "$vendor")
	assertCopy("inbox", true, keywords.FlagColorPurple)
	if _, err := db.GetMessageMutationInternal(ctx, first.ID); err != nil {
		t.Fatalf("a partial result confirmed the edit: %v", err)
	}
	syncFlags(true, "$mailflagbit0", "$mailflagbit2", "$vendor")
	assertCopy("inbox", true, keywords.FlagColorPurple)
	if _, err := db.GetMessageMutationInternal(ctx, first.ID); err == nil {
		t.Fatal("confirmed edit remained queued")
	}
	if err := db.SetMessageFlagColorForUser(ctx, "alice", []ThreadMessageMutationInfo{info}, keywords.FlagColorPurple, false); err != nil {
		t.Fatal(err)
	}
	clear := claim()
	if bits, exact := clear.FlagBitsTarget(); !exact || bits != 0 || clear.TargetValue {
		t.Fatalf("unflag did not clear every bit: %+v", clear)
	}
	assertCopy("inbox", false, 0)
	assertCopy("junk", true, keywords.FlagColorOrange)
	if err := db.SetMessageStarredAndQueueForUser(ctx, 1, true, "alice"); err != nil {
		t.Fatal(err)
	}
	assertCopy("inbox", true, 0)
	assertCopy("junk", true, keywords.FlagColorOrange)
	plain := claim()
	if plain.ID == clear.ID || plain.TargetFlagColor != MessageMutationResetFlagColor || !plain.TargetValue {
		t.Fatalf("plain star did not supersede an in-flight clear: %+v", plain)
	}
	if err := db.CompleteMessageMutation(ctx, clear.ID); err != nil {
		t.Fatal(err)
	}
	if current, err := db.GetMessageMutationInternal(ctx, plain.ID); err != nil || current.Status != MessageMutationProcessing {
		t.Fatalf("old completion consumed newer intent: %+v (%v)", current, err)
	}
}

func TestJMAPFlagColorUsesExactFetchedEmailIDAndTypedFilters(t *testing.T) {
	db, info := seedUserLabelTest(t, "jmap")
	ctx := t.Context()
	for _, query := range []string{
		`UPDATE message_folder_state SET is_starred=1 WHERE folder_id='inbox'`,
		`INSERT INTO message_folder_state(message_id, folder_id, remote_message_id, is_starred) VALUES(1,'junk','m2',1)`,
		`INSERT INTO message_provider_keywords(message_id, remote_message_id, keywords, fetched)
			VALUES(1,'m1','["$mailflagbit2"]',1),(1,'m2','["$mailflagbit1"]',0)`,
	} {
		if _, err := db.Write().Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	for _, check := range []struct {
		folder string
		filter models.EmailFilters
		count  int
		color  keywords.FlagColor
	}{
		{"inbox", models.EmailFilters{}, 1, keywords.FlagColorBlue},
		{"junk", models.EmailFilters{}, 1, 0},
		{"inbox", models.EmailFilters{FlagColor: keywords.FlagColorBlue}, 1, keywords.FlagColorBlue},
		{"inbox", models.EmailFilters{FlagColor: keywords.FlagColorOrange}, 0, 0},
		{"inbox", models.EmailFilters{Tag: "Blue"}, 0, 0},
		{"inbox", models.EmailFilters{HasTags: true}, 1, keywords.FlagColorBlue},
		{"inbox", models.EmailFilters{NoTags: true}, 0, 0},
		{"junk", models.EmailFilters{NoTags: true}, 1, 0},
	} {
		page, err := db.GetEmailsRangeFilteredForUser(ctx, "alice", check.folder, 0, 10, check.filter)
		if err != nil || page.TotalCount != check.count || len(page.Emails) != check.count {
			t.Fatalf("folder %s with %+v = %+v (%v)", check.folder, check.filter, page, err)
		}
		if check.count > 0 && page.Emails[0].FlagColor != check.color {
			t.Fatalf("folder %s color = %v, want %v", check.folder, page.Emails[0].FlagColor, check.color)
		}
	}
	scope := models.MailSearchScope{Kind: models.MailSearchAll}
	page, err := db.GetMailSearchPageForUser(ctx, "alice", scope, 0, 10, models.EmailFilters{FlagColor: keywords.FlagColorBlue})
	if err != nil || len(page.Emails) != 1 || page.Emails[0].FlagColor != keywords.FlagColorBlue || page.Emails[0].FolderID != "inbox" {
		t.Fatalf("colored search = %+v (%v)", page, err)
	}
	if err := db.SetMessageFlagColorForUser(ctx, "alice", []ThreadMessageMutationInfo{info}, keywords.FlagColorGreen, true); err != nil {
		t.Fatal(err)
	}
	after, err := db.GetEmailByIDForFolderForUser(ctx, "1", "junk", "alice")
	if err != nil || after.FlagColor != 0 || !after.IsStarred {
		t.Fatalf("the other JMAP copy changed: %+v (%v)", after, err)
	}
	// Even if the displayed copy in the folder changes, replay must keep
	// the original JMAP Email ID. Its keyword snapshot is still separate.
	if _, err := db.Write().Exec(`UPDATE message_folder_state SET remote_message_id='m2' WHERE folder_id='inbox'`); err != nil {
		t.Fatal(err)
	}
	mutations, err := db.ClaimDueMessageMutations(ctx, time.Now().Add(time.Minute), 10)
	if err != nil || len(mutations) != 1 {
		t.Fatalf("queued color edits = %+v (%v)", mutations, err)
	}
	replay, err := db.GetMessageMutationInfoForReplay(ctx, mutations[0])
	if err != nil || replay == nil || replay.RemoteMessageID != "m1" || len(replay.FolderCopyIDs) != 0 {
		t.Fatalf("replay target after copy changed = %+v (%v)", replay, err)
	}
}

func TestFlagColorEditNeverFollowsAnInvalidatedIMAPUID(t *testing.T) {
	db, info := seedUserLabelTest(t, "imap")
	ctx := t.Context()
	// Keep the logical message alive elsewhere while the Inbox UIDs reset.
	if _, err := db.Write().Exec(`INSERT INTO message_folder_state(message_id, folder_id, remote_uid) VALUES (1,'junk',4)`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMessageFlagColorForUser(ctx, "alice", []ThreadMessageMutationInfo{info}, keywords.FlagColorOrange, true); err != nil {
		t.Fatal(err)
	}
	if err := db.ResetFolderUIDState(ctx, "inbox", 43); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
		AccountID: "acc", FolderID: "inbox", RemoteUID: 2, MessageID: "<label@test.invalid>",
		Subject: "Another copy", KeywordSnapshot: []string{},
	}}); err != nil {
		t.Fatal(err)
	}
	mutations, err := db.ClaimDueMessageMutations(ctx, time.Now().Add(time.Minute), 10)
	if err != nil || len(mutations) != 0 {
		t.Fatalf("invalidated color still targets a UID: %+v (%v)", mutations, err)
	}
	current, err := db.GetMessageMutationInfoInFolder(ctx, 1, "inbox")
	if err != nil || current == nil || current.IsStarred || current.FlagColor.Valid() {
		t.Fatalf("the rebuilt copy inherited an obsolete color: %+v (%v)", current, err)
	}
}

func TestFlagColorRuleCanRemoveColorThenSetAnOrdinaryStar(t *testing.T) {
	db, _ := seedUserLabelTest(t, "imap")
	ctx := t.Context()
	if _, err := db.BatchUpdateFlags(ctx, "inbox", []FlagUpdate{{UID: 2, IsStarred: true, KeywordSnapshot: []string{"$MailFlagBit2"}}}); err != nil {
		t.Fatal(err)
	}
	_, err := db.SaveMailRuleGuarded(ctx, "alice", models.MailRule{
		Name: "Keep starred without its old color", Enabled: true, MatchAll: true,
		Conditions: []models.MailRuleCondition{{Field: models.MailRuleFieldFlagColor, Operator: models.MailRuleOpIs, Value: "blue"}},
		Actions: []models.MailRuleAction{
			{Kind: models.MailRuleActionRemoveFlagColor, FlagColor: "blue"},
			{Kind: models.MailRuleActionStar},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := db.newMailRuleRunner(ctx, "acc", nil)
	if err != nil || runner == nil {
		t.Fatalf("runner: %v (%v)", runner, err)
	}
	outcome, err := runner.run(ctx, 1, "inbox", models.MailRuleTriggerManual)
	if err != nil || !outcome.Changed() {
		t.Fatalf("rule outcome: %+v (%v)", outcome, err)
	}
	info, err := db.GetMessageMutationInfoInFolder(ctx, 1, "inbox")
	if err != nil || info == nil || !info.IsStarred || info.FlagColor != 0 {
		t.Fatalf("rule left %+v (%v), want a plain star", info, err)
	}
}

// useSharedSchemaBeforeFlagColors restores the queue shape from schema 155.
func useSharedSchemaBeforeFlagColors(t *testing.T, db *DB) {
	t.Helper()
	migrationFixtureSQL(t, db, `ALTER TABLE message_mutations DROP COLUMN target_flag_color;
		ALTER TABLE message_mutations DROP COLUMN target_flag_copy;
		DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(155)`)
}

func TestFlagColorMigrationPreservesQueuedFlags(t *testing.T) {
	db := sharedMigrationFixture(t)
	useSharedSchemaBeforeFlagColors(t, db)
	migrationFixtureSQL(t, db, `
		INSERT INTO message_mutations(id, account_id, message_id, folder_id, provider_type, kind, target_value)
		SELECT 'flag-mutation', 'alice-mail', id, 'alice-folder', 'imap', 'starred', 1 FROM messages WHERE account_id='alice-mail'`)
	for range 2 {
		if err := migrateV155ToV156(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	m, err := db.GetMessageMutationInternal(t.Context(), "flag-mutation")
	if err != nil || !m.TargetValue || m.TargetFlagColor != 0 || m.TargetFlagCopy != "" {
		t.Fatalf("legacy mutation = %+v (%v)", m, err)
	}
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 156 {
		t.Fatalf("upgraded schema = %d (%v), want 156", version, err)
	}
}

func TestUserStorageCopyPreservesFlagColorQueueAcrossSchemaVersions(t *testing.T) {
	for _, version := range []int{154, 155, CurrentSchemaVersion} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			ctx := t.Context()
			source := sharedMigrationFixture(t)
			migrationFixtureSQL(t, source, `
				UPDATE accounts SET provider='jmap' WHERE id IN ('alice-mail', 'bob-mail');
				INSERT INTO message_mutations(id, account_id, message_id, folder_id, provider_type, kind, target_value)
				SELECT account_id || '-flag', account_id, id, replace(account_id, '-mail', '-folder'), 'jmap', 'starred', 1
				FROM messages WHERE account_id IN ('alice-mail', 'bob-mail');
				UPDATE calendar_sources SET chosen_color='#d97706' WHERE id='alice-calendar'`)
			var wantColor keywords.FlagColor
			wantCopy := ""
			wantCalendarColor := "#d97706"
			if version < 156 {
				useSharedSchemaBeforeFlagColors(t, source)
			} else {
				migrationFixtureSQL(t, source, `
					UPDATE message_mutations SET target_flag_color=5, target_flag_copy=account_id || '-email'`)
				wantColor, wantCopy = keywords.FlagColorPurple, "alice-mail-email"
			}
			if version < 155 {
				migrationFixtureSQL(t, source, `ALTER TABLE calendar_sources DROP COLUMN chosen_color;
					DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(154)`)
				wantCalendarColor = ""
			}

			report := migrationCopyReport(t, source)
			if report.SchemaVersion != version || report.TargetSchemaVersion != CurrentSchemaVersion {
				t.Fatalf("source schema = %d, target schema = %d", report.SchemaVersion, report.TargetSchemaVersion)
			}
			dest := migrationCopyDestination(t, "alice")
			if err := copyUserStorageMigrationRows(ctx, source.Path(), dest, report, "alice"); err != nil {
				t.Fatal(err)
			}
			m, err := dest.GetMessageMutationInternal(ctx, "alice-mail-flag")
			if err != nil || !m.TargetValue || m.TargetFlagColor != wantColor || m.TargetFlagCopy != wantCopy {
				t.Fatalf("copied mutation = %+v (%v), want a star with color %d and copy %q", m, err, wantColor, wantCopy)
			}
			if _, err := dest.GetMessageMutationInternal(ctx, "bob-mail-flag"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("another owner's mutation was copied: %v", err)
			}
			var calendarColor string
			if err := dest.Read().QueryRow(`SELECT chosen_color FROM calendar_sources WHERE id='alice-calendar'`).Scan(&calendarColor); err != nil || calendarColor != wantCalendarColor {
				t.Fatalf("copied calendar color = %q (%v), want %q", calendarColor, err, wantCalendarColor)
			}
		})
	}
}

func TestRetiredFlagColorReferenceKeepsGenuineNameCollision(t *testing.T) {
	db, info := seedUserLabelTest(t, "imap")
	ctx := t.Context()
	_, err := db.Write().Exec(`INSERT INTO retired_keyword_labels(id, account_id, name, provider_id)
	VALUES('retired-blue', 'acc', 'Blue', '$MailFlagBit2')`)
	if err != nil {
		t.Fatal(err)
	}
	check := func(name, id string, want bool) {
		t.Helper()
		got, err := db.RetiredFlagLabelReference(ctx, "alice", "acc", name, id, "")
		if err != nil || got != want {
			t.Fatalf("retired %s/%s = %t (%v), want %t", name, id, got, err, want)
		}
	}
	check("Blue", "", true)
	check("Blue", "retired-blue", true)
	check("$MAILFLAGBIT2", "", true)
	if _, err := db.Write().Exec(`INSERT INTO label_aliases (account_id, provider_type, provider_id, display_name, source)
		VALUES (?, ?, ?, ?, 'user')`, "acc", LabelProviderKeyword, "$MailFlagBit1", "Old yellow"); err != nil {
		t.Fatal(err)
	}
	check("Old yellow", "", true)
	if err := db.QueueUserLabels(ctx, "alice", []ThreadMessageMutationInfo{info}, "Blue", LabelMutationAdd); err != nil {
		t.Fatal(err)
	}
	check("Blue", "", false)
	check("Blue", "retired-blue", true)
	if got, err := db.RetiredFlagLabelReference(ctx, "bob", "acc", "Blue", "retired-blue", ""); got || err != nil {
		t.Fatalf("another owner could see retired label: %t (%v)", got, err)
	}
}
