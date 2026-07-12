package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func seedMailOperationsTest(t *testing.T) (*DB, string, string) {
	t.Helper()
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users (id, email, name) VALUES ('other', 'other@example.com', 'Other');
		INSERT INTO accounts (id, user_id, provider, email_address) VALUES
			('acc', 'default', 'imap', 'user@example.com'),
			('other-acc', 'other', 'imap', 'other@example.com');
		INSERT INTO messages (id, account_id, internet_message_id, subject, from_email) VALUES
			(1, 'acc', '<operation@example.com>', 'Operation', 'sender@example.com'),
			(2, 'other-acc', '<other-operation@example.com>', 'Other operation', 'sender@example.com');
		INSERT INTO message_mutations (
			id, account_id, message_id, folder_id, provider_type, kind, target_value,
			status, attempt_count, last_error, next_attempt_at
		) VALUES
			('mut-1', 'acc', 1, '', 'imap', 'read', 0, 'failed', 2,
			 'access_token=super-secret authorization: Bearer abc123', CURRENT_TIMESTAMP),
			('other-mut', 'other-acc', 2, '', 'imap', 'starred', 1, 'failed', 1,
			 'other error', CURRENT_TIMESTAMP);`); err != nil {
		t.Fatalf("seed accounts/messages/mutations: %v", err)
	}

	if err := db.EnqueueLabelMutation(ctx, "acc", 1, "", LabelProviderGmail, LabelMutationAdd, "Projects", errors.New("remote label service is busy")); err != nil {
		t.Fatalf("EnqueueLabelMutation() error = %v", err)
	}
	draft, err := db.QueueIMAPDraftUpsert(ctx, QueueIMAPDraftUpsertInput{
		State:         IMAPDraftState{AccountID: "acc", DraftKey: "draft-1", FolderRemoteName: "Drafts"},
		RevisionToken: "revision-1", MIMEData: []byte("draft mime"), MessageDate: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("QueueIMAPDraftUpsert() error = %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		UPDATE imap_draft_operations
		SET status = 'ambiguous', attempt_count = 3, last_error = 'draft connection lost', locked_at = CURRENT_TIMESTAMP
		WHERE id = ?`, draft.ID); err != nil {
		t.Fatalf("mark draft ambiguous: %v", err)
	}

	send, err := db.QueueOutgoingSend(ctx, QueueOutgoingSendInput{
		ID: "send-1", AccountID: "acc", Transport: OutgoingTransportSMTP,
		EnvelopeFrom: "user@example.com", EnvelopeRecipients: []string{"friend@example.com"},
		MIMEData: []byte("message mime"), MessageJSON: []byte(`{"subject":"hello"}`),
		SendAfter: time.Now().UTC().Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("QueueOutgoingSend() error = %v", err)
	}
	if claimed, err := db.ClaimDueOutgoingSends(ctx, time.Now().UTC(), 1); err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDueOutgoingSends() = %#v, %v", claimed, err)
	}
	if err := db.CompleteOutgoingSend(ctx, send.ID, "provider-message-1", true); err != nil {
		t.Fatalf("CompleteOutgoingSend() error = %v", err)
	}
	if claimed, err := db.ClaimDueSentCopies(ctx, time.Now().UTC(), 1); err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDueSentCopies() = %#v, %v", claimed, err)
	}
	if err := db.FinishSentCopyWithError(ctx, send.ID, SentCopyFailed, "sent copy provider busy", time.Now().UTC()); err != nil {
		t.Fatalf("FinishSentCopyWithError() error = %v", err)
	}

	return db, draft.ID, send.ID
}

