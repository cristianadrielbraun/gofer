package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	if version != 51 {
		t.Fatalf("schema version = %d, want 51", version)
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
	if version != 51 {
		t.Fatalf("schema version = %d, want 51", version)
	}
	var totalMessages int
	if err := db.Read().QueryRow(`SELECT COALESCE(last_total_messages, 0) FROM label_sync_state LIMIT 1`).Scan(&totalMessages); err != nil && err != sql.ErrNoRows {
		t.Fatalf("query label_sync_state.last_total_messages: %v", err)
	}
}
