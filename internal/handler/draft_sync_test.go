package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	imapclient "github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

func TestHandleComposeDraftQueuesGenericIMAPRevision(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	h.blobStore = store.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	h.syncer = mailpkg.NewSyncOrchestrator(db, h.accountStore, h.blobStore, nil)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{
		ID: "victim-drafts", AccountID: "victim-account", RemoteID: "Drafts", Name: "Drafts", Role: "drafts", Selectable: true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	form := url.Values{
		"account_id": {"victim-account"},
		"to":         {"recipient@example.com"},
		"bcc":        {"hidden@example.com"},
		"subject":    {"Remote draft"},
		"body":       {"Draft body"},
	}
	req := httptest.NewRequest(http.MethodPost, "/compose/draft", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "owner", Email: "owner@example.com"}))
	rec := httptest.NewRecorder()

	h.handleComposeDraft(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
	var response map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil || response["draft_id"] == "" {
		t.Fatalf("response = %#v, %v", response, err)
	}
	var operationID string
	if err := db.Read().QueryRow(`SELECT id FROM imap_draft_operations WHERE account_id = 'victim-account'`).Scan(&operationID); err != nil {
		t.Fatalf("query queued draft operation: %v", err)
	}
	operation, err := db.GetIMAPDraftOperation(t.Context(), operationID)
	if err != nil {
		t.Fatalf("GetIMAPDraftOperation() error = %v", err)
	}
	raw := string(operation.MIMEData)
	if operation.Kind != storage.IMAPDraftOperationUpsert || !strings.Contains(raw, "X-Gofer-Draft-Revision: ") || !strings.Contains(raw, "\r\nBcc: hidden@example.com\r\n") {
		t.Fatalf("queued draft operation = %#v MIME=%q", operation, raw)
	}
}

func TestIMAPDraftWorkerAppendsBeforeDeletingOldRevision(t *testing.T) {
	h, db, operation, localMessageID := seedIMAPDraftWorkerOperation(t, 10, 100)
	fake := &fakeSentCopyIMAPClient{appendResult: imapclient.AppendResult{UID: 20, UIDValidity: 100}}
	h.sentCopyIMAPFactory = func(context.Context, *models.AccountConfig, string) (sentCopyIMAPClient, error) { return fake, nil }

	h.runDueIMAPDraftOperations(t.Context())

	if fake.appendCalls != 1 || fake.deleteCalls != 1 || len(fake.deleteUIDs) != 1 || fake.deleteUIDs[0] != 10 || fake.deleteValidity != 100 {
		t.Fatalf("IMAP calls append=%d delete=%d uids=%v validity=%d", fake.appendCalls, fake.deleteCalls, fake.deleteUIDs, fake.deleteValidity)
	}
	if len(fake.flags) != 2 || !containsIMAPFlag(fake.flags, goimap.FlagSeen) || !containsIMAPFlag(fake.flags, goimap.FlagDraft) {
		t.Fatalf("append flags = %#v", fake.flags)
	}
	if _, err := db.GetIMAPDraftOperation(t.Context(), operation.ID); err != sql.ErrNoRows {
		t.Fatalf("completed operation error = %v, want sql.ErrNoRows", err)
	}
	state, err := db.GetIMAPDraftState(t.Context(), "victim-account", "<draft-sync@example.com>")
	if err != nil || state == nil || state.RemoteUID != 20 || state.UIDValidity != 100 {
		t.Fatalf("draft state = %#v, %v", state, err)
	}
	var remoteUID uint32
	if err := db.Read().QueryRow(`SELECT remote_uid FROM message_folder_state WHERE message_id = ? AND folder_id = 'victim-drafts'`, localMessageID).Scan(&remoteUID); err != nil || remoteUID != 20 {
		t.Fatalf("local remote UID = %d, %v", remoteUID, err)
	}
}

func TestInterruptedIMAPDraftAppendSearchesRevisionBeforeAppendingAgain(t *testing.T) {
	h, db, operation, _ := seedIMAPDraftWorkerOperation(t, 10, 100)
	if claimed, err := db.ClaimDueIMAPDraftOperations(t.Context(), time.Now(), 1); err != nil || len(claimed) != 1 {
		t.Fatalf("claim operation = %#v, %v", claimed, err)
	}
	if count, err := db.MarkInterruptedIMAPDraftOperationsAmbiguous(t.Context(), "interrupted"); err != nil || count != 1 {
		t.Fatalf("mark interrupted = %d, %v", count, err)
	}
	fake := &fakeSentCopyIMAPClient{findUID: 20, findUIDValidity: 100}
	h.sentCopyIMAPFactory = func(context.Context, *models.AccountConfig, string) (sentCopyIMAPClient, error) { return fake, nil }

	h.runDueIMAPDraftOperations(t.Context())

	if fake.findCalls != 1 || fake.findHeader != imapDraftRevisionHeader || fake.findValue != operation.RevisionToken || fake.appendCalls != 0 {
		t.Fatalf("recovery find=%d header=%q value=%q append=%d", fake.findCalls, fake.findHeader, fake.findValue, fake.appendCalls)
	}
	if fake.deleteCalls != 1 || len(fake.deleteUIDs) != 1 || fake.deleteUIDs[0] != 10 {
		t.Fatalf("old draft cleanup calls=%d uids=%v", fake.deleteCalls, fake.deleteUIDs)
	}
}

