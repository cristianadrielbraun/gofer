package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func TestIdleFolderIDsForAccountSupportsFolderIDsAndLegacyRoles(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_sent", AccountID: "acc", RemoteID: "Sent", Name: "Sent", Role: "sent", Selectable: true},
		{ID: "acc_gmail_sent", AccountID: "acc", RemoteID: "[Gmail]/Sent Mail", Name: "Sent", Role: "sent", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	if err := db.SetIdleFoldersAll(ctx, "default", map[string][]string{
		" acc ": {"acc_sent", "acc_sent", "sent", "", "none"},
	}); err != nil {
		t.Fatalf("SetIdleFoldersAll() error = %v", err)
	}

	var raw string
	if err := db.Read().QueryRowContext(ctx, `SELECT value FROM app_settings WHERE user_id = 'default' AND key = 'idle_folders'`).Scan(&raw); err != nil {
		t.Fatalf("query idle_folders: %v", err)
	}

	var stored map[string][]string
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatalf("decode idle_folders: %v", err)
	}

	if got := strings.Join(stored["acc"], ","); got != "acc_sent,sent" {
		t.Fatalf("stored acc entries = %q, want acc_sent,sent", got)
	}

	ids := db.GetIdleFolderIDsForAccount(ctx, "default", "acc")
	for _, want := range []string{"acc_sent", "acc_gmail_sent"} {
		if !ids[want] {
			t.Fatalf("idle folder ids = %#v, missing %s", ids, want)
		}
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

func TestMutationInfoForFolderUsesActiveUnifiedRole(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_junk", AccountID: "acc", Name: "Junk", Role: "junk", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	for _, msg := range []SyncMessage{
		{AccountID: "acc", FolderID: "acc_inbox", RemoteUID: 11, MessageID: "<shared@example.com>", Subject: "Shared", FromEmail: "sender@example.com", DateSent: time.Now()},
		{AccountID: "acc", FolderID: "acc_junk", RemoteUID: 22, MessageID: "<shared@example.com>", Subject: "Shared", FromEmail: "sender@example.com", DateSent: time.Now()},
	} {
		if err := db.UpsertSyncMessages(ctx, []SyncMessage{msg}); err != nil {
			t.Fatalf("UpsertSyncMessages(%s) error = %v", msg.FolderID, err)
		}
	}

	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<shared@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID(shared) = %d, %v", msgID, err)
	}
	if _, err := db.Write().ExecContext(ctx, `UPDATE messages SET thread_id = 'thread-shared' WHERE id = ?`, msgID); err != nil {
		t.Fatalf("set thread id: %v", err)
	}

	info, err := db.GetMessageMutationInfoForFolder(ctx, msgID, "spam")
	if err != nil {
		t.Fatalf("GetMessageMutationInfoForFolder(spam) error = %v", err)
	}
	if info == nil || info.FolderID != "acc_junk" || info.RemoteUID != 22 {
		t.Fatalf("spam mutation info = %#v, want junk folder UID 22", info)
	}

	info, err = db.GetMessageMutationInfoForFolder(ctx, msgID, "acc_inbox")
	if err != nil {
		t.Fatalf("GetMessageMutationInfoForFolder(acc_inbox) error = %v", err)
	}
	if info == nil || info.FolderID != "acc_inbox" || info.RemoteUID != 11 {
		t.Fatalf("inbox mutation info = %#v, want inbox folder UID 11", info)
	}

	infos, err := db.GetThreadMutationInfosForFolder(ctx, "acc", "thread-shared", "spam")
	if err != nil {
		t.Fatalf("GetThreadMutationInfosForFolder(spam) error = %v", err)
	}
	if len(infos) != 1 || infos[0].FolderID != "acc_junk" || infos[0].RemoteUID != 22 {
		t.Fatalf("spam thread mutation infos = %#v, want junk folder UID 22", infos)
	}
}

func TestUnifiedFolderAccountSettingsFilterMailAndUnread(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc_a', 'default', 'a@example.com'), ('acc_b', 'default', 'b@example.com')`); err != nil {
		t.Fatalf("insert accounts: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_a_inbox", AccountID: "acc_a", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_b_inbox", AccountID: "acc_b", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{
		{
			AccountID: "acc_a",
			FolderID:  "acc_a_inbox",
			RemoteUID: 1,
			MessageID: "<included@example.com>",
			Subject:   "Included account",
			FromEmail: "sender@example.com",
			DateSent:  time.Now(),
		},
		{
			AccountID: "acc_b",
			FolderID:  "acc_b_inbox",
			RemoteUID: 1,
			MessageID: "<excluded@example.com>",
			Subject:   "Excluded account",
			FromEmail: "sender@example.com",
			DateSent:  time.Now().Add(time.Minute),
		},
	}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}

	settings := db.GetUISettings(ctx, "default")
	settings[unifiedFolderAccountSettingKey("inbox", "acc_b")] = "false"
	if err := db.SetUISettings(ctx, "default", settings); err != nil {
		t.Fatalf("SetUISettings() error = %v", err)
	}

	page, err := db.GetEmailsRangeFilteredForUser(ctx, "default", "inbox", 0, 50, models.EmailFilters{})
	if err != nil {
		t.Fatalf("GetEmailsRangeFilteredForUser() error = %v", err)
	}
	if page.TotalCount != 1 || len(page.Emails) != 1 {
		t.Fatalf("unified inbox page total=%d len=%d, want only included account", page.TotalCount, len(page.Emails))
	}
	if page.Emails[0].AccountID != "acc_a" || page.Emails[0].Subject != "Included account" {
		t.Fatalf("unified inbox returned %#v, want acc_a included message", page.Emails[0])
	}

	counts, err := db.GetAllFolderUnreadCounts(ctx, "default")
	if err != nil {
		t.Fatalf("GetAllFolderUnreadCounts() error = %v", err)
	}
	if counts["inbox"] != 1 {
		t.Fatalf("unified inbox unread = %d, want only included account", counts["inbox"])
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

func TestAddMessageToFolderWithoutRemoteUIDAllowsMultipleUnknownUIDs(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "spam", AccountID: "acc", Name: "Spam", Role: "junk", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO messages (id, account_id, internet_message_id, subject, from_email)
		VALUES (1, 'acc', '<one@example.com>', 'one', 'sender@example.com'),
		       (2, 'acc', '<two@example.com>', 'two', 'sender@example.com')`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	if err := db.AddMessageToFolderWithoutRemoteUID(ctx, 1, "spam", true, false); err != nil {
		t.Fatalf("AddMessageToFolderWithoutRemoteUID(1) error = %v", err)
	}
	if err := db.AddMessageToFolderWithoutRemoteUID(ctx, 2, "spam", false, true); err != nil {
		t.Fatalf("AddMessageToFolderWithoutRemoteUID(2) error = %v", err)
	}

	var count, nullUIDs int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*), SUM(CASE WHEN remote_uid IS NULL THEN 1 ELSE 0 END) FROM message_folder_state WHERE folder_id = 'spam'`).Scan(&count, &nullUIDs); err != nil {
		t.Fatalf("query folder state: %v", err)
	}
	if count != 2 || nullUIDs != 2 {
		t.Fatalf("folder state count=%d nullUIDs=%d, want 2 and 2", count, nullUIDs)
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
