package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestGetFoldersForAccountSkipsNonSelectableFolders(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{
			ID:         "acc_gmail_sent",
			AccountID:  "acc",
			ParentID:   "acc_gmail",
			RemoteID:   "[Gmail]/Sent Mail",
			Name:       "Sent",
			Icon:       "send",
			Role:       "sent",
			Selectable: true,
			SortOrder:  2,
		},
		{
			ID:         "acc_gmail",
			AccountID:  "acc",
			RemoteID:   "[Gmail]",
			Name:       "[Gmail]",
			Icon:       "folder",
			Role:       "custom",
			Selectable: false,
			SortOrder:  100,
		},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	folders, err := db.GetFoldersForAccount(ctx, "acc")
	if err != nil {
		t.Fatalf("GetFoldersForAccount() error = %v", err)
	}
	if len(folders) != 1 || folders[0].RemoteID != "[Gmail]/Sent Mail" {
		t.Fatalf("GetFoldersForAccount() = %#v, want only selectable child", folders)
	}

	var parentID sql.NullString
	if err := db.Read().QueryRowContext(ctx, `SELECT parent_id FROM folders WHERE id = 'acc_gmail_sent'`).Scan(&parentID); err != nil {
		t.Fatalf("query parent: %v", err)
	}
	if !parentID.Valid || parentID.String != "acc_gmail" {
		t.Fatalf("parent_id = %#v, want acc_gmail", parentID)
	}
}

func TestEmailQueryFilterUsesExistingMessageFields(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "inbox", AccountID: "acc", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{
		{
			AccountID: "acc",
			FolderID:  "inbox",
			RemoteUID: 1,
			MessageID: "<google@example.com>",
			Subject:   "Security alert",
			FromName:  "Google",
			FromEmail: "no-reply@google.com",
			DateSent:  time.Now(),
			Snippet:   "A new sign-in was detected",
			IsRead:    true,
		},
	}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}

	page, err := db.GetEmailsRangeFilteredForUser(ctx, "default", "inbox", 0, 50, models.EmailFilters{Query: "google"})
	if err != nil {
		t.Fatalf("GetEmailsRangeFilteredForUser() error = %v", err)
	}
	if page.TotalCount != 1 || len(page.Emails) != 1 {
		t.Fatalf("filtered page total=%d len=%d, want 1 result", page.TotalCount, len(page.Emails))
	}
}

func TestUnifiedSpamIncludesJunkRoleFolders(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc_junk', 'default', 'junk@example.com'), ('acc_spam', 'default', 'spam@example.com')`); err != nil {
		t.Fatalf("insert accounts: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_junk_junk", AccountID: "acc_junk", Name: "Junk", Role: "junk", Selectable: true},
		{ID: "acc_spam_spam", AccountID: "acc_spam", Name: "Spam", Role: "spam", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{
		{
			AccountID: "acc_junk",
			FolderID:  "acc_junk_junk",
			RemoteUID: 1,
			MessageID: "<junk@example.com>",
			Subject:   "Junk message",
			FromEmail: "sender@example.com",
			DateSent:  time.Now(),
		},
		{
			AccountID: "acc_spam",
			FolderID:  "acc_spam_spam",
			RemoteUID: 1,
			MessageID: "<spam@example.com>",
			Subject:   "Spam message",
			FromEmail: "sender@example.com",
			DateSent:  time.Now().Add(time.Minute),
		},
	}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}

	page, err := db.GetEmailsRangeFilteredForUser(ctx, "default", "spam", 0, 50, models.EmailFilters{})
	if err != nil {
		t.Fatalf("GetEmailsRangeFilteredForUser() error = %v", err)
	}
	if page.TotalCount != 2 || len(page.Emails) != 2 {
		t.Fatalf("unified spam page total=%d len=%d, want 2 messages", page.TotalCount, len(page.Emails))
	}

	counts, err := db.GetAllFolderUnreadCounts(ctx, "default")
	if err != nil {
		t.Fatalf("GetAllFolderUnreadCounts() error = %v", err)
	}
	if counts["spam"] != 2 {
		t.Fatalf("unified spam unread = %d, want 2", counts["spam"])
	}
}

