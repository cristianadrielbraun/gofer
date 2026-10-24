package storage

import (
	"context"
	"time"
)

// AvatarAttemptLogRetention is how long avatar lookup log rows are kept. It
// is longer than the window the lookup backoff counts failures in.
const AvatarAttemptLogRetention = 30 * 24 * time.Hour

// PruneAvatarAttemptLogs removes up to batch avatar lookup log rows written
// before before, oldest first.
func (db *DB) PruneAvatarAttemptLogs(ctx context.Context, before time.Time, batch int) (int, error) {
	result, err := db.Write().ExecContext(ctx, `
		DELETE FROM avatar_attempt_logs
		WHERE id IN (
			SELECT id FROM avatar_attempt_logs
			WHERE created_at < ?
			ORDER BY created_at
			LIMIT ?
		)`, before.UTC().Format("2006-01-02 15:04:05"), batch)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}
