package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

func TestGetFoldersForAccountSkipsGmailLabelBackedFolders(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'gmail', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_archive", AccountID: "acc", RemoteID: "[Gmail]/All Mail", ProviderRemoteID: "ARCHIVE", Name: "Archive", Role: "archive", Selectable: true},
		{ID: "acc_important", AccountID: "acc", RemoteID: "[Gmail]/Important", ProviderRemoteID: "IMPORTANT", Name: "[Gmail]/Important", Role: "custom", Selectable: true},
		{ID: "acc_category", AccountID: "acc", RemoteID: "CATEGORY_FORUMS", ProviderRemoteID: "CATEGORY_FORUMS", Name: "CATEGORY_FORUMS", Role: "custom", Selectable: true},
		{ID: "acc_label", AccountID: "acc", RemoteID: "Projects", ProviderRemoteID: "Label_Projects", Name: "Projects", Role: "custom", Selectable: true},
		{ID: "acc_imap_label", AccountID: "acc", RemoteID: "[Imap]/Trash", ProviderRemoteID: "Label_ImapTrash", Name: "[Imap]/Trash", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	folders, err := db.GetFoldersForAccount(ctx, "acc")
	if err != nil {
		t.Fatalf("GetFoldersForAccount() error = %v", err)
	}
	byProvider := map[string]bool{}
	for _, folder := range folders {
		byProvider[folder.ProviderRemoteID] = true
	}
	for _, want := range []string{"INBOX", "ARCHIVE"} {
		if !byProvider[want] {
			t.Fatalf("GetFoldersForAccount() missing %s: %#v", want, folders)
		}
	}
	for _, hidden := range []string{"IMPORTANT", "CATEGORY_FORUMS", "Label_Projects", "Label_ImapTrash"} {
		if byProvider[hidden] {
			t.Fatalf("GetFoldersForAccount() included Gmail label %s: %#v", hidden, folders)
		}
	}

	accounts, err := db.GetAccounts(ctx, "default")
	if err != nil {
		t.Fatalf("GetAccounts() error = %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("GetAccounts() = %#v, want one account", accounts)
	}
	for _, folder := range accounts[0].Folders {
		switch folder.ID {
		case "acc_inbox", "acc_archive":
		default:
			t.Fatalf("sidebar folder %s/%s rendered from Gmail label rows: %#v", folder.ID, folder.Name, accounts[0].Folders)
		}
	}
}

func TestUpsertFoldersPreservesRemoteIDWhenProviderRemoteIDChanges(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{
		ID:               "acc_inbox",
		AccountID:        "acc",
		RemoteID:         "INBOX",
		ProviderRemoteID: "graph-inbox-1",
		Name:             "Inbox",
		Role:             "inbox",
		Selectable:       true,
	}}); err != nil {
		t.Fatalf("UpsertFolders(graph) error = %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{
		ID:         "acc_inbox",
		AccountID:  "acc",
		RemoteID:   "INBOX",
		Name:       "Inbox",
		Role:       "inbox",
		Selectable: true,
	}}); err != nil {
		t.Fatalf("UpsertFolders(imap) error = %v", err)
	}

	folders, err := db.GetFoldersForAccount(ctx, "acc")
	if err != nil {
		t.Fatalf("GetFoldersForAccount() error = %v", err)
	}
	if len(folders) != 1 {
		t.Fatalf("folders = %#v, want one folder", folders)
	}
	if folders[0].RemoteID != "INBOX" || folders[0].ProviderRemoteID != "graph-inbox-1" {
		t.Fatalf("folder identity = remote %q provider %q, want INBOX/graph-inbox-1", folders[0].RemoteID, folders[0].ProviderRemoteID)
	}
}

func TestGetFoldersForAccountCountsProviderBackedMessagesSeparately(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'outlook', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{
		ID:               "acc_inbox",
		AccountID:        "acc",
		RemoteID:         "Inbox",
		ProviderRemoteID: "graph-inbox",
		Name:             "Inbox",
		Role:             "inbox",
		Selectable:       true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
		AccountID: "acc",
		FolderID:  "acc_inbox",
		RemoteUID: 100,
		MessageID: "<legacy@example.com>",
		Subject:   "Legacy",
		FromEmail: "sender@example.com",
		DateSent:  time.Now(),
		IsRead:    true,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	if _, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "graph-message-1",
		InternetMessageID: "<graph@example.com>",
		Subject:           "Graph",
		FromEmail:         "sender@example.com",
		DateSent:          time.Now(),
		IsRead:            true,
	}}); err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}

	folders, err := db.GetFoldersForAccount(ctx, "acc")
	if err != nil {
		t.Fatalf("GetFoldersForAccount() error = %v", err)
	}
	if len(folders) != 1 {
		t.Fatalf("GetFoldersForAccount() = %#v, want one folder", folders)
	}
	if folders[0].LocalMessageCount != 2 || folders[0].ProviderMessageCount != 1 {
		t.Fatalf("folder counts local=%d provider=%d, want local 2 and provider-backed 1", folders[0].LocalMessageCount, folders[0].ProviderMessageCount)
	}
}

func TestUpsertFoldersPersistsKnownProviderCounts(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'outlook', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{
		ID:               "acc_inbox",
		AccountID:        "acc",
		RemoteID:         "Inbox",
		ProviderRemoteID: "graph-inbox",
		Name:             "Inbox",
		Role:             "inbox",
		Selectable:       true,
		CountsKnown:      true,
		TotalCount:       2000,
		UnreadCount:      1089,
	}}); err != nil {
		t.Fatalf("UpsertFolders(counts known) error = %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{
		ID:               "acc_inbox",
		AccountID:        "acc",
		RemoteID:         "Inbox",
		ProviderRemoteID: "graph-inbox",
		Name:             "Inbox",
		Role:             "inbox",
		Selectable:       true,
	}}); err != nil {
		t.Fatalf("UpsertFolders(counts unknown) error = %v", err)
	}
	var total, unread int
	if err := db.Read().QueryRowContext(ctx, `SELECT total_count, unread_count FROM folders WHERE id = 'acc_inbox'`).Scan(&total, &unread); err != nil {
		t.Fatalf("query counts: %v", err)
	}
	if total != 2000 || unread != 1089 {
		t.Fatalf("counts total=%d unread=%d, want preserved provider counts 2000/1089", total, unread)
	}
}

func TestUpsertProviderSyncMessagesHydratesExistingIMAPMessage(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "graph-inbox", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
		AccountID: "acc",
		FolderID:  "acc_inbox",
		RemoteUID: 7,
		MessageID: "<shared@example.com>",
		Subject:   "IMAP version",
		FromEmail: "sender@example.com",
		DateSent:  time.Now(),
		IsRead:    false,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	beforeID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<shared@example.com>")
	if err != nil || beforeID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", beforeID, err)
	}

	ids, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "graph-message-1",
		InternetMessageID: "<shared@example.com>",
		ProviderThreadID:  "conversation-1",
		Subject:           "Graph version",
		FromEmail:         "sender@example.com",
		DateSent:          time.Now(),
		DateReceived:      time.Now(),
		IsRead:            true,
		ToRecipients:      []Recipient{{Name: "To", Email: "to@example.com"}},
		CCRecipients:      []Recipient{{Name: "Cc", Email: "cc@example.com"}},
		BCCRecipients:     []Recipient{{Name: "Bcc", Email: "bcc@example.com"}},
		LabelsKnown:       true,
		LabelProvider:     LabelProviderOutlook,
		Labels:            []LabelInput{{Name: "Projects", ProviderID: "Projects", ProviderType: LabelProviderOutlook}},
	}})
	if err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
	if ids["graph-message-1"] != beforeID {
		t.Fatalf("provider ids = %#v, want graph-message-1 -> %d", ids, beforeID)
	}

	var remoteID string
	var isRead bool
	if err := db.Read().QueryRowContext(ctx, `
		SELECT COALESCE(m.remote_message_id, ''), mfs.is_read
		FROM messages m JOIN message_folder_state mfs ON mfs.message_id = m.id
		WHERE m.id = ?`, beforeID).Scan(&remoteID, &isRead); err != nil {
		t.Fatalf("query hydrated message: %v", err)
	}
	if remoteID != "graph-message-1" || !isRead {
		t.Fatalf("hydrated message remote=%q is_read=%v, want graph-message-1/true", remoteID, isRead)
	}

	email, err := db.GetEmailByID(ctx, strconv.FormatInt(beforeID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() error = %v", err)
	}
	if len(email.Labels) != 1 || email.Labels[0].Name != "Projects" || email.Labels[0].ProviderType != LabelProviderOutlook {
		t.Fatalf("labels = %#v, want Projects outlook label", email.Labels)
	}
	if len(email.To) != 1 || email.To[0].Email != "to@example.com" || len(email.CC) != 1 || email.CC[0].Email != "cc@example.com" || len(email.BCC) != 1 || email.BCC[0].Email != "bcc@example.com" {
		t.Fatalf("recipients to=%#v cc=%#v bcc=%#v, want provider to/cc/bcc", email.To, email.CC, email.BCC)
	}
}