func TestEmailsRangeSortsMixedTimezoneDatesByInstant(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "inbox", AccountID: "acc", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	earlierUTC := time.Date(2026, 5, 20, 12, 6, 0, 0, time.UTC)
	laterOffset := time.Date(2026, 5, 20, 12, 3, 0, 0, time.FixedZone("-0400", -4*60*60))
	if !laterOffset.After(earlierUTC) {
		t.Fatalf("test setup invalid: laterOffset must be after earlierUTC")
	}

	if err := db.UpsertSyncMessages(ctx, []SyncMessage{
		{
			AccountID: "acc",
			FolderID:  "inbox",
			RemoteUID: 1,
			MessageID: "<earlier@example.com>",
			Subject:   "Earlier UTC",
			FromName:  "Earlier",
			FromEmail: "earlier@example.com",
			DateSent:  earlierUTC,
			Snippet:   "earlier",
			IsRead:    true,
		},
		{
			AccountID: "acc",
			FolderID:  "inbox",
			RemoteUID: 2,
			MessageID: "<later@example.com>",
			Subject:   "Later offset",
			FromName:  "Later",
			FromEmail: "later@example.com",
			DateSent:  laterOffset,
			Snippet:   "later",
			IsRead:    true,
		},
	}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}

	page, err := db.GetEmailsRangeFilteredForUser(ctx, "default", "inbox", 0, 50, models.EmailFilters{})
	if err != nil {
		t.Fatalf("GetEmailsRangeFilteredForUser() error = %v", err)
	}
	if len(page.Emails) != 2 {
		t.Fatalf("len(page.Emails) = %d, want 2", len(page.Emails))
	}
	if page.Emails[0].Subject != "Later offset" {
		t.Fatalf("first subject = %q, want Later offset", page.Emails[0].Subject)
	}

	var storedReceived string
	if err := db.Read().QueryRowContext(ctx, `SELECT date_received FROM messages WHERE internet_message_id = '<later@example.com>'`).Scan(&storedReceived); err != nil {
		t.Fatalf("query stored date_received: %v", err)
	}
	if storedReceived != "2026-05-20T16:03:00Z" {
		t.Fatalf("stored date_received = %q, want UTC sortable text", storedReceived)
	}
}

func TestSaveDraftMessageStoresUTCDateText(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "drafts", AccountID: "acc", Name: "Drafts", Role: "drafts", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	draftDate := time.Date(2026, 5, 20, 9, 45, 0, 0, time.FixedZone("-0700", -7*60*60))
	if _, err := db.SaveDraftMessage(ctx, DraftMessageInput{
		AccountID:         "acc",
		FolderID:          "drafts",
		InternetMessageID: "<draft-date@example.com>",
		Subject:           "Draft date",
		FromEmail:         "user@example.com",
		Date:              draftDate,
	}); err != nil {
		t.Fatalf("SaveDraftMessage() error = %v", err)
	}

	var dateSent, dateReceived string
	if err := db.Read().QueryRowContext(ctx, `SELECT date_sent, date_received FROM messages WHERE internet_message_id = '<draft-date@example.com>'`).Scan(&dateSent, &dateReceived); err != nil {
		t.Fatalf("query stored dates: %v", err)
	}
	if dateSent != "2026-05-20T16:45:00Z" || dateReceived != "2026-05-20T16:45:00Z" {
		t.Fatalf("stored dates = %q/%q, want UTC sortable text", dateSent, dateReceived)
	}
}

