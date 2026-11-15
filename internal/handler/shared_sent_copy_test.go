package handler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	imapclient "github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

// A message sent as a shared mailbox is kept in that mailbox's Sent folder,
// found by its role there, on the server and locally, not in the person's.
func TestSentCopyOfASharedMailboxGoesToItsSentFolder(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	h.blobStore = store.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	h.syncer = mailpkg.NewSyncOrchestrator(db, h.accountStore, h.blobStore, nil)
	ctx := t.Context()
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "own-sent", AccountID: "victim-account", RemoteID: "Sent Items", Name: "Sent", Role: "sent", Selectable: true},
		{ID: "support-sent", AccountID: "victim-account", RemoteID: "Shared Folders/support@example.com/Sent Items", Name: "Sent Items", Role: "custom", Selectable: true, SharedMailbox: "support@example.com", SharedRole: "sent"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ApplySharedMailboxDiscovery(ctx, "victim-account", []string{"support@example.com"}, true, time.Now()); err != nil {
		t.Fatal(err)
	}

	to, _ := message.ParseAddressList("customer@example.net")
	msg := &message.OutgoingMessage{
		FromName: "Example Support", FromEmail: "support@example.com", To: to,
		Subject: "Your ticket", TextBody: "Fixed.", MessageID: "<ticket@example.com>", Date: time.Now().UTC(),
	}
	queued, err := h.queueOutgoingMessage(ctx, "victim-account", 0, "", msg, time.Now().Add(-time.Second), false)
	if err != nil {
		t.Fatal(err)
	}
	if sends, err := db.ClaimDueOutgoingSends(ctx, time.Now(), 1); err != nil || len(sends) != 1 {
		t.Fatalf("ClaimDueOutgoingSends() = %#v, %v", sends, err)
	}
	h.saveSentMessageSnapshot(ctx, "victim-account", msg, queued.MIMEData)
	if err := db.CompleteOutgoingSend(ctx, queued.ID, msg.MessageID, true); err != nil {
		t.Fatal(err)
	}
	fake := &fakeSentCopyIMAPClient{appendResult: imapclient.AppendResult{UID: 5, UIDValidity: 9}}
	h.sentCopyIMAPFactory = func(context.Context, *models.AccountConfig, string) (sentCopyIMAPClient, error) { return fake, nil }

	h.runDueSentCopies(ctx)

	if fake.appendCalls != 1 || fake.mailbox != "Shared Folders/support@example.com/Sent Items" {
		t.Fatalf("appended to %q (%d times), want the shared mailbox's Sent", fake.mailbox, fake.appendCalls)
	}
	localID, _ := db.GetMessageLocalIDByInternetIDInternal(ctx, "victim-account", msg.MessageID)
	var shared, own int
	_ = db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM message_folder_state WHERE message_id = ? AND folder_id = 'support-sent'`, localID).Scan(&shared)
	_ = db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM message_folder_state WHERE message_id = ? AND folder_id = 'own-sent'`, localID).Scan(&own)
	if shared != 1 || own != 0 {
		t.Fatalf("local copy in shared Sent %d, own Sent %d", shared, own)
	}
}

// A draft whose From changes from a shared mailbox to the person's own
// address leaves the shared Drafts, here and on the server.
func TestDraftMovedOutOfASharedDraftsFolder(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	h.syncer = mailpkg.NewSyncOrchestrator(db, h.accountStore, h.blobStore, nil)
	ctx := t.Context()
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "own-drafts", AccountID: "victim-account", RemoteID: "Drafts", Name: "Drafts", Role: "drafts", Selectable: true},
		{ID: "support-drafts", AccountID: "victim-account", RemoteID: "Shared Folders/support@example.com/Drafts", Name: "Drafts", Role: "custom", Selectable: true, SharedMailbox: "support@example.com", SharedRole: "drafts"},
	}); err != nil {
		t.Fatal(err)
	}
	draft := storage.DraftMessageInput{AccountID: "victim-account", FolderID: "support-drafts", InternetMessageID: "<draft@example.com>", FromEmail: "support@example.com", Subject: "Draft"}
	msgID, err := db.SaveDraftMessage(ctx, draft)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(ctx, `UPDATE message_folder_state SET remote_uid = 41 WHERE message_id = ?`, msgID); err != nil {
		t.Fatal(err)
	}
	previous, err := db.GetDraftProviderInfoInternal(ctx, "victim-account", "<draft@example.com>")
	if err != nil || previous == nil || previous.FolderID != "support-drafts" {
		t.Fatalf("previous = %+v, %v", previous, err)
	}

	draft.FolderID, draft.FromEmail = "own-drafts", "owner@example.com"
	if _, err := db.SaveDraftMessage(ctx, draft); err != nil {
		t.Fatal(err)
	}
	account := &models.Account{ID: "victim-account", Provider: "imap", Email: "owner@example.com"}
	if !h.moveDraftOutOfOldFolder(ctx, account, "<draft@example.com>", msgID, previous, "own-drafts") {
		t.Fatal("draft not taken as moved")
	}
	var folders []string
	rows, _ := db.Read().QueryContext(ctx, `SELECT folder_id FROM message_folder_state WHERE message_id = ?`, msgID)
	for rows.Next() {
		var f string
		_ = rows.Scan(&f)
		folders = append(folders, f)
	}
	rows.Close()
	if len(folders) != 1 || folders[0] != "own-drafts" {
		t.Fatalf("draft is in %v, want only own-drafts", folders)
	}
	ops, err := db.ClaimDueIMAPDraftOperations(ctx, time.Now().Add(time.Minute), 10)
	if err != nil || len(ops) != 1 {
		t.Fatalf("queued draft operations = %+v, %v", ops, err)
	}
	if op := ops[0]; op.Kind != storage.IMAPDraftOperationDelete || op.State.FolderRemoteName != "Shared Folders/support@example.com/Drafts" || op.State.RemoteUID != 41 {
		t.Fatalf("operation = %+v", op)
	}
	// Saved again in the same folder, nothing moves.
	if h.moveDraftOutOfOldFolder(ctx, account, "<draft@example.com>", msgID, &storage.DraftProviderInfo{FolderID: "own-drafts"}, "own-drafts") {
		t.Fatal("a draft saved in place was taken as moved")
	}
}