func TestUpsertProviderSyncMessagesDoesNotBlankExistingHeaders(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "graph-inbox", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	now := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	receivedAt := now.Add(2 * time.Minute)
	if _, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "graph-message-1",
		InternetMessageID: "<shared@example.com>",
		Subject:           "Loaded subject",
		FromName:          "Loaded Sender",
		FromEmail:         "sender@example.com",
		DateSent:          now,
		DateReceived:      receivedAt,
		IsRead:            false,
		ToRecipients:      []Recipient{{Name: "Loaded Recipient", Email: "to@example.com"}},
	}}); err != nil {
		t.Fatalf("UpsertProviderSyncMessages(initial) error = %v", err)
	}
	if _, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "graph-message-1",
		InternetMessageID: "<shared@example.com>",
		IsRead:            true,
	}}); err != nil {
		t.Fatalf("UpsertProviderSyncMessages(partial) error = %v", err)
	}

	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<shared@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}
	email, err := db.GetEmailByID(ctx, strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() error = %v", err)
	}
	if email.Subject != "Loaded subject" || email.From.Name != "Loaded Sender" || email.From.Email != "sender@example.com" || !email.IsRead {
		t.Fatalf("email = %#v, want headers preserved and read state updated", email)
	}
	if len(email.To) != 1 || email.To[0].Email != "to@example.com" {
		t.Fatalf("email.To = %#v, want existing recipients preserved on partial update", email.To)
	}
	var dateSentRaw, dateReceivedRaw string
	if err := db.Read().QueryRowContext(ctx, `SELECT date_sent, date_received FROM messages WHERE id = ?`, msgID).Scan(&dateSentRaw, &dateReceivedRaw); err != nil {
		t.Fatalf("query dates: %v", err)
	}
	dateSent, ok := parseSQLiteDateTime(dateSentRaw)
	if !ok {
		t.Fatalf("parse date_sent %q failed", dateSentRaw)
	}
	dateReceived, ok := parseSQLiteDateTime(dateReceivedRaw)
	if !ok {
		t.Fatalf("parse date_received %q failed", dateReceivedRaw)
	}
	if !dateSent.Equal(now) || !dateReceived.Equal(receivedAt) {
		t.Fatalf("stored dates sent=%v received=%v, want sent=%v received=%v", dateSent, dateReceived, now, receivedAt)
	}
}

func TestMarkProviderMessagesMissingFromFolderMarksOnlyAbsentProviderRows(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'outlook', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{
		ID:               "acc_inbox",
		AccountID:        "acc",
		RemoteID:         "Inbox",
		ProviderRemoteID: "folder-inbox",
		Name:             "Inbox",
		Role:             "inbox",
		Selectable:       true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	now := time.Now()
	if _, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{
		{
			AccountID:         "acc",
			FolderID:          "acc_inbox",
			ProviderMessageID: "graph-current",
			InternetMessageID: "<graph-current@example.com>",
			Subject:           "Current",
			FromEmail:         "sender@example.com",
			DateSent:          now,
			DateReceived:      now,
			IsRead:            true,
		},
		{
			AccountID:         "acc",
			FolderID:          "acc_inbox",
			ProviderMessageID: "graph-stale",
			InternetMessageID: "<graph-stale@example.com>",
			Subject:           "Stale",
			FromEmail:         "sender@example.com",
			DateSent:          now,
			DateReceived:      now,
			IsRead:            true,
		},
	}); err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
		AccountID: "acc",
		FolderID:  "acc_inbox",
		RemoteUID: 99,
		MessageID: "<legacy@example.com>",
		Subject:   "Legacy",
		FromEmail: "sender@example.com",
		DateSent:  now,
		IsRead:    true,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}

	removed, err := db.MarkProviderMessagesMissingFromFolder(ctx, "acc", "acc_inbox", map[string]bool{"graph-current": true})
	if err != nil {
		t.Fatalf("MarkProviderMessagesMissingFromFolder() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want one stale provider row", removed)
	}

	deletedByMessageID := func(messageID string) int {
		t.Helper()
		var deleted int
		if err := db.Read().QueryRowContext(ctx, `
			SELECT mfs.is_deleted
			FROM messages m
			JOIN message_folder_state mfs ON mfs.message_id = m.id
			WHERE m.account_id = 'acc' AND m.internet_message_id = ? AND mfs.folder_id = 'acc_inbox'`, messageID).Scan(&deleted); err != nil {
			t.Fatalf("query deleted state for %s: %v", messageID, err)
		}
		return deleted
	}
	if deletedByMessageID("<graph-current@example.com>") != 0 {
		t.Fatal("current provider message was marked deleted")
	}
	if deletedByMessageID("<graph-stale@example.com>") != 1 {
		t.Fatal("stale provider message was not marked deleted")
	}
	if deletedByMessageID("<legacy@example.com>") != 0 {
		t.Fatal("legacy non-provider message was marked deleted")
	}
}

func TestRefreshAccountFolderThreadStateMakesProviderMessagesVisible(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'gmail', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{
		ID:               "acc_inbox",
		AccountID:        "acc",
		RemoteID:         "INBOX",
		ProviderRemoteID: "INBOX",
		Name:             "Inbox",
		Role:             "inbox",
		Selectable:       true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	base := time.Date(2020, 8, 7, 12, 0, 0, 0, time.UTC)
	if _, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "gmail-old",
		InternetMessageID: "<gmail-old@example.com>",
		Subject:           "Old inbox message",
		FromEmail:         "sender@example.com",
		DateSent:          base,
		DateReceived:      base,
		IsRead:            true,
	}}); err != nil {
		t.Fatalf("UpsertProviderSyncMessages(old) error = %v", err)
	}
	if err := db.RefreshAccountFolderThreadState(ctx, "acc"); err != nil {
		t.Fatalf("RefreshAccountFolderThreadState(initial) error = %v", err)
	}

	newer := base.AddDate(0, 0, 3)
	if _, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "gmail-new",
		InternetMessageID: "<gmail-new@example.com>",
		Subject:           "New inbox message",
		FromEmail:         "sender@example.com",
		DateSent:          newer,
		DateReceived:      newer,
		IsRead:            true,
	}}); err != nil {
		t.Fatalf("UpsertProviderSyncMessages(new) error = %v", err)
	}

	stalePage, err := db.GetEmailsRangeForUser(ctx, "default", "inbox", 0, 10)
	if err != nil {
		t.Fatalf("GetEmailsRangeForUser(stale) error = %v", err)
	}
	if len(stalePage.Emails) != 1 || stalePage.Emails[0].Subject != "Old inbox message" {
		t.Fatalf("stale unified inbox = %#v, want old cached thread before explicit refresh", stalePage.Emails)
	}

	if err := db.RefreshAccountFolderThreadState(ctx, "acc"); err != nil {
		t.Fatalf("RefreshAccountFolderThreadState(final) error = %v", err)
	}
	page, err := db.GetEmailsRangeForUser(ctx, "default", "inbox", 0, 10)
	if err != nil {
		t.Fatalf("GetEmailsRangeForUser(refreshed) error = %v", err)
	}
	if page.TotalCount != 2 || len(page.Emails) != 2 {
		t.Fatalf("refreshed unified inbox count=%d emails=%#v, want two messages", page.TotalCount, page.Emails)
	}
	if page.Emails[0].Subject != "New inbox message" || page.Emails[1].Subject != "Old inbox message" {
		t.Fatalf("refreshed unified inbox order = %#v, want new then old", page.Emails)
	}
}

func TestOutlookProviderUnreadCountPreservedWhileImportIncomplete(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'outlook', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{
		ID:               "acc_inbox",
		AccountID:        "acc",
		RemoteID:         "Inbox",
		ProviderRemoteID: "graph-inbox",
		Name:             "Inbox",
		Role:             "inbox",
		Selectable:       true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `UPDATE folders SET total_count = 2000, unread_count = 1089 WHERE id = 'acc_inbox'`); err != nil {
		t.Fatalf("seed provider counts: %v", err)
	}

	now := time.Now()
	msgs := make([]ProviderSyncMessage, 0, 24)
	for i := 0; i < 24; i++ {
		msgs = append(msgs, ProviderSyncMessage{
			AccountID:         "acc",
			FolderID:          "acc_inbox",
			ProviderMessageID: fmt.Sprintf("graph-message-%02d", i),
			InternetMessageID: fmt.Sprintf("<graph-message-%02d@example.com>", i),
			Subject:           fmt.Sprintf("Message %02d", i),
			FromEmail:         "sender@example.com",
			DateSent:          now.Add(time.Duration(i) * time.Second),
			DateReceived:      now.Add(time.Duration(i) * time.Second),
			IsRead:            false,
		})
	}
	if _, err := db.UpsertProviderSyncMessages(ctx, msgs); err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}

	refreshed, err := db.RefreshFolderUnreadCount(ctx, "acc_inbox")
	if err != nil {
		t.Fatalf("RefreshFolderUnreadCount() error = %v", err)
	}
	if refreshed != 1089 {
		t.Fatalf("RefreshFolderUnreadCount() = %d, want provider unread 1089 while import is incomplete", refreshed)
	}

	counts, err := db.GetAllFolderUnreadCounts(ctx, "default")
	if err != nil {
		t.Fatalf("GetAllFolderUnreadCounts() error = %v", err)
	}
	if counts["acc_inbox"] != 1089 {
		t.Fatalf("per-folder unread = %d, want provider unread 1089", counts["acc_inbox"])
	}
	if counts["inbox"] != 1089 {
		t.Fatalf("unified inbox unread = %d, want provider unread 1089", counts["inbox"])
	}

	count, err := db.GetFolderEmailCountFilteredForUser(ctx, "default", "inbox", models.EmailFilters{})
	if err != nil {
		t.Fatalf("GetFolderEmailCountFilteredForUser() error = %v", err)
	}
	if count != 24 {
		t.Fatalf("unfiltered unified inbox count = %d, want local count 24 while import is incomplete", count)
	}

	filteredCount, err := db.GetFolderEmailCountFilteredForUser(ctx, "default", "inbox", models.EmailFilters{Unread: true})
	if err != nil {
		t.Fatalf("GetFolderEmailCountFilteredForUser(unread) error = %v", err)
	}
	if filteredCount != 24 {
		t.Fatalf("filtered unread unified inbox count = %d, want local count 24", filteredCount)
	}

	page, err := db.GetEmailsRangeForUser(ctx, "default", "inbox", 0, 50)
	if err != nil {
		t.Fatalf("GetEmailsRangeForUser() error = %v", err)
	}
	if page.TotalCount != 24 || page.DisplayTotalCount != 24 || len(page.Emails) != 24 || page.HasMore {
		t.Fatalf("unified inbox page total=%d display=%d len=%d hasMore=%v, want local total/display 24 and no local rows pending", page.TotalCount, page.DisplayTotalCount, len(page.Emails), page.HasMore)
	}

	concretePage, err := db.GetEmailsRange(ctx, "acc_inbox", 0, 50)
	if err != nil {
		t.Fatalf("GetEmailsRange() error = %v", err)
	}
	if concretePage.TotalCount != 24 || concretePage.DisplayTotalCount != 24 || len(concretePage.Emails) != 24 {
		t.Fatalf("concrete inbox page total=%d display=%d len=%d, want local total/display 24", concretePage.TotalCount, concretePage.DisplayTotalCount, len(concretePage.Emails))
	}

	staleKnownTotalPage, err := db.GetEmailsRangeFilteredForUserWithTotal(ctx, "default", "inbox", 0, 50, models.EmailFilters{}, 49)
	if err != nil {
		t.Fatalf("GetEmailsRangeFilteredForUserWithTotal(stale known total) error = %v", err)
	}
	if staleKnownTotalPage.TotalCount != 24 || staleKnownTotalPage.DisplayTotalCount != 24 {
		t.Fatalf("page totals with stale known_total local=%d display=%d, want local total/display 24", staleKnownTotalPage.TotalCount, staleKnownTotalPage.DisplayTotalCount)
	}

	nextPage, err := db.GetEmailsAfterCursorForUser(ctx, "default", "inbox", page.Emails[len(page.Emails)-1].ID, 50)
	if err != nil {
		t.Fatalf("GetEmailsAfterCursorForUser() error = %v", err)
	}
	if nextPage.TotalCount != 24 || nextPage.DisplayTotalCount != 24 || len(nextPage.Emails) != 0 || nextPage.HasMore {
		t.Fatalf("empty follow-up page total=%d display=%d len=%d hasMore=%v, want local total/display 24 and no repeated empty loads", nextPage.TotalCount, nextPage.DisplayTotalCount, len(nextPage.Emails), nextPage.HasMore)
	}
}

