package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type fakeMessageMutationIMAPClient struct {
	storeErr        error
	storeCalls      int
	folder          string
	uid             uint32
	op              goimap.StoreFlagsOp
	flags           []goimap.Flag
	findUID         uint32
	findByFolder    map[string]uint32
	moveUID         uint32
	moveErr         error
	moveCalls       int
	moveSource      string
	moveDest        string
	deleteErr       error
	deleteCalls     int
	deleteFolder    string
	deleteUIDs      []uint32
	deleteValidity  uint32
	validityChanged bool
}

func (c *fakeMessageMutationIMAPClient) StoreFlags(_ context.Context, folder string, uid uint32, op goimap.StoreFlagsOp, flags []goimap.Flag) error {
	c.storeCalls++
	c.folder = folder
	c.uid = uid
	c.op = op
	c.flags = append([]goimap.Flag(nil), flags...)
	return c.storeErr
}

func (c *fakeMessageMutationIMAPClient) FindUIDByMessageID(_ context.Context, folder, _ string) (uint32, error) {
	if c.findByFolder != nil {
		return c.findByFolder[folder], nil
	}
	return c.findUID, nil
}

func (c *fakeMessageMutationIMAPClient) MoveMessageWithDestUID(_ context.Context, source string, uid uint32, destination string) (uint32, error) {
	c.moveCalls++
	c.moveSource = source
	c.moveDest = destination
	c.uid = uid
	return c.moveUID, c.moveErr
}

func (c *fakeMessageMutationIMAPClient) DeleteMessagesIfUIDValidity(_ context.Context, folder string, uids []uint32, expectedUIDValidity uint32) (bool, error) {
	c.deleteCalls++
	c.deleteFolder = folder
	c.deleteUIDs = append([]uint32(nil), uids...)
	c.deleteValidity = expectedUIDValidity
	return c.validityChanged, c.deleteErr
}

func (c *fakeMessageMutationIMAPClient) Close() error { return nil }

