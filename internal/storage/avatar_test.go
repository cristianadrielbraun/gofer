package storage

import (
	"fmt"
	"testing"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
)

func TestSenderAvatarSnapshotFiltersCacheLogsAndStats(t *testing.T) {
	db := newContactsTestDB(t)
	ctx := t.Context()
	for _, email := range []string{"alice@example.com", "bob@example.com", "orphan@example.com"} {
		hash := avatarresolver.GravatarHash(email)
		if err := db.SaveSenderAvatarFound(ctx, hash, email, "gravatar", "image/png", "", []byte("png"), time.Now().Add(time.Hour), "found", "missing"); err != nil {
			t.Fatal(err)
		}
		if err := db.RecordSenderAvatarAttempt(ctx, hash, email, "gravatar", "error", "provider timeout"); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name   string
		emails []string
		total  int
	}{
		{"global", nil, 3}, {"empty", []string{}, 0},
		{"selected", []string{"alice@example.com", "bob@example.com"}, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows, total, err := db.GetSenderAvatarRows(ctx, SenderAvatarRowFilter{VisibleEmails: test.emails, Status: "found", Source: "gravatar", Provider: "gravatar", Query: "example", Limit: 1, Offset: 1})
			wantRows := 0
			if test.total > 1 {
				wantRows = 1
			}
			if err != nil || total != test.total || len(rows) != wantRows {
				t.Fatal("rows", rows, total, err)
			}
			logs, total, err := db.GetSenderAvatarAttemptLogs(ctx, SenderAvatarAttemptLogFilter{VisibleEmails: test.emails, Provider: "gravatar", Status: "error", ErrorsOnly: true, Query: "example", Limit: 1, Offset: 1})
			if err != nil || total != test.total || len(logs) != wantRows {
				t.Fatal("logs", logs, total, err)
			}
			var stats SenderAvatarStats
			if test.emails == nil {
				stats, err = db.GetSenderAvatarStats(ctx)
			} else {
				stats, err = db.GetSenderAvatarStatsForEmails(ctx, test.emails)
			}
			if err != nil || stats.Total != test.total || stats.Found != test.total || stats.GravatarChecked != test.total || stats.BIMIChecked != test.total {
				t.Fatal("stats", stats, err)
			}
		})
	}
	// Large snapshots use one JSON parameter rather than one bind per sender.
	emails := make([]string, 40000)
	for i := range emails {
		emails[i] = fmt.Sprintf("sender-%d@example.com", i)
	}
	emails[len(emails)-1] = "alice@example.com"
	rows, total, err := db.GetSenderAvatarRows(ctx, SenderAvatarRowFilter{UserID: "default", VisibleEmails: emails})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].Email != "alice@example.com" {
		t.Fatal("large snapshot", total, rows, err)
	}
	stats, err := db.GetSenderAvatarStatsForEmails(ctx, nil)
	if err != nil || stats.Total != 0 {
		t.Fatal("nil explicit snapshot exposed global stats", stats, err)
	}
}

func TestSenderAvatarSnapshotPreservesLegacyVisibility(t *testing.T) {
	db := newContactsTestDB(t)
	ctx := t.Context()
	if _, err := db.Write().ExecContext(ctx, `
 INSERT INTO accounts(id,user_id,email_address,is_deleting) VALUES
 ('active','default','active@example.com',0),('deleting','default','deleting@example.com',1);
 INSERT INTO messages(account_id,from_email) VALUES ('active',' Message@Example.com '),('deleting','excluded@example.com');
 INSERT INTO contact_profiles(id,user_id,primary_email,is_deleted) VALUES
 ('active','default',' Primary@Example.com ',0),('deleted','default','deleted@example.com',1);
 INSERT INTO contact_identities(user_id,profile_id,kind,normalized_value) VALUES
 ('default','active','email','alias@example.com'),('default','active','email',' Unexpected@Example.com '),
 ('default','deleted','email','deleted-alias@example.com');`); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"message@example.com", "primary@example.com", "alias@example.com", "unexpected@example.com", "excluded@example.com", "deleted@example.com", "deleted-alias@example.com", "orphan@example.com"} {
		if err := db.SaveSenderAvatarMissing(ctx, avatarresolver.GravatarHash(email), email, "none", time.Now().Add(time.Hour), "missing", "missing"); err != nil {
			t.Fatal(err)
		}
	}
	emails, err := db.listAdminAvatarEmails(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	old, oldTotal, err := db.GetSenderAvatarRows(ctx, SenderAvatarRowFilter{UserID: "default"})
	if err != nil {
		t.Fatal(err)
	}
	current, currentTotal, err := db.GetSenderAvatarRows(ctx, SenderAvatarRowFilter{VisibleEmails: emails})
	if err != nil || currentTotal != 3 || oldTotal != currentTotal || len(old) != len(current) {
		t.Fatal("visibility parity", oldTotal, currentTotal, err)
	}
	for i := range old {
		if old[i].Email != current[i].Email {
			t.Fatal("visibility parity", old, current)
		}
	}
}

func TestGetSenderAvatarUserIDsUsesActiveAccountOwnership(t *testing.T) {
	ctx := t.Context()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users (id, username, username_normalized, name) VALUES ('user-b', 'user-b', 'user-b', 'User B');
		INSERT INTO accounts (id, user_id, email_address) VALUES ('account-a', 'default', 'a@example.com');
		INSERT INTO accounts (id, user_id, email_address, is_deleting) VALUES ('account-b', 'user-b', 'b@example.com', 1);
	`); err != nil {
		t.Fatalf("seed users and accounts: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "inbox-a", AccountID: "account-a", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "inbox-b", AccountID: "account-b", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	for _, message := range []SyncMessage{
		{AccountID: "account-a", FolderID: "inbox-a", RemoteUID: 1, MessageID: "<a@example.com>", FromEmail: "Sender@Example.com", DateSent: time.Now()},
		{AccountID: "account-b", FolderID: "inbox-b", RemoteUID: 1, MessageID: "<b@example.com>", FromEmail: "sender@example.com", DateSent: time.Now()},
	} {
		if err := db.UpsertSyncMessages(ctx, []SyncMessage{message}); err != nil {
			t.Fatalf("UpsertSyncMessages() error = %v", err)
		}
	}
	userIDs, err := db.GetSenderAvatarUserIDs(ctx, " sender@example.com ")
	if err != nil {
		t.Fatalf("GetSenderAvatarUserIDs() error = %v", err)
	}
	if len(userIDs) != 1 || userIDs[0] != "default" {
		t.Fatalf("GetSenderAvatarUserIDs() = %#v, want only active owner", userIDs)
	}
}