func TestOutlookProviderTotalUsesLocalThreadCountWhenImportComplete(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'outlook', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{
		ID:               "acc_inbox",
		AccountID:        "acc",
		RemoteID:         "Inbox",
		ProviderRemoteID: "graph-inbox",
		Name:             "Inbox",
		Role:             "inbox",
		Selectable:       true,
		CountsKnown:      true,
		TotalCount:       2,
		UnreadCount:      0,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	now := time.Now()
	if _, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{
		{
			AccountID:         "acc",
			FolderID:          "acc_inbox",
			ProviderMessageID: "graph-root",
			InternetMessageID: "<graph-root@example.com>",
			Subject:           "Threaded message",
			FromEmail:         "sender@example.com",
			DateSent:          now,
			DateReceived:      now,
			IsRead:            true,
		},
		{
			AccountID:         "acc",
			FolderID:          "acc_inbox",
			ProviderMessageID: "graph-reply",
			InternetMessageID: "<graph-reply@example.com>",
			InReplyTo:         "<graph-root@example.com>",
			References:        "<graph-root@example.com>",
			Subject:           "Re: Threaded message",
			FromEmail:         "sender@example.com",
			DateSent:          now.Add(time.Minute),
			DateReceived:      now.Add(time.Minute),
			IsRead:            true,
		},
	}); err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
	if _, err := db.RefreshFolderUnreadCount(ctx, "acc_inbox"); err != nil {
		t.Fatalf("RefreshFolderUnreadCount() error = %v", err)
	}

	count, err := db.GetFolderEmailCountFilteredForUser(ctx, "default", "inbox", models.EmailFilters{})
	if err != nil {
		t.Fatalf("GetFolderEmailCountFilteredForUser() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("complete provider folder count = %d, want local thread count 1", count)
	}

	page, err := db.GetEmailsRangeForUser(ctx, "default", "inbox", 0, 50)
	if err != nil {
		t.Fatalf("GetEmailsRangeForUser() error = %v", err)
	}
	if page.TotalCount != 1 || page.DisplayTotalCount != 1 || len(page.Emails) != 1 || page.Emails[0].ThreadCount != 2 {
		t.Fatalf("complete provider page total=%d display=%d len=%d first=%#v, want one visible thread containing two messages", page.TotalCount, page.DisplayTotalCount, len(page.Emails), page.Emails)
	}
}

func TestReplaceAttachmentsPreservesProviderStoragePath(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'outlook', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "acc_inbox", AccountID: "acc", RemoteID: "Inbox", ProviderRemoteID: "graph-inbox", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	ids, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "graph-message-1",
		InternetMessageID: "<attachment-preserve@example.com>",
		Subject:           "Attachment preserve",
		FromEmail:         "sender@example.com",
		DateSent:          time.Now(),
		DateReceived:      time.Now(),
		IsRead:            true,
		HasAttachments:    true,
	}})
	if err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
	msgID := ids["graph-message-1"]
	if msgID == 0 {
		t.Fatal("provider message was not inserted")
	}

	if err := db.ReplaceAttachments(ctx, msgID, []AttachmentRow{{
		Filename:         "old.pdf",
		ContentType:      "application/pdf",
		SizeBytes:        10,
		StoragePath:      "/tmp/gofer-downloaded-old.pdf",
		ProviderRemoteID: "graph-attachment-1",
	}}); err != nil {
		t.Fatalf("ReplaceAttachments(initial) error = %v", err)
	}
	if err := db.ReplaceAttachments(ctx, msgID, []AttachmentRow{{
		Filename:         "new.pdf",
		ContentType:      "application/pdf",
		SizeBytes:        12,
		ProviderRemoteID: "graph-attachment-1",
	}}); err != nil {
		t.Fatalf("ReplaceAttachments(refresh) error = %v", err)
	}

	var filename, storagePath string
	if err := db.Read().QueryRowContext(ctx, `SELECT filename, storage_path FROM attachments WHERE message_id = ?`, msgID).Scan(&filename, &storagePath); err != nil {
		t.Fatalf("query attachment: %v", err)
	}
	if filename != "new.pdf" {
		t.Fatalf("filename = %q, want refreshed metadata", filename)
	}
	if storagePath != "/tmp/gofer-downloaded-old.pdf" {
		t.Fatalf("storage_path = %q, want preserved downloaded path", storagePath)
	}
}

func TestSyncMessagesReplaceIMAPKeywordLabels(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
		AccountID:     "acc",
		FolderID:      "acc_inbox",
		RemoteUID:     42,
		MessageID:     "<label-keyword@example.com>",
		Subject:       "Keyword label",
		FromEmail:     "sender@example.com",
		DateSent:      time.Now(),
		IsRead:        true,
		LabelsKnown:   true,
		LabelProvider: LabelProviderIMAPKeyword,
		Labels: []LabelInput{{
			AccountID:    "acc",
			Name:         "Work",
			ProviderID:   "Work",
			ProviderType: LabelProviderIMAPKeyword,
		}},
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<label-keyword@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}
	email, err := db.GetEmailByID(ctx, strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() error = %v", err)
	}
	if len(email.Labels) != 1 || email.Labels[0].Name != "Work" || email.Labels[0].ProviderType != LabelProviderIMAPKeyword {
		t.Fatalf("labels after sync = %#v, want Work IMAP keyword", email.Labels)
	}

	if _, err := db.BatchUpdateFlags(ctx, "acc_inbox", []FlagUpdate{{
		UID:           42,
		IsRead:        true,
		LabelsKnown:   true,
		LabelProvider: LabelProviderIMAPKeyword,
	}}); err != nil {
		t.Fatalf("BatchUpdateFlags() error = %v", err)
	}
	email, err = db.GetEmailByID(ctx, strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() after refresh error = %v", err)
	}
	if len(email.Labels) != 0 {
		t.Fatalf("labels after keyword removal = %#v, want none", email.Labels)
	}
}

func TestSyncMessagesResolveIMAPKeywordAliases(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
		AccountID:     "acc",
		FolderID:      "acc_inbox",
		RemoteUID:     42,
		MessageID:     "<alias-keyword@example.com>",
		Subject:       "Keyword alias",
		FromEmail:     "sender@example.com",
		DateSent:      time.Now(),
		IsRead:        true,
		LabelsKnown:   true,
		LabelProvider: LabelProviderIMAPKeyword,
		Labels: []LabelInput{
			{Name: "$label2", ProviderID: "$label2", ProviderType: LabelProviderIMAPKeyword},
			{Name: "$VendorSnooze", ProviderID: "$VendorSnooze", ProviderType: LabelProviderIMAPKeyword},
		},
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<alias-keyword@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}
	email, err := db.GetEmailByID(ctx, strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() error = %v", err)
	}
	labels := map[string]string{}
	for _, label := range email.Labels {
		labels[label.Name] = label.ProviderID
	}
	if labels["Work"] != "$label2" {
		t.Fatalf("labels = %#v, want Work backed by $label2", email.Labels)
	}
	if labels["$VendorSnooze"] != "$VendorSnooze" {
		t.Fatalf("labels = %#v, want discovered raw vendor label", email.Labels)
	}

	providerID, ok, err := db.ResolveLabelAliasProviderID(ctx, "acc", LabelProviderIMAPKeyword, "Work")
	if err != nil || !ok || providerID != "$label2" {
		t.Fatalf("ResolveLabelAliasProviderID(Work) = %q, %v, %v; want $label2 true nil", providerID, ok, err)
	}
}

