package storage

import (
	"context"
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

// Avatar times are compared with SQLite's clock, which is UTC. On a server in
// another time zone, a picture is still looked up again when its time comes,
// not hours early or late.
func TestSenderAvatarTimesIgnoreServerTimeZone(t *testing.T) {
	ctx := context.Background()
	serverZone := time.Local
	t.Cleanup(func() { time.Local = serverZone })
	for _, zone := range []*time.Location{time.FixedZone("UTC-7", -7*60*60), time.FixedZone("UTC+9", 9*60*60)} {
		time.Local = zone
		db := newContactsTestDB(t)
		now := time.Now()
		senders := []struct {
			hash string
			due  bool
			save func(hash, email string) error
		}{
			{"found-in-an-hour", false, func(hash, email string) error {
				return db.SaveSenderAvatarFound(ctx, hash, email, "domain_icon", "image/png", "", []byte("png"), now.Add(time.Hour), "missing", "missing")
			}},
			{"found-an-hour-ago", true, func(hash, email string) error {
				return db.SaveSenderAvatarFound(ctx, hash, email, "gravatar", "image/png", "", []byte("png"), now.Add(-time.Hour), "found", "missing")
			}},
			{"missing-in-an-hour", false, func(hash, email string) error {
				return db.SaveSenderAvatarMissing(ctx, hash, email, "none", now.Add(time.Hour), "missing", "missing")
			}},
			{"missing-an-hour-ago", true, func(hash, email string) error {
				return db.SaveSenderAvatarMissing(ctx, hash, email, "none", now.Add(-time.Hour), "missing", "missing")
			}},
			{"error-in-an-hour", false, func(hash, email string) error {
				return db.SaveSenderAvatarError(ctx, hash, email, "gravatar", "timeout", now.Add(time.Hour), "error", "missing")
			}},
			{"error-an-hour-ago", true, func(hash, email string) error {
				return db.SaveSenderAvatarError(ctx, hash, email, "gravatar", "timeout", now.Add(-time.Hour), "error", "missing")
			}},
		}
		for i, sender := range senders {
			if err := sender.save(sender.hash, fmt.Sprintf("sender%d@brand.example", i)); err != nil {
				t.Fatalf("%s: save %s: %v", zone, sender.hash, err)
			}
		}
		candidates, err := db.GetDueSenderAvatarCandidates(ctx, 100)
		if err != nil {
			t.Fatalf("%s: GetDueSenderAvatarCandidates() error = %v", zone, err)
		}
		due := map[string]bool{}
		for _, candidate := range candidates {
			due[candidate.EmailHash] = true
		}
		for _, sender := range senders {
			if due[sender.hash] != sender.due {
				t.Errorf("%s: %s due = %v, want %v", zone, sender.hash, due[sender.hash], sender.due)
			}
		}

		// Another sender at the same company reuses its icon while it is fresh.
		reused, err := db.GetReusableDomainIconAvatar(ctx, "colleague", "alex@brand.example")
		if err != nil || reused == nil || reused.EmailHash != "found-in-an-hour" {
			t.Errorf("%s: GetReusableDomainIconAvatar() = %+v, %v; want the icon that expires in an hour", zone, reused, err)
		}

		// Only failed lookups inside the window count.
		if _, err := db.Write().ExecContext(ctx, `INSERT INTO avatar_attempt_logs (email_hash, email, provider, status, message, created_at) VALUES ('looked-up', 'sam@brand.example', 'gravatar', 'missing', '', ?)`,
			now.Add(-2*time.Hour).UTC().Format("2006-01-02 15:04:05")); err != nil {
			t.Fatal(err)
		}
		for _, window := range []struct {
			since time.Duration
			want  int
		}{{time.Hour, 0}, {3 * time.Hour, 1}} {
			count, err := db.CountFailedSenderAvatarLookupsSince(ctx, "looked-up", now.Add(-window.since))
			if err != nil || count != window.want {
				t.Errorf("%s: failed lookups in the last %v = %d, %v; want %d", zone, window.since, count, err, window.want)
			}
		}
	}
}
