package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type fakeMessageMutationIMAPClient struct {
	storeErr   error
	storeCalls int
	folder     string
	uid        uint32
	op         goimap.StoreFlagsOp
	flags      []goimap.Flag
	findUID    uint32
}

func (c *fakeMessageMutationIMAPClient) StoreFlags(_ context.Context, folder string, uid uint32, op goimap.StoreFlagsOp, flags []goimap.Flag) error {
	c.storeCalls++
	c.folder = folder
	c.uid = uid
	c.op = op
	c.flags = append([]goimap.Flag(nil), flags...)
	return c.storeErr
}

func (c *fakeMessageMutationIMAPClient) FindUIDByMessageID(context.Context, string, string) (uint32, error) {
	return c.findUID, nil
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