func TestUpsertLabelAliasRenamesExistingIMAPKeywordLabel(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
		AccountID:     "acc",
		FolderID:      "acc_inbox",
		RemoteUID:     42,
		MessageID:     "<custom-alias-keyword@example.com>",
		Subject:       "Custom keyword alias",
		FromEmail:     "sender@example.com",
		DateSent:      time.Now(),
		IsRead:        true,
		LabelsKnown:   true,
		LabelProvider: LabelProviderIMAPKeyword,
		Labels: []LabelInput{{
			Name:         "$VendorSnooze",
			ProviderID:   "$VendorSnooze",
			ProviderType: LabelProviderIMAPKeyword,
		}},
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<custom-alias-keyword@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}

	if err := db.UpsertLabelAlias(ctx, LabelAliasInput{
		AccountID:    "acc",
		ProviderType: LabelProviderIMAPKeyword,
		ProviderID:   "$VendorSnooze",
		DisplayName:  "Snooze",
		Source:       "user",
	}); err != nil {
		t.Fatalf("UpsertLabelAlias() error = %v", err)
	}
	email, err := db.GetEmailByID(ctx, strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() error = %v", err)
	}
	if len(email.Labels) != 1 || email.Labels[0].Name != "Snooze" || email.Labels[0].ProviderID != "$VendorSnooze" {
		t.Fatalf("labels = %#v, want Snooze backed by vendor keyword", email.Labels)
	}

	providerID, ok, err := db.ResolveLabelAliasProviderID(ctx, "acc", LabelProviderIMAPKeyword, "Snooze")
	if err != nil || !ok || providerID != "$VendorSnooze" {
		t.Fatalf("ResolveLabelAliasProviderID(Snooze) = %q, %v, %v; want $VendorSnooze true nil", providerID, ok, err)
	}
}

func TestSyncMessagesDeduplicateResolvedIMAPKeywordAliases(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
		AccountID:     "acc",
		FolderID:      "acc_inbox",
		RemoteUID:     42,
		MessageID:     "<duplicate-alias-keyword@example.com>",
		Subject:       "Duplicate keyword alias",
		FromEmail:     "sender@example.com",
		DateSent:      time.Now(),
		IsRead:        true,
		LabelsKnown:   true,
		LabelProvider: LabelProviderIMAPKeyword,
		Labels: []LabelInput{
			{Name: "Work", ProviderID: "Work", ProviderType: LabelProviderIMAPKeyword},
			{Name: "$label2", ProviderID: "$label2", ProviderType: LabelProviderIMAPKeyword},
		},
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<duplicate-alias-keyword@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}
	email, err := db.GetEmailByID(ctx, strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() error = %v", err)
	}
	if len(email.Labels) != 1 || email.Labels[0].Name != "Work" || email.Labels[0].ProviderID != "$label2" {
		t.Fatalf("labels = %#v, want one Work label backed by $label2", email.Labels)
	}
}

func TestGetLabelAdminStatusAggregatesCoverageAndLastRun(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address, display_name) VALUES ('acc', 'default', 'gmail', 'user@example.com', 'User Gmail')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	now := time.Now()
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{
		{AccountID: "acc", FolderID: "acc_inbox", RemoteUID: 1, MessageID: "<labeled@example.com>", Subject: "Labeled", FromEmail: "sender@example.com", DateSent: now, IsRead: true},
		{AccountID: "acc", FolderID: "acc_inbox", RemoteUID: 2, MessageID: "<unlabeled@example.com>", Subject: "Unlabeled", FromEmail: "sender@example.com", DateSent: now, IsRead: true},
	}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	labeledID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<labeled@example.com>")
	if err != nil {
		t.Fatalf("GetMessageLocalIDByInternetID() error = %v", err)
	}
	unlabeledID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<unlabeled@example.com>")
	if err != nil {
		t.Fatalf("GetMessageLocalIDByInternetID(unlabeled) error = %v", err)
	}
	if err := db.SetMessageProviderMessageID(ctx, labeledID, "gmail-message-1"); err != nil {
		t.Fatalf("SetMessageProviderMessageID() error = %v", err)
	}
	if _, err := db.AddMessageLabel(ctx, labeledID, "acc", LabelInput{
		AccountID:    "acc",
		Name:         "Projects",
		ProviderID:   "Label_1",
		ProviderType: LabelProviderGmail,
	}); err != nil {
		t.Fatalf("AddMessageLabel() error = %v", err)
	}
	if _, err := db.AddMessageLabel(ctx, unlabeledID, "acc", LabelInput{
		AccountID:    "acc",
		Name:         "NonJunk",
		ProviderID:   "NonJunk",
		ProviderType: LabelProviderIMAPKeyword,
	}); err != nil {
		t.Fatalf("AddMessageLabel(NonJunk) error = %v", err)
	}
	if err := db.EnqueueLabelMutation(ctx, "acc", labeledID, "acc_inbox", LabelProviderGmail, LabelMutationAdd, "Later", errors.New("remote busy")); err != nil {
		t.Fatalf("EnqueueLabelMutation() error = %v", err)
	}
	if err := db.MarkLabelSyncRun(ctx, LabelSyncRunStats{
		AccountID:               "acc",
		ProviderType:            LabelProviderGmail,
		Scope:                   "messages",
		StartedAt:               now.Add(-time.Minute),
		FinishedAt:              now,
		Full:                    true,
		TotalMessages:           2,
		SyncedMessages:          1,
		WithLabels:              1,
		MissingProviderMessages: 1,
		PendingMutations:        1,
	}, nil); err != nil {
		t.Fatalf("MarkLabelSyncRun() error = %v", err)
	}

	status, err := db.GetLabelAdminStatus(ctx, "default")
	if err != nil {
		t.Fatalf("GetLabelAdminStatus() error = %v", err)
	}
	if len(status.Accounts) != 1 {
		t.Fatalf("accounts = %#v, want one account", status.Accounts)
	}
	account := status.Accounts[0]
	if account.TotalMessages != 2 || account.MessagesWithLabels != 1 || account.MessagesWithoutLabels != 1 || account.ProviderBackedMessages != 1 || account.MissingProviderMessages != 1 {
		t.Fatalf("account coverage = %#v, want one labeled and one missing provider id", account)
	}
	if account.KnownLabels != 1 {
		t.Fatalf("known labels = %d, want only Projects; NonJunk should be excluded", account.KnownLabels)
	}
	if account.PendingMutations != 1 || account.MutationErrors != 1 || !strings.Contains(account.LatestMutationError, "remote busy") {
		t.Fatalf("mutation status = %#v, want queued mutation error", account)
	}
	if account.Sync.LastTotalMessages != 2 || account.Sync.LastSyncedMessages != 1 || account.Sync.LastMissingProviderMessages != 1 || account.Sync.LastPendingMutations != 1 {
		t.Fatalf("sync status = %#v, want persisted last-run counters", account.Sync)
	}
	if status.Totals.TotalMessages != 2 || status.Totals.PendingMutations != 1 || status.Totals.LastRunMissingProvider != 1 {
		t.Fatalf("totals = %#v, want account totals", status.Totals)
	}
	if len(account.TopLabels) != 1 || account.TopLabels[0].Name != "Projects" || account.TopLabels[0].Count != 1 {
		t.Fatalf("top labels = %#v, want Projects usage", account.TopLabels)
	}
}

func TestMarkLabelSyncRunDoesNotAdvanceCursorOnError(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'gmail', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.MarkLabelSyncRun(ctx, LabelSyncRunStats{
		AccountID:    "acc",
		ProviderType: LabelProviderGmail,
		Scope:        "messages",
		Cursor:       "100",
		StartedAt:    time.Now().Add(-time.Minute),
		FinishedAt:   time.Now(),
	}, nil); err != nil {
		t.Fatalf("MarkLabelSyncRun(success) error = %v", err)
	}
	if err := db.MarkLabelSyncRun(ctx, LabelSyncRunStats{
		AccountID:    "acc",
		ProviderType: LabelProviderGmail,
		Scope:        "messages",
		Cursor:       "200",
		StartedAt:    time.Now().Add(-time.Minute),
		FinishedAt:   time.Now(),
	}, errors.New("temporary failure")); err != nil {
		t.Fatalf("MarkLabelSyncRun(error) error = %v", err)
	}
	state, err := db.GetLabelSyncState(ctx, "acc", LabelProviderGmail, "messages")
	if err != nil {
		t.Fatalf("GetLabelSyncState() error = %v", err)
	}
	if state.Cursor != "100" || state.LastError != "temporary failure" {
		t.Fatalf("sync state = %#v, want cursor preserved and error recorded", state)
	}
}

func TestGetLabelAdminStatusIncludesOutlookGraphDiagnostics(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address, display_name) VALUES ('acc', 'default', 'outlook', 'user@example.com', 'User Outlook')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "Inbox", ProviderRemoteID: "graph-inbox", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_legacy", AccountID: "acc", RemoteID: "Legacy", Name: "Legacy", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	now := time.Now()
	if _, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "graph-message-1",
		InternetMessageID: "<graph@example.com>",
		Subject:           "Graph",
		FromEmail:         "sender@example.com",
		DateSent:          now,
		DateReceived:      now,
		IsRead:            true,
	}}); err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{
		{AccountID: "acc", FolderID: "acc_inbox", RemoteUID: 2, MessageID: "<backfillable@example.com>", Subject: "Backfillable", FromEmail: "sender@example.com", DateSent: now, IsRead: true},
		{AccountID: "acc", FolderID: "acc_legacy", RemoteUID: 3, MessageID: "", Subject: "Synthetic", FromEmail: "sender@example.com", DateSent: now, IsRead: true},
	}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}

	status, err := db.GetLabelAdminStatus(ctx, "default")
	if err != nil {
		t.Fatalf("GetLabelAdminStatus() error = %v", err)
	}
	if len(status.Accounts) != 1 || status.Accounts[0].OutlookGraph == nil {
		t.Fatalf("accounts = %#v, want Outlook graph diagnostics", status.Accounts)
	}
	graph := status.Accounts[0].OutlookGraph
	if graph.GraphBackedMessages != 1 || graph.IMAPBackedMessages != 2 || graph.MessagesMissingGraphID != 2 {
		t.Fatalf("graph diagnostics = %#v, want graph/imap/missing counts", graph)
	}
	if graph.MessageParityDelta != -1 || graph.GraphParityReady {
		t.Fatalf("graph parity = delta %d ready %v, want -1/not ready", graph.MessageParityDelta, graph.GraphParityReady)
	}
	if graph.MissingGraphIDWithInternetID != 1 || graph.MissingGraphIDWithoutInternetID != 1 || graph.MissingGraphIDWithoutGraphFolder != 1 {
		t.Fatalf("graph missing reason buckets = %#v, want one in each expected bucket", graph)
	}
	if graph.LocalFolders != 2 || graph.GraphBackedFolders != 1 || graph.FoldersMissingGraphID != 1 {
		t.Fatalf("graph folder diagnostics = %#v, want one graph-backed and one legacy folder", graph)
	}
}

