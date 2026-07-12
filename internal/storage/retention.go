package storage

import (
	"context"
	"time"
)

// These values are deliberately code-level policy for now. If operators ever
// need different retention windows, that should be a separate, auditable
// setting rather than an implicit environment override.
const (
	OutgoingSendRetentionPeriod         = 30 * 24 * time.Hour
	CanceledOutgoingSendRetentionPeriod = 30 * 24 * time.Hour
	RetentionBatchSize                  = 100
)

// DurableJobPruneResult is the aggregate result of one bounded maintenance
// transaction. A zero result means that the next pass can stop.
type DurableJobPruneResult struct {
	OutgoingSends       int
	MessageMutations    int
	IMAPDraftOperations int
	LabelMutations      int
}

func (r DurableJobPruneResult) Total() int {
	return r.OutgoingSends + r.MessageMutations + r.IMAPDraftOperations + r.LabelMutations
}

// PruneDurableMailJobs removes only terminal rows whose provider work is
// already complete and whose payload has been released. It intentionally does
// not age out failed, ambiguous, or in-flight work: those rows are still part
// of recovery and user-visible diagnostics.
//
// Each call is one small transaction. The maintenance worker may call it
// repeatedly, but no individual transaction can take an unbounded SQLite
// writer lock.
func (db *DB) PruneDurableMailJobs(ctx context.Context, now time.Time, batch int) (DurableJobPruneResult, error) {
	if batch <= 0 {
		batch = RetentionBatchSize
	}
	now = now.UTC()
	completedCutoff := now.Add(-OutgoingSendRetentionPeriod)
	canceledCutoff := now.Add(-CanceledOutgoingSendRetentionPeriod)

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return DurableJobPruneResult{}, err
	}
	defer tx.Rollback()

	deleteResult, err := tx.ExecContext(ctx, `
		DELETE FROM outgoing_sends
		WHERE id IN (
			SELECT os.id
			FROM outgoing_sends os
			WHERE (
				(os.status = ?
				 AND os.sent_copy_status IN (?, ?)
				 AND os.updated_at < ?
				 AND os.locked_at IS NULL
				 AND os.sent_copy_locked_at IS NULL
				 AND (os.mime_data IS NULL OR length(os.mime_data) = 0)
				 AND length(COALESCE(os.message_json, '')) = 0)
				OR
				(os.status = ?
				 AND os.updated_at < ?
				 AND os.locked_at IS NULL
				 AND os.sent_copy_locked_at IS NULL)
			)
			AND NOT EXISTS (
				SELECT 1
				FROM imap_draft_operations draft_op
				WHERE draft_op.account_id = os.account_id
				  AND draft_op.draft_key = os.draft_id
				  AND draft_op.status IN ('pending', 'syncing', 'failed', 'ambiguous')
			)
			ORDER BY os.updated_at ASC, os.id ASC
			LIMIT ?
		)`,
		OutgoingSendSent, SentCopyNotRequired, SentCopyComplete, completedCutoff,
		OutgoingSendCanceled, canceledCutoff, batch)
	if err != nil {
		return DurableJobPruneResult{}, err
	}
	deleted, err := deleteResult.RowsAffected()
	if err != nil {
		return DurableJobPruneResult{}, err
	}
	pruneResult := DurableJobPruneResult{OutgoingSends: int(deleted)}

	if err := tx.Commit(); err != nil {
		return DurableJobPruneResult{}, err
	}
	return pruneResult, nil
}
