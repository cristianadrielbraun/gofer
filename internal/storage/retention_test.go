package storage

import (
	"context"
	"testing"
	"time"
)

func insertRetentionOutgoing(t *testing.T, db *DB, id, status, sentCopyStatus, draftID string, messageID any, updatedAt time.Time, mimeData any, messageJSON string) {
	t.Helper()
	_, err := db.Write().ExecContext(context.Background(), `
		INSERT INTO outgoing_sends (
			id, account_id, message_id, draft_id, transport, envelope_from,
			envelope_recipients, mime_data, message_json, send_after, next_attempt_at,
			status, sent_copy_status, created_at, updated_at
		) VALUES (?, 'acc', ?, ?, 'smtp', 'user@example.com', '[]', ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, messageID, draftID, mimeData, messageJSON, updatedAt, updatedAt, status, sentCopyStatus, updatedAt, updatedAt)
	if err != nil {
		t.Fatalf("insert outgoing %s: %v", id, err)
	}
}

func TestPruneDurableMailJobsKeepsRecoveryRowsAndReferences(t *testing.T) {
	db := newContactsTestDB(t)
	ctx := context.Background()
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	messageResult, err := db.Write().ExecContext(ctx, `
		INSERT INTO messages (account_id, internet_message_id, subject, from_email)
		VALUES ('acc', '<retention@example.com>', 'Retention', 'sender@example.com')`)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}
	messageID, err := messageResult.LastInsertId()
	if err != nil {
		t.Fatalf("message id: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO attachments (message_id, filename, storage_path)
		VALUES (?, 'receipt.pdf', 'messages/retention/receipt.pdf')`, messageID); err != nil {
		t.Fatalf("insert attachment: %v", err)
	}

	now := time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)
	old := now.Add(-OutgoingSendRetentionPeriod - time.Hour)
	recent := now.Add(-OutgoingSendRetentionPeriod + time.Hour)
	insertRetentionOutgoing(t, db, "old-complete", OutgoingSendSent, SentCopyComplete, "", messageID, old, nil, "")
	insertRetentionOutgoing(t, db, "old-canceled", OutgoingSendCanceled, SentCopyNotRequired, "", nil, old, []byte("cancelled payload"), `{"message_id":"cancelled"}`)
	insertRetentionOutgoing(t, db, "recent-complete", OutgoingSendSent, SentCopyNotRequired, "", nil, recent, nil, "")
	insertRetentionOutgoing(t, db, "old-pending", OutgoingSendPending, SentCopyNotRequired, "", nil, old, []byte("pending"), `{"message_id":"pending"}`)
	insertRetentionOutgoing(t, db, "old-sending", OutgoingSendSending, SentCopyNotRequired, "", nil, old, []byte("sending"), `{"message_id":"sending"}`)
	insertRetentionOutgoing(t, db, "old-failed", OutgoingSendFailed, SentCopyNotRequired, "", nil, old, []byte("failed"), `{"message_id":"failed"}`)
	insertRetentionOutgoing(t, db, "old-ambiguous", OutgoingSendAmbiguous, SentCopyNotRequired, "ambiguous-draft", nil, old, []byte("ambiguous"), `{"message_id":"ambiguous"}`)
	insertRetentionOutgoing(t, db, "old-copying", OutgoingSendSent, SentCopyCopying, "", nil, old, []byte("copying"), `{"message_id":"copying"}`)
	insertRetentionOutgoing(t, db, "old-copy-failed", OutgoingSendSent, SentCopyFailed, "", nil, old, []byte("copy failed"), `{"message_id":"copy-failed"}`)
	insertRetentionOutgoing(t, db, "old-legacy-payload", OutgoingSendSent, SentCopyComplete, "", nil, old, []byte("legacy"), `{"message_id":"legacy"}`)
	insertRetentionOutgoing(t, db, "old-active-draft", OutgoingSendSent, SentCopyComplete, "active-draft", nil, old, nil, "")

	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO imap_draft_states (account_id, draft_key, folder_remote_name)
		VALUES ('acc', 'ambiguous-draft', 'Drafts'), ('acc', 'active-draft', 'Drafts');
		INSERT INTO imap_draft_operations (id, account_id, draft_key, kind, status)
		VALUES ('draft-op', 'acc', 'ambiguous-draft', 'upsert', 'pending'),
		       ('active-draft-op', 'acc', 'active-draft', 'upsert', 'syncing')`); err != nil {
		t.Fatalf("insert active draft operation: %v", err)
	}

	pruned, err := db.PruneDurableMailJobs(ctx, now, RetentionBatchSize)
	if err != nil {
		t.Fatalf("PruneDurableMailJobs() error = %v", err)
	}
	if pruned.OutgoingSends != 2 {
		t.Fatalf("pruned = %#v, want two safe terminal sends", pruned)
	}

	var remaining int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM outgoing_sends WHERE id IN ('old-complete', 'old-canceled')`).Scan(&remaining); err != nil {
		t.Fatalf("count pruned rows: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("safe terminal rows remaining = %d", remaining)
	}
	var protected int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM outgoing_sends WHERE id IN ('recent-complete', 'old-pending', 'old-sending', 'old-failed', 'old-ambiguous', 'old-copying', 'old-copy-failed', 'old-legacy-payload', 'old-active-draft')`).Scan(&protected); err != nil {
		t.Fatalf("count protected rows: %v", err)
	}
	if protected != 9 {
		t.Fatalf("protected rows = %d, want 9", protected)
	}

	var messageCount, attachmentCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE id = ?`, messageID).Scan(&messageCount); err != nil {
		t.Fatalf("count message: %v", err)
	}
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments WHERE message_id = ?`, messageID).Scan(&attachmentCount); err != nil {
		t.Fatalf("count attachment: %v", err)
	}
	if messageCount != 1 || attachmentCount != 1 {
		t.Fatalf("message references after prune: messages=%d attachments=%d", messageCount, attachmentCount)
	}

	second, err := db.PruneDurableMailJobs(ctx, now, RetentionBatchSize)
	if err != nil {
		t.Fatalf("second PruneDurableMailJobs() error = %v", err)
	}
	if second.Total() != 0 {
		t.Fatalf("second prune = %#v, want idempotent zero", second)
	}
}

func TestPruneDurableMailJobsUsesBoundedBatches(t *testing.T) {
	db := newContactsTestDB(t)
	ctx := context.Background()
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	now := time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)
	old := now.Add(-OutgoingSendRetentionPeriod - time.Hour)
	for _, id := range []string{"batch-a", "batch-b", "batch-c"} {
		insertRetentionOutgoing(t, db, id, OutgoingSendSent, SentCopyNotRequired, "", nil, old, nil, "")
	}

	first, err := db.PruneDurableMailJobs(ctx, now, 2)
	if err != nil || first.OutgoingSends != 2 {
		t.Fatalf("first bounded prune = %#v, %v", first, err)
	}
	second, err := db.PruneDurableMailJobs(ctx, now, 2)
	if err != nil || second.OutgoingSends != 1 {
		t.Fatalf("second bounded prune = %#v, %v", second, err)
	}
	third, err := db.PruneDurableMailJobs(ctx, now, 2)
	if err != nil || third.Total() != 0 {
		t.Fatalf("third bounded prune = %#v, %v", third, err)
	}

	var count int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM outgoing_sends`).Scan(&count); err != nil {
		t.Fatalf("count remaining outgoing rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("remaining outgoing rows = %d, want zero", count)
	}
}