func TestGetLabelAdminStatusIncludesGmailAPIDiagnostics(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address, display_name) VALUES ('acc', 'default', 'gmail', 'user@example.com', 'User Gmail')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_legacy", AccountID: "acc", RemoteID: "Legacy", Name: "Legacy", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	now := time.Now()
	if _, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "gmail-message-1",
		InternetMessageID: "<gmail@example.com>",
		Subject:           "Gmail",
		FromEmail:         "sender@example.com",
		DateSent:          now,
		DateReceived:      now,
		IsRead:            true,
	}}); err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{
		{AccountID: "acc", FolderID: "acc_inbox", RemoteUID: 2, MessageID: "<backfillable@example.com>", Subject: "Backfillable", FromEmail: "sender@example.com", DateSent: now, IsRead: true},
		{AccountID: "acc", FolderID: "acc_legacy", RemoteUID: 3, MessageID: "", Subject: "Synthetic", FromEmail: "sender@example.com", DateSent: now, IsRead: true},
	}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	if err := db.MarkLabelSyncRun(ctx, LabelSyncRunStats{
		AccountID:      "acc",
		ProviderType:   LabelProviderGmail,
		Scope:          "messages",
		StartedAt:      now.Add(-time.Minute),
		FinishedAt:     now,
		Full:           true,
		Cursor:         "gmail-history-123",
		TotalMessages:  3,
		SyncedMessages: 1,
	}, nil); err != nil {
		t.Fatalf("MarkLabelSyncRun() error = %v", err)
	}
	if err := db.MarkGmailPollCheck(ctx, GmailPollState{
		AccountID:        "acc",
		ProfileHistoryID: "gmail-profile-456",
		LastCheckedAt:    sql.NullTime{Time: now, Valid: true},
		LastChangedAt:    sql.NullTime{Time: now.Add(time.Minute), Valid: true},
	}, true, nil); err != nil {
		t.Fatalf("MarkGmailPollCheck() error = %v", err)
	}

	status, err := db.GetLabelAdminStatus(ctx, "default")
	if err != nil {
		t.Fatalf("GetLabelAdminStatus() error = %v", err)
	}
	if len(status.Accounts) != 1 || status.Accounts[0].GmailAPI == nil {
		t.Fatalf("accounts = %#v, want Gmail API diagnostics", status.Accounts)
	}
	gmail := status.Accounts[0].GmailAPI
	if gmail.APIBackedMessages != 1 || gmail.IMAPBackedMessages != 2 || gmail.MessagesMissingGmailID != 2 {
		t.Fatalf("gmail diagnostics = %#v, want api/imap/missing counts", gmail)
	}
	if gmail.MessageParityDelta != -1 || gmail.APIParityReady {
		t.Fatalf("gmail parity = delta %d ready %v, want -1/not ready", gmail.MessageParityDelta, gmail.APIParityReady)
	}
	if gmail.MissingGmailIDWithInternetID != 1 || gmail.MissingGmailIDWithoutInternetID != 1 || gmail.MissingGmailIDWithoutGmailLabel != 1 {
		t.Fatalf("gmail missing reason buckets = %#v, want one in each expected bucket", gmail)
	}
	if gmail.LocalFolders != 2 || gmail.GmailBackedFolders != 1 || gmail.FoldersMissingGmailID != 1 {
		t.Fatalf("gmail folder diagnostics = %#v, want one gmail-backed and one legacy folder", gmail)
	}
	if !gmail.HasHistoryCursor || gmail.HistoryCursor != "gmail-history-123" || status.Accounts[0].Sync.Cursor != "gmail-history-123" {
		t.Fatalf("gmail cursor diagnostics = %#v sync=%#v, want cursor surfaced", gmail, status.Accounts[0].Sync)
	}
	if gmail.PollProfileHistoryID != "gmail-profile-456" || gmail.LastPollAt.IsZero() || gmail.LastPollChangeAt.IsZero() || gmail.LastPollError != "" {
		t.Fatalf("gmail poll diagnostics = %#v, want profile history and poll timestamps", gmail)
	}
}