func seedIMAPMessageMutationWorker(t *testing.T) (*Handler, *storage.DB, int64, string) {
	t.Helper()
	h, db := newAccountOwnershipTestHandler(t)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{
		ID: "victim-inbox", AccountID: "victim-account", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if err := db.UpsertSyncMessages(t.Context(), []storage.SyncMessage{{
		AccountID: "victim-account", FolderID: "victim-inbox", RemoteUID: 42,
		MessageID: "<queued-state@example.com>", Subject: "Queued state", FromEmail: "sender@example.com",
		DateSent: time.Now(), IsRead: true,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	messageID, err := db.GetMessageLocalIDByInternetID(t.Context(), "victim-account", "<queued-state@example.com>")
	if err != nil || messageID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", messageID, err)
	}
	if err := db.SetMessageReadAndQueue(t.Context(), messageID, false); err != nil {
		t.Fatalf("SetMessageReadAndQueue() error = %v", err)
	}
	var mutationID string
	if err := db.Read().QueryRow(`SELECT id FROM message_mutations WHERE message_id = ? AND kind = 'read'`, messageID).Scan(&mutationID); err != nil {
		t.Fatalf("query mutation id: %v", err)
	}
	return h, db, messageID, mutationID
}

func TestMessageMutationWorkerAppliesGenericIMAPState(t *testing.T) {
	h, db, _, mutationID := seedIMAPMessageMutationWorker(t)
	fake := &fakeMessageMutationIMAPClient{}
	h.messageMutationIMAPFactory = func(context.Context, *models.AccountConfig, string) (messageMutationIMAPClient, error) {
		return fake, nil
	}

	h.runDueMessageMutations(t.Context())

	if fake.storeCalls != 1 || fake.folder != "INBOX" || fake.uid != 42 || fake.op != goimap.StoreFlagsDel || len(fake.flags) != 1 || fake.flags[0] != goimap.FlagSeen {
		t.Fatalf("IMAP mutation calls=%d folder=%q uid=%d op=%v flags=%v", fake.storeCalls, fake.folder, fake.uid, fake.op, fake.flags)
	}
	mutation, err := db.GetMessageMutation(t.Context(), mutationID)
	if err != nil || mutation.Status != storage.MessageMutationApplied {
		t.Fatalf("applied mutation = %#v, %v", mutation, err)
	}
}

func TestMessageMutationWorkerRetriesProviderFailure(t *testing.T) {
	h, db, _, mutationID := seedIMAPMessageMutationWorker(t)
	fake := &fakeMessageMutationIMAPClient{storeErr: errors.New("temporary IMAP failure")}
	h.messageMutationIMAPFactory = func(context.Context, *models.AccountConfig, string) (messageMutationIMAPClient, error) {
		return fake, nil
	}

	h.runDueMessageMutations(t.Context())
	failed, err := db.GetMessageMutation(t.Context(), mutationID)
	if err != nil || failed.Status != storage.MessageMutationFailed || failed.AttemptCount != 1 || failed.LastError == "" {
		t.Fatalf("failed mutation = %#v, %v", failed, err)
	}
	if _, err := db.Write().Exec(`UPDATE message_mutations SET next_attempt_at = CURRENT_TIMESTAMP WHERE id = ?`, mutationID); err != nil {
		t.Fatalf("make retry due: %v", err)
	}
	fake.storeErr = nil
	h.runDueMessageMutations(t.Context())
	applied, err := db.GetMessageMutation(t.Context(), mutationID)
	if err != nil || applied.Status != storage.MessageMutationApplied || applied.AttemptCount != 2 || fake.storeCalls != 2 {
		t.Fatalf("retried mutation = %#v calls=%d error=%v", applied, fake.storeCalls, err)
	}
}

func TestMessageMutationWorkerDoesNotFallbackAcrossProviders(t *testing.T) {
	ctx := t.Context()
	h, db := newGmailAPITestHandler(t, ctx)
	messageID := seedGmailAPIMessage(t, ctx, db, []storage.UpsertFolderInput{{
		ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true,
	}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary Gmail failure", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	previousBase := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBase })
	fakeIMAP := &fakeMessageMutationIMAPClient{}
	h.messageMutationIMAPFactory = func(context.Context, *models.AccountConfig, string) (messageMutationIMAPClient, error) {
		return fakeIMAP, nil
	}
	if err := db.SetMessageStarredAndQueue(ctx, messageID, true); err != nil {
		t.Fatalf("SetMessageStarredAndQueue() error = %v", err)
	}

	h.runDueMessageMutations(ctx)

	if fakeIMAP.storeCalls != 0 {
		t.Fatalf("Gmail failure fell through to IMAP: %d call(s)", fakeIMAP.storeCalls)
	}
	var provider, status string
	if err := db.Read().QueryRow(`SELECT provider_type, status FROM message_mutations WHERE message_id = ? AND kind = 'starred'`, messageID).Scan(&provider, &status); err != nil {
		t.Fatalf("query failed Gmail mutation: %v", err)
	}
	if provider != storage.MessageMutationProviderGmail || status != storage.MessageMutationFailed {
		t.Fatalf("failed Gmail mutation provider=%q status=%q", provider, status)
	}
}

func TestMessageMutationWorkerMovesGenericIMAPMessage(t *testing.T) {
	h, db, messageID, _ := seedIMAPMessageMutationWorker(t)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{
		ID: "victim-archive", AccountID: "victim-account", RemoteID: "Archive", Name: "Archive", Role: "archive", Selectable: true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if _, err := db.Write().Exec(`DELETE FROM message_mutations WHERE message_id = ?`, messageID); err != nil {
		t.Fatalf("clear seeded mutation: %v", err)
	}
	if err := db.MoveMessageAndQueue(t.Context(), messageID, "victim-inbox", "victim-archive"); err != nil {
		t.Fatalf("MoveMessageAndQueue() error = %v", err)
	}
	fake := &fakeMessageMutationIMAPClient{moveUID: 84}
	h.messageMutationIMAPFactory = func(context.Context, *models.AccountConfig, string) (messageMutationIMAPClient, error) {
		return fake, nil
	}

	h.runDueMessageMutations(t.Context())

	if fake.moveCalls != 1 || fake.moveSource != "INBOX" || fake.moveDest != "Archive" || fake.uid != 42 {
		t.Fatalf("IMAP move calls=%d source=%q dest=%q uid=%d", fake.moveCalls, fake.moveSource, fake.moveDest, fake.uid)
	}
	var status, sourceFolder, destinationFolder string
	if err := db.Read().QueryRow(`SELECT status, folder_id, destination_folder_id FROM message_mutations WHERE message_id = ? AND kind = 'move'`, messageID).
		Scan(&status, &sourceFolder, &destinationFolder); err != nil {
		t.Fatalf("query move mutation: %v", err)
	}
	if status != storage.MessageMutationApplied || sourceFolder != "victim-archive" || destinationFolder != "victim-archive" {
		t.Fatalf("move mutation status=%q source=%q destination=%q", status, sourceFolder, destinationFolder)
	}
	var destinationUID uint32
	if err := db.Read().QueryRow(`SELECT remote_uid FROM message_folder_state WHERE message_id = ? AND folder_id = 'victim-archive'`, messageID).Scan(&destinationUID); err != nil || destinationUID != 84 {
		t.Fatalf("destination UID = %d, %v", destinationUID, err)
	}
}

func TestMessageMutationWorkerRecoversAmbiguousIMAPMove(t *testing.T) {
	h, db, messageID, _ := seedIMAPMessageMutationWorker(t)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{
		ID: "victim-archive", AccountID: "victim-account", RemoteID: "Archive", Name: "Archive", Role: "archive", Selectable: true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	_, _ = db.Write().Exec(`DELETE FROM message_mutations WHERE message_id = ?`, messageID)
	if err := db.MoveMessageAndQueue(t.Context(), messageID, "victim-inbox", "victim-archive"); err != nil {
		t.Fatalf("MoveMessageAndQueue() error = %v", err)
	}
	fake := &fakeMessageMutationIMAPClient{
		moveErr:      errors.New("connection lost after MOVE"),
		findByFolder: map[string]uint32{"Archive": 84},
	}
	h.messageMutationIMAPFactory = func(context.Context, *models.AccountConfig, string) (messageMutationIMAPClient, error) {
		return fake, nil
	}

	h.runDueMessageMutations(t.Context())

	var status string
	if err := db.Read().QueryRow(`SELECT status FROM message_mutations WHERE message_id = ? AND kind = 'move'`, messageID).Scan(&status); err != nil {
		t.Fatalf("query move status: %v", err)
	}
	if status != storage.MessageMutationApplied {
		t.Fatalf("ambiguous move status=%q, want applied", status)
	}
}

func TestMessageMutationWorkerRetriesIMAPMove(t *testing.T) {
	h, db, messageID, _ := seedIMAPMessageMutationWorker(t)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{
		ID: "victim-archive", AccountID: "victim-account", RemoteID: "Archive", Name: "Archive", Role: "archive", Selectable: true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	_, _ = db.Write().Exec(`DELETE FROM message_mutations WHERE message_id = ?`, messageID)
	if err := db.MoveMessageAndQueue(t.Context(), messageID, "victim-inbox", "victim-archive"); err != nil {
		t.Fatalf("MoveMessageAndQueue() error = %v", err)
	}
	fake := &fakeMessageMutationIMAPClient{moveErr: errors.New("temporary MOVE failure")}
	h.messageMutationIMAPFactory = func(context.Context, *models.AccountConfig, string) (messageMutationIMAPClient, error) {
		return fake, nil
	}

	h.runDueMessageMutations(t.Context())
	var mutationID, status string
	if err := db.Read().QueryRow(`SELECT id, status FROM message_mutations WHERE message_id = ? AND kind = 'move'`, messageID).Scan(&mutationID, &status); err != nil {
		t.Fatalf("query failed move: %v", err)
	}
	if status != storage.MessageMutationFailed {
		t.Fatalf("failed move status=%q", status)
	}
	if _, err := db.Write().Exec(`UPDATE message_mutations SET next_attempt_at = CURRENT_TIMESTAMP WHERE id = ?`, mutationID); err != nil {
		t.Fatalf("make move retry due: %v", err)
	}
	fake.moveErr = nil
	fake.moveUID = 84
	h.runDueMessageMutations(t.Context())
	if err := db.Read().QueryRow(`SELECT status FROM message_mutations WHERE id = ?`, mutationID).Scan(&status); err != nil {
		t.Fatalf("query retried move: %v", err)
	}
	if status != storage.MessageMutationApplied || fake.moveCalls != 2 {
		t.Fatalf("retried move status=%q calls=%d", status, fake.moveCalls)
	}
}

func seedIMAPDeleteMutationWorker(t *testing.T) (*Handler, *storage.DB, int64) {
	t.Helper()
	h, db := newAccountOwnershipTestHandler(t)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{
		ID: "victim-trash", AccountID: "victim-account", RemoteID: "Trash", Name: "Trash", Role: "trash", Selectable: true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if _, err := db.Write().Exec(`UPDATE folders SET uid_validity = 77 WHERE id = 'victim-trash'`); err != nil {
		t.Fatalf("set UIDVALIDITY: %v", err)
	}
	if err := db.UpsertSyncMessages(t.Context(), []storage.SyncMessage{{
		AccountID: "victim-account", FolderID: "victim-trash", RemoteUID: 42,
		MessageID: "<permanent-delete@example.com>", Subject: "Delete", FromEmail: "sender@example.com",
		DateSent: time.Now(), IsRead: true,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	messageID, err := db.GetMessageLocalIDByInternetID(t.Context(), "victim-account", "<permanent-delete@example.com>")
	if err != nil || messageID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", messageID, err)
	}
	if err := db.PermanentlyDeleteMessageAndQueue(t.Context(), messageID, "victim-trash"); err != nil {
		t.Fatalf("PermanentlyDeleteMessageAndQueue() error = %v", err)
	}
	return h, db, messageID
}

func TestMessageMutationWorkerPermanentlyDeletesGenericIMAPMessage(t *testing.T) {
	h, db, messageID := seedIMAPDeleteMutationWorker(t)
	fake := &fakeMessageMutationIMAPClient{}
	h.messageMutationIMAPFactory = func(context.Context, *models.AccountConfig, string) (messageMutationIMAPClient, error) {
		return fake, nil
	}

	h.runDueMessageMutations(t.Context())

	if fake.deleteCalls != 1 || fake.deleteFolder != "Trash" || len(fake.deleteUIDs) != 1 || fake.deleteUIDs[0] != 42 || fake.deleteValidity != 77 {
		t.Fatalf("IMAP delete calls=%d folder=%q uids=%v validity=%d", fake.deleteCalls, fake.deleteFolder, fake.deleteUIDs, fake.deleteValidity)
	}
	var status string
	if err := db.Read().QueryRow(`SELECT status FROM message_mutations WHERE message_id = ? AND kind = 'delete'`, messageID).Scan(&status); err != nil || status != storage.MessageMutationApplied {
		t.Fatalf("delete mutation status=%q, %v", status, err)
	}
	if _, err := db.RemoveExpungedUIDs(t.Context(), "victim-trash", []uint32{42}); err != nil {
		t.Fatalf("RemoveExpungedUIDs() error = %v", err)
	}
	var remaining int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE message_id = ?`, messageID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("confirmed delete mutations remaining=%d, %v", remaining, err)
	}
}

func TestMessageMutationWorkerStopsIMAPDeleteAfterUIDValidityChange(t *testing.T) {
	h, db, messageID := seedIMAPDeleteMutationWorker(t)
	fake := &fakeMessageMutationIMAPClient{validityChanged: true}
	h.messageMutationIMAPFactory = func(context.Context, *models.AccountConfig, string) (messageMutationIMAPClient, error) {
		return fake, nil
	}

	h.runDueMessageMutations(t.Context())

	var status, lastError string
	if err := db.Read().QueryRow(`SELECT status, last_error FROM message_mutations WHERE message_id = ? AND kind = 'delete'`, messageID).Scan(&status, &lastError); err != nil {
		t.Fatalf("query failed delete: %v", err)
	}
	if status != storage.MessageMutationFailed || !strings.Contains(lastError, "UIDVALIDITY") {
		t.Fatalf("delete status=%q error=%q", status, lastError)
	}
}

func TestMessageMutationWorkerTreatsMissingGmailDeleteAsSuccess(t *testing.T) {
	ctx := t.Context()
	h, db := newGmailAPITestHandler(t, ctx)
	messageID := seedGmailAPIMessage(t, ctx, db, []storage.UpsertFolderInput{{
		ID: "acc_trash", AccountID: "acc", RemoteID: "Trash", ProviderRemoteID: "TRASH", Name: "Trash", Role: "trash", Selectable: true,
	}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/users/me/messages/gmail-msg-1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		http.Error(w, "already deleted", http.StatusNotFound)
	}))
	defer server.Close()
	previousBase := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBase })
	fakeIMAP := &fakeMessageMutationIMAPClient{}
	h.messageMutationIMAPFactory = func(context.Context, *models.AccountConfig, string) (messageMutationIMAPClient, error) {
		return fakeIMAP, nil
	}
	if err := db.PermanentlyDeleteMessageAndQueue(ctx, messageID, "acc_trash"); err != nil {
		t.Fatalf("PermanentlyDeleteMessageAndQueue() error = %v", err)
	}

	h.runDueMessageMutations(ctx)

	if fakeIMAP.deleteCalls != 0 {
		t.Fatalf("Gmail delete fell through to IMAP: %d", fakeIMAP.deleteCalls)
	}
	var remaining int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE message_id = ? AND kind = 'delete'`, messageID).Scan(&remaining); err != nil {
		t.Fatalf("query Gmail delete: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("already missing Gmail delete remaining=%d", remaining)
	}
}

func TestDeleteThreadOnlyQueuesMessagesFromTheViewedTrashFolder(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	h.syncer = mailpkg.NewSyncOrchestrator(db, nil, nil, nil)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{
		{ID: "victim-inbox", AccountID: "victim-account", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "victim-trash", AccountID: "victim-account", RemoteID: "Trash", Name: "Trash", Role: "trash", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if _, err := db.Write().Exec(`UPDATE folders SET uid_validity = 77 WHERE id = 'victim-trash'`); err != nil {
		t.Fatalf("set UIDVALIDITY: %v", err)
	}
	if err := db.UpsertSyncMessages(t.Context(), []storage.SyncMessage{
		{AccountID: "victim-account", FolderID: "victim-trash", RemoteUID: 42, MessageID: "<trash-thread@example.com>", Subject: "Thread", FromEmail: "sender@example.com", DateSent: time.Now()},
		{AccountID: "victim-account", FolderID: "victim-inbox", RemoteUID: 43, MessageID: "<inbox-thread@example.com>", Subject: "Thread", FromEmail: "sender@example.com", DateSent: time.Now()},
	}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	trashID, _ := db.GetMessageLocalIDByInternetID(t.Context(), "victim-account", "<trash-thread@example.com>")
	inboxID, _ := db.GetMessageLocalIDByInternetID(t.Context(), "victim-account", "<inbox-thread@example.com>")
	if _, err := db.Write().Exec(`UPDATE messages SET thread_id = 'shared-thread' WHERE id IN (?, ?)`, trashID, inboxID); err != nil {
		t.Fatalf("set thread IDs: %v", err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/messages/"+strconv.FormatInt(trashID, 10)+"/thread?folder_id=victim-trash", nil)
	req.SetPathValue("id", strconv.FormatInt(trashID, 10))
	recorder := httptest.NewRecorder()

	h.handleDeleteThread(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("delete thread status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var trashDeletes, inboxDeletes, inboxDeleted int
	_ = db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE message_id = ? AND kind = 'delete'`, trashID).Scan(&trashDeletes)
	_ = db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE message_id = ? AND kind = 'delete'`, inboxID).Scan(&inboxDeletes)
	_ = db.Read().QueryRow(`SELECT is_deleted FROM message_folder_state WHERE message_id = ? AND folder_id = 'victim-inbox'`, inboxID).Scan(&inboxDeleted)
	if trashDeletes != 1 || inboxDeletes != 0 || inboxDeleted != 0 {
		t.Fatalf("folder-scoped delete trash_ops=%d inbox_ops=%d inbox_deleted=%d", trashDeletes, inboxDeletes, inboxDeleted)
	}
}

func TestEmailPartialUsesViewedFolderForDeleteAction(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{
		{ID: "victim-inbox", AccountID: "victim-account", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "victim-trash", AccountID: "victim-account", RemoteID: "Trash", Name: "Trash", Role: "trash", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if err := db.UpsertSyncMessages(t.Context(), []storage.SyncMessage{
		{AccountID: "victim-account", FolderID: "victim-inbox", RemoteUID: 1, MessageID: "<viewed-folder@example.com>", Subject: "Folder context", FromEmail: "sender@example.com", DateSent: time.Now()},
		{AccountID: "victim-account", FolderID: "victim-trash", RemoteUID: 2, MessageID: "<viewed-folder@example.com>", Subject: "Folder context", FromEmail: "sender@example.com", DateSent: time.Now()},
	}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	messageID, err := db.GetMessageLocalIDByInternetID(t.Context(), "victim-account", "<viewed-folder@example.com>")
	if err != nil || messageID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", messageID, err)
	}

	render := func(folderID string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/email/"+strconv.FormatInt(messageID, 10)+"?folder_id="+folderID, nil)
		req.SetPathValue("id", strconv.FormatInt(messageID, 10))
		recorder := httptest.NewRecorder()
		h.handleEmailPartial(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("email partial for %s status=%d body=%s", folderID, recorder.Code, recorder.Body.String())
		}
		return recorder.Body.String()
	}

	trashHTML := render("victim-trash")
	for _, want := range []string{"Permanently delete", "border-red-500/35", "deleteMessage"} {
		if !strings.Contains(trashHTML, want) {
			t.Fatalf("Trash email partial missing %q: %s", want, trashHTML)
		}
	}
	unifiedTrashHTML := render("trash")
	if !strings.Contains(unifiedTrashHTML, "Permanently delete") || !strings.Contains(unifiedTrashHTML, "border-red-500/35") {
		t.Fatalf("unified Trash email partial lost permanent-delete treatment: %s", unifiedTrashHTML)
	}
	inboxHTML := render("victim-inbox")
	if strings.Contains(inboxHTML, "Permanently delete") || strings.Contains(inboxHTML, "border-red-500/35") {
		t.Fatalf("Inbox email partial kept Trash delete treatment: %s", inboxHTML)
	}
}
