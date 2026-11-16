package storage

import (
	"context"
	"testing"
	"time"
)

func TestUnifiedFoldersHoldSharedMailboxMail(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'jmap', 'user@example.com')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "own_inbox", AccountID: "acc", RemoteID: "Inbox", ProviderRemoteID: "a", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "support_inbox", AccountID: "acc", RemoteID: "support@example.com/Inbox", ProviderRemoteID: "z:a", Name: "Inbox", Role: "custom", Selectable: true, SharedMailbox: "support@example.com", SharedRole: "inbox"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ApplySharedMailboxDiscovery(ctx, "acc", []string{"support@example.com"}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	for i, folder := range []string{"own_inbox", "support_inbox"} {
		if _, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{{
			AccountID: "acc", FolderID: folder, ProviderMessageID: folder + "-1", InternetMessageID: "<" + folder + "@example.com>",
			Subject: folder, FromEmail: "sender@example.com", DateSent: at.Add(time.Duration(i) * time.Minute), DateReceived: at.Add(time.Duration(i) * time.Minute),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RefreshAccountFolderThreadState(ctx, "acc"); err != nil {
		t.Fatal(err)
	}
	unified := func() int {
		t.Helper()
		page, err := db.GetEmailsRangeForUser(ctx, "default", "inbox", 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		return page.TotalCount
	}
	// Shared mail stays out of unified folders until the user asks for it.
	if got := unified(); got != 1 {
		t.Fatalf("unified inbox by default holds %d messages, want only the account's own", got)
	}
	if err := db.SetSetting(ctx, "default", "ui_settings", `{"shared_mailboxes_in_unified":"true"}`); err != nil {
		t.Fatal(err)
	}
	if got := unified(); got != 2 {
		t.Fatalf("unified inbox holds %d messages, want the shared mailbox's too", got)
	}
	// Its unread badge counts them too.
	if counts, err := db.GetAllFolderUnreadCounts(ctx, "default"); err != nil || counts["inbox"] != 2 {
		t.Fatalf("unified inbox unread = %d, %v; want 2", counts["inbox"], err)
	}
	// The account's own Inbox keeps only its own mail.
	if page, err := db.GetEmailsRangeForUser(ctx, "default", "own_inbox", 0, 10); err != nil || page.TotalCount != 1 {
		t.Fatalf("own inbox = %d, %v", page.TotalCount, err)
	}
	// Turned off, the shared mailbox leaves the unified folders.
	if err := db.SetSharedMailboxEnabled(ctx, "acc", "support@example.com", false); err != nil {
		t.Fatal(err)
	}
	if got := unified(); got != 1 {
		t.Fatalf("unified inbox with the shared mailbox off holds %d", got)
	}
	if err := db.SetSharedMailboxEnabled(ctx, "acc", "support@example.com", true); err != nil {
		t.Fatal(err)
	}
	// So does all shared mail when the user takes it out again.
	if err := db.SetSetting(ctx, "default", "ui_settings", `{"shared_mailboxes_in_unified":"false"}`); err != nil {
		t.Fatal(err)
	}
	if got := unified(); got != 1 {
		t.Fatalf("unified inbox with shared mail kept out holds %d", got)
	}
	if counts, err := db.GetAllFolderUnreadCounts(ctx, "default"); err != nil || counts["inbox"] != 1 {
		t.Fatalf("unified inbox unread with shared mail kept out = %d, %v; want 1", counts["inbox"], err)
	}
}

func TestSharedMailboxNames(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'jmap', 'user@example.com')`); err != nil {
		t.Fatal(err)
	}
	sharedIdentitiesOn(t, db)
	if err := db.SetSharedMailboxServerNames(ctx, "acc", map[string]string{"support@example.com": "Customer Support"}); err != nil {
		t.Fatal(err)
	}
	changes, _, err := db.ApplySharedMailboxDiscovery(ctx, "acc", []string{"support@example.com", "sales@example.com"}, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// A mailbox named ahead of its discovery is still announced as added.
	if len(changes.Added) != 2 {
		t.Fatalf("added = %v", changes.Added)
	}
	identities, err := db.ListAccountIdentitiesInternal(ctx, "acc")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, identity := range identities {
		names[identity.Email] = identity.Name
	}
	// The server's name by default, and one made from the address without.
	if names["support@example.com"] != "Customer Support" || names["sales@example.com"] != "Example Sales" {
		t.Fatalf("identity names = %v", names)
	}
	if err := db.SetSharedMailboxName(ctx, "acc", "support@example.com", "Help desk"); err != nil {
		t.Fatal(err)
	}
	mailboxes, err := db.ListSharedMailboxes(ctx, "acc")
	if err != nil {
		t.Fatal(err)
	}
	shown := map[string]string{}
	for _, m := range mailboxes {
		shown[m.Email] = m.Name
	}
	if shown["support@example.com"] != "Help desk" || shown["sales@example.com"] != "Sales" {
		t.Fatalf("mailbox names = %v", shown)
	}
	if err := db.SetSharedMailboxName(ctx, "acc", "nobody@example.com", "x"); err != ErrSharedMailboxNotFound {
		t.Fatalf("naming an unknown mailbox = %v", err)
	}
}