func TestEmailQueryFilterMatchesThreadWhenOlderMessageMatches(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "inbox", AccountID: "acc", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	base := time.Now().Add(-time.Hour)
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{
		{
			AccountID: "acc",
			FolderID:  "inbox",
			RemoteUID: 1,
			MessageID: "<root@example.com>",
			Subject:   "Project update",
			FromName:  "Root Sender",
			FromEmail: "root@example.com",
			DateSent:  base,
			Snippet:   "needlechild appears only in the older message",
			IsRead:    true,
		},
		{
			AccountID:  "acc",
			FolderID:   "inbox",
			RemoteUID:  2,
			MessageID:  "<reply@example.com>",
			InReplyTo:  "<root@example.com>",
			References: "<root@example.com>",
			Subject:    "Re: Project update",
			FromName:   "Reply Sender",
			FromEmail:  "reply@example.com",
			DateSent:   base.Add(time.Minute),
			Snippet:    "newer thread head does not include the search term",
			IsRead:     true,
		},
	}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}

	replyID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<reply@example.com>")
	if err != nil || replyID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID(reply) = %d, %v", replyID, err)
	}
	page, err := db.GetEmailsRangeFilteredForUser(ctx, "default", "inbox", 0, 50, models.EmailFilters{Query: "needlechild"})
	if err != nil {
		t.Fatalf("GetEmailsRangeFilteredForUser() error = %v", err)
	}
	if page.TotalCount != 1 || len(page.Emails) != 1 {
		t.Fatalf("filtered page total=%d len=%d, want 1 thread", page.TotalCount, len(page.Emails))
	}
	if page.Emails[0].ID != strconv.FormatInt(replyID, 10) {
		t.Fatalf("matched email ID = %s, want newer thread head %d", page.Emails[0].ID, replyID)
	}
}

func TestEmailBodyFilterUsesMaintainedSearchIndex(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "inbox", AccountID: "acc", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
		AccountID: "acc",
		FolderID:  "inbox",
		RemoteUID: 1,
		MessageID: "<body@example.com>",
		Subject:   "Body test",
		FromName:  "Sender",
		FromEmail: "sender@example.com",
		DateSent:  time.Now(),
		Snippet:   "preview without unique body token",
		IsRead:    true,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<body@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID(body) = %d, %v", msgID, err)
	}
	bodyPath := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(bodyPath, []byte("full body includes uniquebodytoken for search"), 0600); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := db.UpdateMessageBody(ctx, msgID, bodyPath, "", "", "preview without unique body token"); err != nil {
		t.Fatalf("UpdateMessageBody() error = %v", err)
	}

	page, err := db.GetEmailsRangeFilteredForUser(ctx, "default", "inbox", 0, 50, models.EmailFilters{Body: "uniquebodytoken"})
	if err != nil {
		t.Fatalf("GetEmailsRangeFilteredForUser() error = %v", err)
	}
	if page.TotalCount != 1 || len(page.Emails) != 1 {
		t.Fatalf("body filtered page total=%d len=%d, want 1 result", page.TotalCount, len(page.Emails))
	}
}

func TestMigrateV34MarksGmailContainerNonSelectable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB() error = %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (34);
		CREATE TABLE accounts (id TEXT PRIMARY KEY, imap_host TEXT NOT NULL DEFAULT '');
		CREATE TABLE folders (id TEXT PRIMARY KEY, account_id TEXT NOT NULL, remote_id TEXT);
		INSERT INTO accounts (id, imap_host) VALUES ('acc', 'imap.gmail.com');
		INSERT INTO folders (id, account_id, remote_id) VALUES ('acc_gmail', 'acc', '[Gmail]');
	`); err != nil {
		raw.Close()
		t.Fatalf("seed v34 database: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var selectable int
	if err := db.Read().QueryRow(`SELECT selectable FROM folders WHERE id = 'acc_gmail'`).Scan(&selectable); err != nil {
		t.Fatalf("query selectable: %v", err)
	}
	if selectable != 0 {
		t.Fatalf("selectable = %d, want 0", selectable)
	}
}