func TestIMAPDraftDeleteUsesTrackedUIDAfterLocalDraftIsGone(t *testing.T) {
	h, db, _, _ := seedIMAPDraftWorkerOperation(t, 30, 200)
	state, err := db.GetIMAPDraftState(t.Context(), "victim-account", "<draft-sync@example.com>")
	if err != nil || state == nil {
		t.Fatalf("GetIMAPDraftState() = %#v, %v", state, err)
	}
	deleteOperation, err := db.QueueIMAPDraftDelete(t.Context(), *state)
	if err != nil {
		t.Fatalf("QueueIMAPDraftDelete() error = %v", err)
	}
	if _, err := db.DeleteDraftMessage(t.Context(), state.AccountID, state.DraftKey); err != nil {
		t.Fatalf("DeleteDraftMessage() error = %v", err)
	}
	fake := &fakeSentCopyIMAPClient{}
	h.sentCopyIMAPFactory = func(context.Context, *models.AccountConfig, string) (sentCopyIMAPClient, error) { return fake, nil }

	h.runDueIMAPDraftOperations(t.Context())

	if fake.deleteCalls != 1 || len(fake.deleteUIDs) != 1 || fake.deleteUIDs[0] != 30 || fake.deleteValidity != 200 {
		t.Fatalf("delete calls=%d uids=%v validity=%d", fake.deleteCalls, fake.deleteUIDs, fake.deleteValidity)
	}
	if _, err := db.GetIMAPDraftOperation(t.Context(), deleteOperation.ID); err != sql.ErrNoRows {
		t.Fatalf("delete operation error = %v, want sql.ErrNoRows", err)
	}
	if state, err := db.GetIMAPDraftState(t.Context(), "victim-account", "<draft-sync@example.com>"); err != nil || state != nil {
		t.Fatalf("state after deletion = %#v, %v", state, err)
	}
}

func seedIMAPDraftWorkerOperation(t *testing.T, oldUID, uidValidity uint32) (*Handler, *storage.DB, storage.IMAPDraftOperation, int64) {
	t.Helper()
	h, db := newAccountOwnershipTestHandler(t)
	if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{
		ID: "victim-drafts", AccountID: "victim-account", RemoteID: "Drafts", Name: "Drafts", Role: "drafts", Selectable: true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if _, err := db.Write().Exec(`UPDATE folders SET uid_validity = ? WHERE id = 'victim-drafts'`, uidValidity); err != nil {
		t.Fatalf("set UID validity: %v", err)
	}
	localMessageID, err := db.SaveDraftMessage(t.Context(), storage.DraftMessageInput{
		AccountID: "victim-account", FolderID: "victim-drafts", InternetMessageID: "<draft-sync@example.com>",
		Subject: "Draft sync", FromEmail: "owner@example.com", Date: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("SaveDraftMessage() error = %v", err)
	}
	if oldUID > 0 {
		if err := db.SetMessageFolderRemoteUID(t.Context(), localMessageID, "victim-drafts", oldUID); err != nil {
			t.Fatalf("SetMessageFolderRemoteUID() error = %v", err)
		}
	}
	to, _ := message.ParseAddressList("recipient@example.com")
	msg := &message.OutgoingMessage{
		FromEmail: "owner@example.com", To: to, Subject: "Draft sync", TextBody: "latest body",
		MessageID: "<draft-sync@example.com>", Date: time.Now().UTC(),
	}
	raw, err := message.BuildMIMEMessageForIMAPDraft(msg, "revision-worker")
	if err != nil {
		t.Fatalf("BuildMIMEMessageForIMAPDraft() error = %v", err)
	}
	operation, err := db.QueueIMAPDraftUpsert(t.Context(), storage.QueueIMAPDraftUpsertInput{
		State: storage.IMAPDraftState{
			AccountID: "victim-account", DraftKey: msg.MessageID, LocalMessageID: localMessageID,
			FolderID: "victim-drafts", FolderRemoteName: "Drafts", RemoteUID: oldUID, UIDValidity: uidValidity,
		},
		RevisionToken: "revision-worker", MIMEData: raw, MessageDate: msg.Date,
	})
	if err != nil {
		t.Fatalf("QueueIMAPDraftUpsert() error = %v", err)
	}
	return h, db, operation, localMessageID
}

func containsIMAPFlag(flags []goimap.Flag, want goimap.Flag) bool {
	for _, flag := range flags {
		if flag == want {
			return true
		}
	}
	return false
}
