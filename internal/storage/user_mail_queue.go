package storage

import (
	"context"
	"database/sql"
	"time"
)

// RecoverAccountMailQueue requires serialized execution for this account. SMTP
// uncertainty is terminal until confirmed; IMAP copies/revisions can be searched.
func (db *DB) RecoverAccountMailQueue(ctx context.Context, accountID string) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`UPDATE outgoing_sends SET status='ambiguous',last_error='Gofer stopped during delivery. This message may have been sent.',locked_at=NULL,updated_at=CURRENT_TIMESTAMP WHERE account_id=? AND status='sending'`,
		`UPDATE outgoing_sends SET sent_copy_status='ambiguous',sent_copy_last_error='Gofer stopped during Sent copy.',sent_copy_locked_at=NULL,sent_copy_next_attempt_at=CURRENT_TIMESTAMP WHERE account_id=? AND sent_copy_status='copying'`,
		`UPDATE imap_draft_operations SET status='ambiguous',last_error='Gofer stopped during draft sync.',locked_at=NULL,next_attempt_at=CURRENT_TIMESTAMP WHERE account_id=? AND status='syncing'`,
	} {
		if _, err := tx.ExecContext(ctx, q, accountID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) NextAccountMailQueueAttempt(ctx context.Context, accountID string) (time.Time, error) {
	// modernc's UTC time bindings include " +0000 UTC". Strip that suffix
	// only for SQLite date comparisons; keep the original value for Go parsing.
	var next sqliteNullTime
	err := db.Read().QueryRowContext(ctx, `SELECT due FROM (
		SELECT CASE WHEN julianday(replace(send_after,' +0000 UTC',''))>julianday(replace(next_attempt_at,' +0000 UTC','')) THEN send_after ELSE next_attempt_at END AS due
		FROM outgoing_sends WHERE account_id=? AND status IN ('pending','sending') AND length(mime_data)>0
		UNION ALL SELECT sent_copy_next_attempt_at FROM outgoing_sends WHERE account_id=? AND status='sent'
		AND sent_copy_status IN ('pending','failed','ambiguous','copying') AND length(mime_data)>0
		UNION ALL SELECT next_attempt_at FROM imap_draft_operations WHERE account_id=?
	) ORDER BY julianday(replace(due,' +0000 UTC','')) LIMIT 1`, accountID, accountID, accountID).Scan(&next)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	return next.Time, err
}
