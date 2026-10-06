package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDraftPublicationRollbackPreservesDraftAndWaitingDelivery(t *testing.T) {
	db, id, state := seedIMAPDraftSyncTest(t)
	oldPath := filepath.Join(t.TempDir(), "old.txt")
	newPath := filepath.Join(t.TempDir(), "new.txt")
	if err := os.WriteFile(oldPath, []byte("old body"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("new body"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMessageBodyInternal(t.Context(), id, oldPath, "", "", "old snippet"); err != nil {
		t.Fatal(err)
	}
	oldOp := queueDraftRevision(t, db, state, "old-revision", []byte("old remote body"))
	input := outgoingTestInput(state.AccountID, id, time.Now().Add(time.Hour), true)
	input.DraftID = state.DraftKey
	send, err := db.QueueOutgoingSend(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input.ID = send.ID
	input.MIMEData = []byte("new delivery body")
	input.MessageJSON = []byte(`{"message_id":"stable-delivery","text_body":"new body"}`)
	draft := DraftMessageInput{AccountID: state.AccountID, FolderID: state.FolderID, InternetMessageID: state.DraftKey, Subject: "new subject", FromEmail: "user@example.com", ToRecipients: []Recipient{{Email: "new@example.com"}}}
	stage := func(localID int64) (DraftPublication, error) {
		if localID != id {
			t.Fatal("draft ID changed")
		}
		return DraftPublication{TextPath: newPath, Attachments: []AttachmentRow{{Filename: "new.txt", StoragePath: newPath}}, Sync: QueueIMAPDraftUpsertInput{State: state, RevisionToken: "new-revision", MIMEData: []byte("new remote body")}, Pending: &input}, nil
	}
	if _, err := db.Write().Exec(`CREATE TRIGGER reject_waiting_send BEFORE UPDATE ON outgoing_sends BEGIN SELECT RAISE(ABORT,'injected publication failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PublishDraft(t.Context(), draft, stage); err == nil {
		t.Fatal("injected publication succeeded")
	}
	email, err := db.GetEmailByIDInternal(t.Context(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if email.Subject != "Local draft" || email.TextBody != "old body" || len(email.Attachments) != 0 {
		t.Fatalf("failed publication changed draft: %+v", email)
	}
	op, err := db.GetIMAPDraftOperation(t.Context(), oldOp.ID)
	if err != nil || op.RevisionToken != "old-revision" {
		t.Fatalf("remote intent changed: %+v %v", op, err)
	}
	still, err := db.GetOutgoingSend(t.Context(), send.ID)
	if err != nil || !bytes.Equal(still.MIMEData, send.MIMEData) {
		t.Fatal("waiting delivery changed after rollback", err)
	}
	if _, err := db.Write().Exec(`DROP TRIGGER reject_waiting_send`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PublishDraft(t.Context(), draft, stage); err != nil {
		t.Fatal(err)
	}
	email, err = db.GetEmailByIDInternal(t.Context(), "1")
	if err != nil || email.Subject != "new subject" || email.TextBody != "new body" || len(email.Attachments) != 1 {
		t.Fatal("complete publication failed", err)
	}
	still, err = db.GetOutgoingSend(t.Context(), send.ID)
	if err != nil || !bytes.Equal(still.MIMEData, input.MIMEData) {
		t.Fatal("waiting delivery was not updated", err)
	}
}

func TestDiscardDraftRollbackKeepsWaitingSendAndRemoteRevision(t *testing.T) {
	db, id, state := seedIMAPDraftSyncTest(t)
	op := queueDraftRevision(t, db, state, "revision", []byte("draft"))
	send, err := db.QueueOutgoingSend(t.Context(), outgoingTestInput(state.AccountID, id, time.Now().Add(time.Hour), true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`CREATE TRIGGER reject_draft_delete BEFORE DELETE ON messages BEGIN SELECT RAISE(ABORT,'injected discard failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DiscardIMAPDraft(t.Context(), state.AccountID, state.DraftKey); err == nil {
		t.Fatal("injected discard succeeded")
	}
	still, err := db.GetOutgoingSend(t.Context(), send.ID)
	if err != nil || still.Status != OutgoingSendPending {
		t.Fatal("failed discard canceled delivery", err)
	}
	old, err := db.GetIMAPDraftOperation(t.Context(), op.ID)
	if err != nil || old.Kind != IMAPDraftOperationUpsert {
		t.Fatal("failed discard changed remote intent", err)
	}
	if _, err := db.Write().Exec(`DROP TRIGGER reject_draft_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DiscardIMAPDraft(t.Context(), state.AccountID, state.DraftKey); err != nil {
		t.Fatal(err)
	}
	still, err = db.GetOutgoingSend(t.Context(), send.ID)
	if err != nil || still.Status != OutgoingSendCanceled || len(still.MIMEData) != 0 {
		t.Fatal("discard retained waiting payload", err)
	}
	old, err = db.GetIMAPDraftOperation(t.Context(), op.ID)
	if err != nil || old.Kind != IMAPDraftOperationDelete {
		t.Fatal("discard lost remote delete", err)
	}
}

func TestAccountMailQueueClaimsRecoveryAndDeadlines(t *testing.T) {
	db, _, state := seedIMAPDraftSyncTest(t)
	if _, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,provider,email_address) VALUES('other','default','imap','other@example.com')`); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{state.AccountID, "other"} {
		for i := 0; i < 2; i++ {
			if _, err := db.QueueOutgoingSend(t.Context(), outgoingTestInput(account, 0, time.Now().Add(-time.Minute), false)); err != nil {
				t.Fatal(err)
			}
		}
	}
	queueDraftRevision(t, db, state, "revision", []byte("draft"))
	for _, account := range []string{state.AccountID, "other"} {
		claimed, err := db.ClaimDueOutgoingSendsForAccount(t.Context(), account, time.Now(), 10)
		if err != nil || len(claimed) != 2 {
			t.Fatalf("account claim %s: %d %v", account, len(claimed), err)
		}
		if err := db.CompleteOutgoingSend(t.Context(), claimed[0].ID, "<sent@example.com>", true); err != nil {
			t.Fatal(err)
		}
		copies, err := db.ClaimDueSentCopiesForAccount(t.Context(), account, time.Now().Add(time.Second), 10)
		if err != nil || len(copies) != 1 || copies[0].AccountID != account {
			t.Fatal("Sent claim crossed accounts", err)
		}
	}
	if drafts, err := db.ClaimDueIMAPDraftOperationsForAccount(t.Context(), state.AccountID, time.Now().Add(time.Second), 10); err != nil || len(drafts) != 1 {
		t.Fatal("draft claim", err)
	}
	if err := db.RecoverAccountMailQueue(t.Context(), state.AccountID); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		q    string
		want int
	}{
		{`SELECT COUNT(*) FROM outgoing_sends WHERE account_id='other' AND status='sending'`, 1},
		{`SELECT COUNT(*) FROM outgoing_sends WHERE account_id='other' AND sent_copy_status='copying'`, 1},
		{`SELECT COUNT(*) FROM outgoing_sends WHERE account_id='acc' AND status='ambiguous'`, 1},
		{`SELECT COUNT(*) FROM outgoing_sends WHERE account_id='acc' AND sent_copy_status='ambiguous'`, 1},
		{`SELECT COUNT(*) FROM imap_draft_operations WHERE account_id='acc' AND status='ambiguous'`, 1},
	} {
		var count int
		if err := db.Read().QueryRow(check.q).Scan(&count); err != nil || count != check.want {
			t.Fatalf("recovery count=%d want=%d %v", count, check.want, err)
		}
	}
	if _, err := db.Write().Exec(`DELETE FROM imap_draft_operations WHERE account_id='acc'; UPDATE outgoing_sends SET sent_copy_status='complete' WHERE account_id='acc' AND status='sent'`); err != nil {
		t.Fatal(err)
	}
	if next, err := db.NextAccountMailQueueAttempt(t.Context(), state.AccountID); err != nil || !next.IsZero() {
		t.Fatalf("ambiguous SMTP must not auto-retry: %s %v", next, err)
	}
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	queued, err := db.QueueOutgoingSend(t.Context(), outgoingTestInput(state.AccountID, 0, at, true))
	if err != nil {
		t.Fatal(err)
	}
	if next, err := db.NextAccountMailQueueAttempt(t.Context(), state.AccountID); err != nil || !next.Equal(at) {
		t.Fatalf("schedule deadline %s want %s %v", next, at, err)
	}
	backoff := at.Add(time.Hour)
	if _, err := db.Write().Exec(`UPDATE outgoing_sends SET next_attempt_at=? WHERE id=?`, backoff, queued.ID); err != nil {
		t.Fatal(err)
	}
	if next, err := db.NextAccountMailQueueAttempt(t.Context(), state.AccountID); err != nil || !next.Equal(backoff) {
		t.Fatalf("backoff deadline %s want %s %v", next, backoff, err)
	}
	if _, err := db.ClaimDueOutgoingSendsForAccount(t.Context(), "", time.Now(), 1); err == nil {
		t.Fatal("empty account claim accepted")
	}
}

func TestDraftPublicationRejectsNonDraftIdentityAndForeignFolder(t *testing.T) {
	db, id, state := seedIMAPDraftSyncTest(t)
	if _, err := db.Write().Exec(`UPDATE message_folder_state SET is_draft=0 WHERE message_id=?`, id); err != nil {
		t.Fatal(err)
	}
	stage := func(int64) (DraftPublication, error) {
		t.Fatal("invalid identity reached staging")
		return DraftPublication{}, nil
	}
	draft := DraftMessageInput{AccountID: state.AccountID, FolderID: state.FolderID, InternetMessageID: state.DraftKey}
	if _, err := db.PublishDraft(t.Context(), draft, stage); err == nil {
		t.Fatal("non-draft overwritten")
	}
	draft.InternetMessageID = "<new@example.com>"
	draft.FolderID = "missing"
	if _, err := db.PublishDraft(t.Context(), draft, stage); err == nil {
		t.Fatal("foreign folder accepted")
	}
}

func TestDraftDeletionKeepsUIDAndValidityTogether(t *testing.T) {
	db, id, state := seedIMAPDraftSyncTest(t)
	state.RemoteUID = 10
	state.UIDValidity = 100
	queueDraftRevision(t, db, state, "revision", []byte("draft"))
	if _, err := db.Write().Exec(`UPDATE folders SET uid_validity=200 WHERE id='drafts'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`UPDATE message_folder_state SET remote_uid=10 WHERE message_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DiscardIMAPDraft(t.Context(), state.AccountID, state.DraftKey); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimDueIMAPDraftOperationsForAccount(t.Context(), state.AccountID, time.Now().Add(time.Second), 1)
	if err != nil || len(claimed) != 1 {
		t.Fatal("delete claim", err)
	}
	if claimed[0].State.RemoteUID != 10 || claimed[0].State.UIDValidity != 100 {
		t.Fatalf("tracked UID relabeled with replacement mailbox validity: %+v", claimed[0].State)
	}
}