func TestGmailPollStateAndAccountListing(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, email_address)
		VALUES ('acc', 'default', 'gmail', 'User@Example.com'),
		       ('disabled', 'default', 'gmail', 'disabled@example.com'),
		       ('outlook', 'default', 'outlook', 'outlook@example.com')
	`); err != nil {
		t.Fatalf("insert accounts: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `UPDATE accounts SET email_sync_enabled = 0 WHERE id = 'disabled'`); err != nil {
		t.Fatalf("disable account: %v", err)
	}

	accounts, err := db.GetGmailEmailSyncAccountIDs(ctx, "default")
	if err != nil {
		t.Fatalf("GetGmailEmailSyncAccountIDs() error = %v", err)
	}
	if len(accounts) != 1 || accounts[0] != "acc" {
		t.Fatalf("accounts = %#v, want only enabled Gmail account", accounts)
	}

	now := time.Now().UTC()
	if err := db.MarkGmailPollCheck(ctx, GmailPollState{
		AccountID:        "acc",
		ProfileHistoryID: "100",
		LastCheckedAt:    sql.NullTime{Time: now, Valid: true},
		LastChangedAt:    sql.NullTime{Time: now, Valid: true},
	}, true, nil); err != nil {
		t.Fatalf("MarkGmailPollCheck(success) error = %v", err)
	}
	if err := db.MarkGmailPollCheck(ctx, GmailPollState{
		AccountID:         "acc",
		ProfileHistoryID:  "101",
		LastCheckedAt:     sql.NullTime{Time: now.Add(time.Minute), Valid: true},
		ConsecutiveErrors: 0,
	}, false, errors.New("profile failed")); err != nil {
		t.Fatalf("MarkGmailPollCheck(error) error = %v", err)
	}
	state, err := db.GetGmailPollState(ctx, "acc")
	if err != nil {
		t.Fatalf("GetGmailPollState() error = %v", err)
	}
	if state.ProfileHistoryID != "101" || !state.LastCheckedAt.Valid || !state.LastChangedAt.Valid || state.LastError != "profile failed" || state.ConsecutiveErrors != 1 {
		t.Fatalf("state = %#v, want latest profile history, check/change timestamps, and error", state)
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

func TestEmailQueryFilterSearchesBeyondInitialWindow(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "inbox", AccountID: "acc", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	base := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	msgs := make([]SyncMessage, 0, 61)
	for i := 0; i < 60; i++ {
		msgs = append(msgs, SyncMessage{
			AccountID: "acc",
			FolderID:  "inbox",
			RemoteUID: uint32(i + 1),
			MessageID: fmt.Sprintf("<recent-%02d@example.com>", i),
			Subject:   fmt.Sprintf("Recent message %02d", i),
			FromEmail: "recent@example.com",
			DateSent:  base.Add(time.Duration(i) * time.Minute),
			Snippet:   "ordinary visible-window message",
			IsRead:    true,
		})
	}
	msgs = append(msgs, SyncMessage{
		AccountID: "acc",
		FolderID:  "inbox",
		RemoteUID: 100,
		MessageID: "<deep-search@example.com>",
		Subject:   "Archive candidate",
		FromEmail: "deep@example.com",
		DateSent:  base.Add(-time.Hour),
		Snippet:   "contains deepsearchneedle outside the initial page",
		IsRead:    true,
	})
	if err := db.UpsertSyncMessages(ctx, msgs); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	targetID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<deep-search@example.com>")
	if err != nil || targetID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID(target) = %d, %v", targetID, err)
	}

	firstPage, err := db.GetEmailsRangeFilteredForUser(ctx, "default", "inbox", 0, 10, models.EmailFilters{})
	if err != nil {
		t.Fatalf("GetEmailsRangeFilteredForUser(first page) error = %v", err)
	}
	for _, email := range firstPage.Emails {
		if email.ID == strconv.FormatInt(targetID, 10) {
			t.Fatalf("target unexpectedly appeared in first unfiltered page")
		}
	}

	page, err := db.GetEmailsRangeFilteredForUser(ctx, "default", "inbox", 0, 10, models.EmailFilters{Query: "deepsearchneedle"})
	if err != nil {
		t.Fatalf("GetEmailsRangeFilteredForUser(query) error = %v", err)
	}
	if page.TotalCount != 1 || len(page.Emails) != 1 || page.Emails[0].ID != strconv.FormatInt(targetID, 10) {
		t.Fatalf("query page total=%d emails=%#v, want hidden target %d", page.TotalCount, page.Emails, targetID)
	}
}

func TestEmailFiltersCoverStructuredSearchFields(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, email_address) VALUES
			('acc_a', 'default', 'outlook', 'a@example.com'),
			('acc_b', 'default', 'outlook', 'b@example.com')`); err != nil {
		t.Fatalf("insert accounts: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_a_inbox", AccountID: "acc_a", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_b_inbox", AccountID: "acc_b", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	targetDate := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	otherDate := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if _, err := db.UpsertProviderSyncMessages(ctx, []ProviderSyncMessage{
		{
			AccountID:         "acc_a",
			FolderID:          "acc_a_inbox",
			ProviderMessageID: "target-provider",
			InternetMessageID: "<filter-target@example.com>",
			Subject:           "Quarterly Budget",
			FromName:          "Alice Finance",
			FromEmail:         "alice@finance.example.com",
			DateSent:          targetDate,
			DateReceived:      targetDate,
			Snippet:           "budget planning preview",
			IsRead:            false,
			IsStarred:         true,
			ToRecipients:      []Recipient{{Name: "Bob", Email: "bob@example.net"}},
			CCRecipients:      []Recipient{{Name: "Carol", Email: "carol@example.net"}},
			BCCRecipients:     []Recipient{{Name: "Hidden", Email: "hidden@example.net"}},
		},
		{
			AccountID:         "acc_a",
			FolderID:          "acc_a_inbox",
			ProviderMessageID: "target-child-provider",
			InternetMessageID: "<filter-target-child@example.com>",
			InReplyTo:         "<filter-target@example.com>",
			References:        "<filter-target@example.com>",
			Subject:           "Re: Quarterly Budget",
			FromName:          "Thread Child",
			FromEmail:         "child@example.com",
			DateSent:          targetDate.Add(-time.Minute),
			DateReceived:      targetDate.Add(-time.Minute),
			Snippet:           "thread child without the specific filters",
			IsRead:            true,
		},
		{
			AccountID:         "acc_b",
			FolderID:          "acc_b_inbox",
			ProviderMessageID: "other-provider",
			InternetMessageID: "<filter-other@example.com>",
			Subject:           "Casual note",
			FromName:          "Other Sender",
			FromEmail:         "other@else.example.com",
			DateSent:          otherDate,
			DateReceived:      otherDate,
			Snippet:           "plain read message",
			IsRead:            true,
		},
	}); err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}

	targetID, err := db.GetMessageLocalIDByInternetID(ctx, "acc_a", "<filter-target@example.com>")
	if err != nil || targetID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID(target) = %d, %v", targetID, err)
	}
	otherID, err := db.GetMessageLocalIDByInternetID(ctx, "acc_b", "<filter-other@example.com>")
	if err != nil || otherID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID(other) = %d, %v", otherID, err)
	}
	bodyPath := filepath.Join(t.TempDir(), "target-body.txt")
	if err := os.WriteFile(bodyPath, []byte("body contains bodyneedle for full text search"), 0600); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := db.UpdateMessageBody(ctx, targetID, bodyPath, "", "", "budget planning preview"); err != nil {
		t.Fatalf("UpdateMessageBody() error = %v", err)
	}
	if err := db.ReplaceAttachments(ctx, targetID, []AttachmentRow{{Filename: "receipt-deep.pdf", ContentType: "application/pdf", SizeBytes: 1024}}); err != nil {
		t.Fatalf("ReplaceAttachments() error = %v", err)
	}
	if _, err := db.AddMessageLabel(ctx, targetID, "acc_a", LabelInput{AccountID: "acc_a", Name: "ProjectX", ProviderID: "ProjectX", ProviderType: LabelProviderLocal}); err != nil {
		t.Fatalf("AddMessageLabel() error = %v", err)
	}

	tests := []struct {
		name    string
		filters models.EmailFilters
		wantID  int64
	}{
		{name: "query sender", filters: models.EmailFilters{Query: "alice"}, wantID: targetID},
		{name: "query bcc recipient", filters: models.EmailFilters{Query: "hidden@example.net"}, wantID: targetID},
		{name: "query attachment filename", filters: models.EmailFilters{Query: "receipt-deep"}, wantID: targetID},
		{name: "from", filters: models.EmailFilters{From: "Alice"}, wantID: targetID},
		{name: "from domain", filters: models.EmailFilters{FromDomain: "finance.example.com"}, wantID: targetID},
		{name: "recipient includes bcc", filters: models.EmailFilters{To: "hidden@example.net"}, wantID: targetID},
		{name: "subject", filters: models.EmailFilters{Subject: "Quarterly"}, wantID: targetID},
		{name: "body", filters: models.EmailFilters{Body: "bodyneedle"}, wantID: targetID},
		{name: "attachment", filters: models.EmailFilters{Attachment: "receipt-deep.pdf"}, wantID: targetID},
		{name: "tag", filters: models.EmailFilters{Tag: "ProjectX", TagAccountID: "acc_a"}, wantID: targetID},
		{name: "account", filters: models.EmailFilters{AccountID: "acc_a"}, wantID: targetID},
		{name: "unread", filters: models.EmailFilters{Unread: true}, wantID: targetID},
		{name: "starred", filters: models.EmailFilters{Starred: true}, wantID: targetID},
		{name: "attachments", filters: models.EmailFilters{Attachments: true}, wantID: targetID},
		{name: "has tags", filters: models.EmailFilters{HasTags: true}, wantID: targetID},
		{name: "threads only", filters: models.EmailFilters{ThreadsOnly: true}, wantID: targetID},
		{name: "after", filters: models.EmailFilters{After: "2026-06-01"}, wantID: targetID},
		{name: "read", filters: models.EmailFilters{Read: true}, wantID: otherID},
		{name: "no attachments", filters: models.EmailFilters{NoAttach: true}, wantID: otherID},
		{name: "before", filters: models.EmailFilters{Before: "2026-05-15"}, wantID: otherID},
		{name: "unread wins read conflict", filters: models.EmailFilters{Unread: true, Read: true}, wantID: targetID},
		{name: "attachments wins no-attachments conflict", filters: models.EmailFilters{Attachments: true, NoAttach: true}, wantID: targetID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := db.GetEmailsRangeFilteredForUser(ctx, "default", "inbox", 0, 10, tt.filters)
			if err != nil {
				t.Fatalf("GetEmailsRangeFilteredForUser() error = %v", err)
			}
			if page.TotalCount != 1 || len(page.Emails) != 1 || page.Emails[0].ID != strconv.FormatInt(tt.wantID, 10) {
				t.Fatalf("filtered page total=%d emails=%#v, want only %d", page.TotalCount, page.Emails, tt.wantID)
			}
		})
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

func TestSidebarTagFilterMatchesAliasedIMAPKeywordLabels(t *testing.T) {
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
		MessageID: "<work-label@example.com>",
		Subject:   "Work label",
		FromEmail: "sender@example.com",
		DateSent:  time.Now(),
		IsRead:    true,
		Labels:    []LabelInput{{Name: "$label2", ProviderID: "$label2", ProviderType: LabelProviderIMAPKeyword}},
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}

	page, err := db.GetEmailsRangeFilteredForUser(ctx, "default", "inbox", 0, 50, models.EmailFilters{
		Tag:          "Work",
		TagAccountID: "acc",
	})
	if err != nil {
		t.Fatalf("GetEmailsRangeFilteredForUser() error = %v", err)
	}
	if page.TotalCount != 1 || len(page.Emails) != 1 {
		t.Fatalf("sidebar tag filtered page total=%d len=%d, want 1 result", page.TotalCount, len(page.Emails))
	}
	if len(page.Emails[0].Labels) != 1 || page.Emails[0].Labels[0].Name != "Work" || page.Emails[0].Labels[0].ProviderID != "$label2" {
		t.Fatalf("email labels = %#v, want Work backed by $label2", page.Emails[0].Labels)
	}
}

func TestSidebarTagFilterMatchesLegacyRawIMAPKeywordRows(t *testing.T) {
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
		MessageID: "<legacy-work-label@example.com>",
		Subject:   "Legacy work label",
		FromEmail: "sender@example.com",
		DateSent:  time.Now(),
		IsRead:    true,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	messageID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<legacy-work-label@example.com>")
	if err != nil || messageID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", messageID, err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO labels (id, account_id, name, color, provider_id, provider_type, is_system, updated_at)
		VALUES ('legacy-label', 'acc', '$label2', 'bg-muted text-muted-foreground', '$label2', 'imap_keyword', 0, CURRENT_TIMESTAMP);
		INSERT INTO message_labels (message_id, label_id)
		VALUES (?, 'legacy-label')`, messageID); err != nil {
		t.Fatalf("insert legacy label: %v", err)
	}
	page, err := db.GetEmailsRangeFilteredForUser(ctx, "default", "inbox", 0, 50, models.EmailFilters{
		Tag:             "Work",
		TagAccountID:    "acc",
		TagProviderID:   "$label2",
		TagProviderType: LabelProviderIMAPKeyword,
	})
	if err != nil {
		t.Fatalf("GetEmailsRangeFilteredForUser() error = %v", err)
	}
	if page.TotalCount != 1 || len(page.Emails) != 1 {
		t.Fatalf("provider-backed sidebar tag page total=%d len=%d, want 1 legacy result", page.TotalCount, len(page.Emails))
	}

	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO label_aliases (account_id, provider_type, provider_id, display_name, color, source, updated_at)
		VALUES ('acc', 'imap_keyword', '$label2', 'Work', 'bg-muted text-muted-foreground', 'default', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("insert alias: %v", err)
	}
	page, err = db.GetEmailsRangeFilteredForUser(ctx, "default", "inbox", 0, 50, models.EmailFilters{
		Tag:          "Work",
		TagAccountID: "acc",
	})
	if err != nil {
		t.Fatalf("GetEmailsRangeFilteredForUser(alias) error = %v", err)
	}
	if page.TotalCount != 1 || len(page.Emails) != 1 {
		t.Fatalf("alias sidebar tag page total=%d len=%d, want 1 legacy result", page.TotalCount, len(page.Emails))
	}
}

func TestUnknownRemoteUIDAllowsMultipleFolderMemberships(t *testing.T) {
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
	if err := db.AddMessageToFolder(ctx, 2, "spam", 0, false, true); err != nil {
		t.Fatalf("AddMessageToFolder(2, uid=0) error = %v", err)
	}

	var count, nullUIDs int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*), SUM(CASE WHEN remote_uid IS NULL THEN 1 ELSE 0 END) FROM message_folder_state WHERE folder_id = 'spam'`).Scan(&count, &nullUIDs); err != nil {
		t.Fatalf("query folder state: %v", err)
	}
	if count != 2 || nullUIDs != 2 {
		t.Fatalf("folder state count=%d nullUIDs=%d, want 2 and 2", count, nullUIDs)
	}

	if err := db.SetMessageFolderRemoteUID(ctx, 2, "spam", 55); err != nil {
		t.Fatalf("SetMessageFolderRemoteUID() error = %v", err)
	}
	var remoteUID sql.NullInt64
	var isRead, isStarred int
	if err := db.Read().QueryRowContext(ctx, `
		SELECT remote_uid, is_read, is_starred
		FROM message_folder_state
		WHERE message_id = 2 AND folder_id = 'spam'`).Scan(&remoteUID, &isRead, &isStarred); err != nil {
		t.Fatalf("query updated folder state: %v", err)
	}
	if !remoteUID.Valid || remoteUID.Int64 != 55 || isRead != 0 || isStarred != 1 {
		t.Fatalf("updated state uid=%v read=%d starred=%d, want uid=55 read=0 starred=1", remoteUID, isRead, isStarred)
	}
	if err := db.Read().QueryRowContext(ctx, `
		SELECT remote_uid FROM message_folder_state
		WHERE message_id = 1 AND folder_id = 'spam'`).Scan(&remoteUID); err != nil {
		t.Fatalf("query remaining provisional state: %v", err)
	}
	if remoteUID.Valid {
		t.Fatalf("first provisional remote uid = %d, want NULL", remoteUID.Int64)
	}
}