func TestMailOperationsAreUserScopedAndRetrySafe(t *testing.T) {
	ctx := context.Background()
	db, draftID, sentCopyID := seedMailOperationsTest(t)

	operations, err := db.ListMailOperationsForUser(ctx, "default")
	if err != nil {
		t.Fatalf("ListMailOperationsForUser() error = %v", err)
	}
	if len(operations) != 4 {
		t.Fatalf("default operations = %d, want 4: %#v", len(operations), operations)
	}
	seen := make(map[string]models.MailOperationSummary, len(operations))
	for _, operation := range operations {
		seen[operation.ID] = operation
		if operation.AccountEmail != "user@example.com" {
			t.Fatalf("operation account email = %q, want owner email", operation.AccountEmail)
		}
		if strings.Contains(operation.LastError, "super-secret") || strings.Contains(operation.LastError, "abc123") {
			t.Fatalf("operation error leaked a secret: %q", operation.LastError)
		}
	}
	for _, id := range []string{"message_mutation:mut-1", "label_mutation:1", "imap_draft:" + draftID, "sent_copy:" + sentCopyID} {
		if _, ok := seen[id]; !ok {
			t.Fatalf("missing operation %q in %#v", id, seen)
		}
	}
	if !seen["message_mutation:mut-1"].CanRetry || !seen["label_mutation:1"].CanRetry || !seen["imap_draft:"+draftID].CanReconcile || !seen["sent_copy:"+sentCopyID].CanRetry {
		t.Fatalf("retry/reconcile affordances = %#v", seen)
	}

	if _, err := db.GetMailOperationForUser(ctx, "other", "message_mutation:mut-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign GetMailOperationForUser() error = %v, want sql.ErrNoRows", err)
	}
	if _, err := db.RetryMailOperationForUser(ctx, "other", "message_mutation:mut-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign RetryMailOperationForUser() error = %v, want sql.ErrNoRows", err)
	}

	retried, err := db.RetryMailOperationForUser(ctx, "default", "message_mutation:mut-1")
	if err != nil || retried.State != MessageMutationPending || retried.ID != "message_mutation:mut-1" {
		t.Fatalf("retried message mutation = %#v, %v", retried, err)
	}
	if _, err := db.RetryMailOperationForUser(ctx, "default", "message_mutation:mut-1"); !errors.Is(err, ErrMailOperationNotRetryable) {
		t.Fatalf("retrying pending message mutation error = %v, want not retryable", err)
	}

	if _, err := db.RetryMailOperationForUser(ctx, "default", "label_mutation:1"); err != nil {
		t.Fatalf("retry label mutation error = %v", err)
	}
	if _, err := db.RetryMailOperationForUser(ctx, "default", "imap_draft:"+draftID); err != nil {
		t.Fatalf("retry ambiguous draft error = %v", err)
	}
	if _, err := db.RetryMailOperationForUser(ctx, "default", "sent_copy:"+sentCopyID); err != nil {
		t.Fatalf("retry sent copy error = %v", err)
	}

	var deliveryStatus, sentCopyStatus string
	if err := db.Read().QueryRow(`SELECT status, sent_copy_status FROM outgoing_sends WHERE id = ?`, sentCopyID).Scan(&deliveryStatus, &sentCopyStatus); err != nil {
		t.Fatalf("query sent copy retry: %v", err)
	}
	if deliveryStatus != OutgoingSendSent || sentCopyStatus != SentCopyFailed {
		t.Fatalf("sent copy retry changed delivery state: status=%q sent_copy_status=%q", deliveryStatus, sentCopyStatus)
	}
}

func TestMailOperationsAdminStatusIsAggregatedAndMasked(t *testing.T) {
	ctx := context.Background()
	db, draftID, sentCopyID := seedMailOperationsTest(t)
	status, err := db.ListMailOperationsAdminStatus(ctx)
	if err != nil {
		t.Fatalf("ListMailOperationsAdminStatus() error = %v", err)
	}
	if status.Total != 5 || status.ActionRequired != 5 {
		t.Fatalf("admin totals = %#v, want five retained/action-required operations", status)
	}
	if len(status.ByAccount) != 2 || len(status.ByType) != 4 {
		t.Fatalf("admin aggregates = %#v, want two accounts and four types", status)
	}
	for _, account := range status.ByAccount {
		if strings.Contains(account.AccountLabel, "@example.com") && strings.Contains(account.AccountLabel, "user@example.com") {
			t.Fatalf("admin account label leaked full email: %q", account.AccountLabel)
		}
		if strings.Contains(account.AccountLabel, "user@example.com") || strings.Contains(account.AccountLabel, "other@example.com") {
			t.Fatalf("admin account label leaked full address: %q", account.AccountLabel)
		}
	}
	if _, err := db.GetMailOperationForUser(ctx, "default", "imap_draft:"+draftID); err != nil {
		t.Fatalf("GetMailOperationForUser(draft) error = %v", err)
	}
	if _, err := db.GetMailOperationForUser(ctx, "default", "sent_copy:"+sentCopyID); err != nil {
		t.Fatalf("GetMailOperationForUser(sent copy) error = %v", err)
	}
}