func TestResetFolderUIDStateStartsANewUIDGeneration(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_archive", AccountID: "acc", RemoteID: "Archive", Name: "Archive", Role: "archive", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		UPDATE folders
		SET uid_validity = 100,
		    uid_next = 5001,
		    highest_seen_uid = 5000,
		    highest_modseq = 9000,
		    total_count = 2,
		    unread_count = 1,
		    last_full_sync_at = CURRENT_TIMESTAMP,
		    last_incremental_sync_at = CURRENT_TIMESTAMP,
		    sync_error = 'old error'
		WHERE id = 'acc_inbox';
		INSERT INTO messages (id, account_id, internet_message_id, subject, from_email)
		VALUES (1, 'acc', '<inbox-only@example.com>', 'Inbox only', 'sender@example.com'),
		       (2, 'acc', '<shared@example.com>', 'Shared', 'sender@example.com');
		INSERT INTO message_folder_state (message_id, folder_id, remote_uid, is_read, synced_at)
		VALUES (1, 'acc_inbox', 5000, 0, CURRENT_TIMESTAMP),
		       (2, 'acc_inbox', 4999, 1, CURRENT_TIMESTAMP),
		       (2, 'acc_archive', 42, 1, CURRENT_TIMESTAMP);
		INSERT INTO folder_thread_state (folder_id, thread_key, head_message_id, account_id)
		VALUES ('acc_inbox', 'thread-1', 1, 'acc');
	`); err != nil {
		t.Fatalf("seed folder UID state: %v", err)
	}

	if err := db.ResetFolderUIDState(ctx, "acc_inbox", 200); err != nil {
		t.Fatalf("ResetFolderUIDState() error = %v", err)
	}

	var uidValidity, highestUID, totalCount, unreadCount int64
	var uidNext, highestModseq sql.NullInt64
	var lastFullIsNull, lastIncrementalIsNull, syncErrorIsNull int
	if err := db.Read().QueryRowContext(ctx, `
		SELECT uid_validity, highest_seen_uid, uid_next, highest_modseq,
		       total_count, unread_count,
		       last_full_sync_at IS NULL, last_incremental_sync_at IS NULL, sync_error IS NULL
		FROM folders WHERE id = 'acc_inbox'`).Scan(
		&uidValidity, &highestUID, &uidNext, &highestModseq,
		&totalCount, &unreadCount,
		&lastFullIsNull, &lastIncrementalIsNull, &syncErrorIsNull,
	); err != nil {
		t.Fatalf("query reset folder: %v", err)
	}
	if uidValidity != 200 || highestUID != 0 || uidNext.Valid || highestModseq.Valid || totalCount != 0 || unreadCount != 0 {
		t.Fatalf("reset folder state = validity:%d highest:%d uidNext:%v modseq:%v total:%d unread:%d", uidValidity, highestUID, uidNext, highestModseq, totalCount, unreadCount)
	}
	if lastFullIsNull != 1 || lastIncrementalIsNull != 1 || syncErrorIsNull != 1 {
		t.Fatalf("reset timestamps/error = full null:%d incremental null:%d error null:%d", lastFullIsNull, lastIncrementalIsNull, syncErrorIsNull)
	}

	var inboxStates, inboxThreads, inboxOnlyMessages, sharedMessages, archiveStates int
	queries := []struct {
		query string
		value *int
	}{
		{query: `SELECT COUNT(*) FROM message_folder_state WHERE folder_id = 'acc_inbox'`, value: &inboxStates},
		{query: `SELECT COUNT(*) FROM folder_thread_state WHERE folder_id = 'acc_inbox'`, value: &inboxThreads},
		{query: `SELECT COUNT(*) FROM messages WHERE id = 1`, value: &inboxOnlyMessages},
		{query: `SELECT COUNT(*) FROM messages WHERE id = 2`, value: &sharedMessages},
		{query: `SELECT COUNT(*) FROM message_folder_state WHERE message_id = 2 AND folder_id = 'acc_archive'`, value: &archiveStates},
	}
	for _, query := range queries {
		if err := db.Read().QueryRowContext(ctx, query.query).Scan(query.value); err != nil {
			t.Fatalf("query reset contents: %v", err)
		}
	}
	if inboxStates != 0 || inboxThreads != 0 || inboxOnlyMessages != 0 || sharedMessages != 1 || archiveStates != 1 {
		t.Fatalf("reset contents = inbox states:%d threads:%d orphan:%d shared:%d archive states:%d", inboxStates, inboxThreads, inboxOnlyMessages, sharedMessages, archiveStates)
	}
}

func TestGetAttachmentFetchInfoForUserRejectsForeignAttachment(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO users (id, email, name) VALUES ('other', 'other@example.com', 'Other')`); err != nil {
		t.Fatalf("insert other user: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, email_address)
		VALUES ('owned', 'default', 'imap', 'owned@example.com'),
		       ('foreign', 'other', 'imap', 'foreign@example.com')`); err != nil {
		t.Fatalf("insert accounts: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO messages (id, account_id, internet_message_id, subject, from_email)
		VALUES (1, 'foreign', '<foreign@example.com>', 'foreign', 'sender@example.com')`); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	result, err := db.Write().ExecContext(ctx, `
		INSERT INTO attachments (message_id, filename, content_type, content_id, size_bytes, storage_path)
		VALUES (1, 'secret.txt', 'text/plain', 'secret-content', 6, '/tmp/secret.txt')`)
	if err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
	attachmentID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("attachment id: %v", err)
	}

	info, err := db.GetAttachmentFetchInfoForUser(ctx, attachmentID, "default")
	if err != nil {
		t.Fatalf("GetAttachmentFetchInfoForUser(default) error = %v", err)
	}
	if info != nil {
		t.Fatalf("GetAttachmentFetchInfoForUser(default) = %#v, want nil", info)
	}

	info, err = db.GetAttachmentFetchInfoForUser(ctx, attachmentID, "other")
	if err != nil {
		t.Fatalf("GetAttachmentFetchInfoForUser(other) error = %v", err)
	}
	if info == nil || info.AccountID != "foreign" {
		t.Fatalf("GetAttachmentFetchInfoForUser(other) = %#v, want foreign attachment", info)
	}

	info, err = db.GetAttachmentFetchInfoByContentIDForUser(ctx, 1, "secret-content", "default")
	if err != nil {
		t.Fatalf("GetAttachmentFetchInfoByContentIDForUser(default) error = %v", err)
	}
	if info != nil {
		t.Fatalf("GetAttachmentFetchInfoByContentIDForUser(default) = %#v, want nil", info)
	}
	info, err = db.GetAttachmentFetchInfoByContentIDForUser(ctx, 1, "secret-content", "other")
	if err != nil {
		t.Fatalf("GetAttachmentFetchInfoByContentIDForUser(other) error = %v", err)
	}
	if info == nil || info.AccountID != "foreign" {
		t.Fatalf("GetAttachmentFetchInfoByContentIDForUser(other) = %#v, want foreign attachment", info)
	}
}

func TestSyncGmailInboxMembershipPreservesRealUIDAndRemovesOnlySyntheticRows(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)

	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'gmail', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_important", AccountID: "acc", RemoteID: "[Gmail]/Important", Name: "Important", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	now := time.Now()
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
		AccountID: "acc",
		FolderID:  "acc_important",
		RemoteUID: 7,
		MessageID: "<synthetic-inbox@example.com>",
		Subject:   "Synthetic inbox",
		FromEmail: "sender@example.com",
		DateSent:  now,
		IsRead:    true,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages(important) error = %v", err)
	}
	syntheticID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<synthetic-inbox@example.com>")
	if err != nil || syntheticID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID(synthetic) = %d, %v", syntheticID, err)
	}
	if err := db.SyncGmailInboxMembership(ctx, syntheticID, "acc", []string{"INBOX", "UNREAD"}); err != nil {
		t.Fatalf("SyncGmailInboxMembership(add synthetic) error = %v", err)
	}
	var remoteUID sql.NullInt64
	var isRead int
	if err := db.Read().QueryRowContext(ctx, `SELECT remote_uid, is_read FROM message_folder_state WHERE message_id = ? AND folder_id = 'acc_inbox'`, syntheticID).Scan(&remoteUID, &isRead); err != nil {
		t.Fatalf("query synthetic inbox row: %v", err)
	}
	if remoteUID.Valid || isRead != 0 {
		t.Fatalf("synthetic inbox row remoteUID=%v isRead=%d, want null/unread", remoteUID, isRead)
	}
	if err := db.SyncGmailInboxMembership(ctx, syntheticID, "acc", []string{"IMPORTANT"}); err != nil {
		t.Fatalf("SyncGmailInboxMembership(remove synthetic) error = %v", err)
	}
	var syntheticRows int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM message_folder_state WHERE message_id = ? AND folder_id = 'acc_inbox'`, syntheticID).Scan(&syntheticRows); err != nil {
		t.Fatalf("query synthetic inbox rows: %v", err)
	}
	if syntheticRows != 0 {
		t.Fatalf("synthetic inbox rows after removal = %d, want 0", syntheticRows)
	}

	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{
		AccountID: "acc",
		FolderID:  "acc_inbox",
		RemoteUID: 99,
		MessageID: "<real-inbox@example.com>",
		Subject:   "Real inbox",
		FromEmail: "sender@example.com",
		DateSent:  now,
		IsRead:    false,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages(inbox) error = %v", err)
	}
	realID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<real-inbox@example.com>")
	if err != nil || realID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID(real) = %d, %v", realID, err)
	}
	if err := db.SyncGmailInboxMembership(ctx, realID, "acc", []string{"INBOX"}); err != nil {
		t.Fatalf("SyncGmailInboxMembership(update real) error = %v", err)
	}
	if err := db.Read().QueryRowContext(ctx, `SELECT remote_uid, is_read FROM message_folder_state WHERE message_id = ? AND folder_id = 'acc_inbox'`, realID).Scan(&remoteUID, &isRead); err != nil {
		t.Fatalf("query real inbox row: %v", err)
	}
	if !remoteUID.Valid || remoteUID.Int64 != 99 || isRead != 1 {
		t.Fatalf("real inbox row remoteUID=%v isRead=%d, want UID 99/read", remoteUID, isRead)
	}
	if err := db.SyncGmailInboxMembership(ctx, realID, "acc", []string{"IMPORTANT"}); err != nil {
		t.Fatalf("SyncGmailInboxMembership(skip real removal) error = %v", err)
	}
	var realRows int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM message_folder_state WHERE message_id = ? AND folder_id = 'acc_inbox'`, realID).Scan(&realRows); err != nil {
		t.Fatalf("query real inbox rows: %v", err)
	}
	if realRows != 1 {
		t.Fatalf("real inbox rows after removal attempt = %d, want preserved row", realRows)
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

func TestFreshSchemaStartsAtCurrentVersion(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if version != 60 {
		t.Fatalf("schema version = %d, want 60", version)
	}
}

func TestMigrateV54ConvertsZeroRemoteUIDsToNull(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB() error = %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (54);
		CREATE TABLE message_folder_state (
			message_id INTEGER NOT NULL,
			folder_id TEXT NOT NULL,
			remote_uid INTEGER,
			PRIMARY KEY (message_id, folder_id)
		);
		INSERT INTO message_folder_state (message_id, folder_id, remote_uid) VALUES (1, 'sent', 0);
	`); err != nil {
		raw.Close()
		t.Fatalf("seed v54 database: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var remoteUID sql.NullInt64
	if err := db.Read().QueryRow(`SELECT remote_uid FROM message_folder_state WHERE message_id = 1`).Scan(&remoteUID); err != nil {
		t.Fatalf("query migrated remote uid: %v", err)
	}
	if remoteUID.Valid {
		t.Fatalf("remote_uid = %d, want NULL", remoteUID.Int64)
	}
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if version != 60 {
		t.Fatalf("schema version = %d, want 60", version)
	}
}

func TestMigrateV55AddsMailSecurityExceptions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB() error = %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (55);
	`); err != nil {
		raw.Close()
		t.Fatalf("seed v55 database: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var table string
	if err := db.Read().QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'mail_security_exceptions'`).Scan(&table); err != nil {
		t.Fatalf("query mail security table: %v", err)
	}
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if version != 60 {
		t.Fatalf("schema version = %d, want 60", version)
	}
}

func TestMigrateV56AddsOAuthAccountFlows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB() error = %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (56);
		CREATE TABLE users (id TEXT PRIMARY KEY);
	`); err != nil {
		raw.Close()
		t.Fatalf("seed v56 database: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var table string
	if err := db.Read().QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'oauth_account_flows'`).Scan(&table); err != nil {
		t.Fatalf("query OAuth flow table: %v", err)
	}
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if version != 60 {
		t.Fatalf("schema version = %d, want 60", version)
	}
}

func TestMigrateV57MovesScheduledSendsIntoOutgoingQueue(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB() error = %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (57);
		CREATE TABLE accounts (
			id TEXT PRIMARY KEY,
			provider TEXT NOT NULL DEFAULT '',
			email_address TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			internet_message_id TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE scheduled_sends (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			message_id INTEGER NOT NULL,
			scheduled_for DATETIME NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			attempt_count INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			locked_at DATETIME,
			sent_message_id TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO accounts (id, provider, email_address) VALUES ('acc', 'imap', 'user@example.com');
		INSERT INTO messages (id, internet_message_id) VALUES (1, '<draft@example.com>');
		INSERT INTO scheduled_sends (id, account_id, message_id, scheduled_for)
		VALUES ('scheduled', 'acc', 1, '2026-08-01 10:00:00');
	`); err != nil {
		raw.Close()
		t.Fatalf("seed v57 database: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var draftID, transport, status string
	var scheduled int
	var mime []byte
	if err := db.Read().QueryRow(`
		SELECT draft_id, transport, status, is_scheduled, mime_data
		FROM outgoing_sends WHERE id = 'scheduled'`).Scan(&draftID, &transport, &status, &scheduled, &mime); err != nil {
		t.Fatalf("query migrated outgoing send: %v", err)
	}
	if draftID != "<draft@example.com>" || transport != OutgoingTransportSMTP || status != OutgoingSendPending || scheduled != 1 || len(mime) != 0 {
		t.Fatalf("migrated outgoing send draft=%q transport=%q status=%q scheduled=%d mime=%d", draftID, transport, status, scheduled, len(mime))
	}
	var oldTableCount int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'scheduled_sends'`).Scan(&oldTableCount); err != nil {
		t.Fatalf("query old scheduled table: %v", err)
	}
	if oldTableCount != 0 {
		t.Fatalf("scheduled_sends still exists")
	}
}

func TestMigrateV58AddsRemoteSentCopyState(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB() error = %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (58);
		CREATE TABLE outgoing_sends (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			message_id INTEGER,
			draft_id TEXT NOT NULL DEFAULT '',
			transport TEXT NOT NULL,
			envelope_from TEXT NOT NULL,
			envelope_recipients TEXT NOT NULL DEFAULT '[]',
			mime_data BLOB,
			message_json TEXT NOT NULL DEFAULT '',
			send_after DATETIME NOT NULL,
			is_scheduled INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'pending',
			attempt_count INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			locked_at DATETIME,
			sent_message_id TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(message_id)
		);
		INSERT INTO outgoing_sends (
			id, account_id, transport, envelope_from, envelope_recipients, mime_data,
			message_json, send_after, status, sent_message_id
		) VALUES (
			'sent', 'acc', 'smtp', 'user@example.com', '["friend@example.com"]', X'01',
			'{}', CURRENT_TIMESTAMP, 'sent', '<sent@example.com>'
		);
	`); err != nil {
		raw.Close()
		t.Fatalf("seed v58 database: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var copyStatus string
	var attempts int
	if err := db.Read().QueryRow(`
		SELECT sent_copy_status, sent_copy_attempt_count
		FROM outgoing_sends WHERE id = 'sent'`).Scan(&copyStatus, &attempts); err != nil {
		t.Fatalf("query migrated Sent copy state: %v", err)
	}
	if copyStatus != SentCopyNotRequired || attempts != 0 {
		t.Fatalf("migrated Sent copy status=%q attempts=%d", copyStatus, attempts)
	}
}

func TestMigrateV59AddsIMAPDraftSyncQueue(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB() error = %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (59);
		CREATE TABLE accounts (id TEXT PRIMARY KEY);
		CREATE TABLE messages (id INTEGER PRIMARY KEY);
		INSERT INTO accounts (id) VALUES ('acc');
		INSERT INTO messages (id) VALUES (1);
	`); err != nil {
		raw.Close()
		t.Fatalf("seed v59 database: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if version != 60 {
		t.Fatalf("schema version = %d, want 60", version)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO imap_draft_states (
			account_id, draft_key, local_message_id, folder_id, folder_remote_name
		) VALUES ('acc', '<draft@example.com>', 1, 'drafts', 'Drafts');
		INSERT INTO imap_draft_operations (
			id, account_id, draft_key, kind, revision_token, mime_data
		) VALUES ('operation', 'acc', '<draft@example.com>', 'upsert', 'revision', X'01');
	`); err != nil {
		t.Fatalf("use migrated IMAP draft tables: %v", err)
	}
}

func TestMigrateV45AddsLabelMutationQueueFolderID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gofer.db")
	raw, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB() error = %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO schema_version (version) VALUES (45);
		CREATE TABLE label_mutation_queue (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id TEXT NOT NULL,
			message_id INTEGER NOT NULL,
			provider_type TEXT NOT NULL,
			operation TEXT NOT NULL,
			label_name TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			last_error TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`); err != nil {
		raw.Close()
		t.Fatalf("seed v45 database: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	db, err := New(dbPath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var folderID string
	if err := db.Read().QueryRow(`SELECT COALESCE(folder_id, '') FROM label_mutation_queue LIMIT 1`).Scan(&folderID); err != nil && err != sql.ErrNoRows {
		t.Fatalf("query label_mutation_queue.folder_id: %v", err)
	}

	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if version != 60 {
		t.Fatalf("schema version = %d, want 60", version)
	}
	var totalMessages int
	if err := db.Read().QueryRow(`SELECT COALESCE(last_total_messages, 0) FROM label_sync_state LIMIT 1`).Scan(&totalMessages); err != nil && err != sql.ErrNoRows {
		t.Fatalf("query label_sync_state.last_total_messages: %v", err)
	}
	var aliasRows int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM label_aliases`).Scan(&aliasRows); err != nil {
		t.Fatalf("query label_aliases: %v", err)
	}
}
