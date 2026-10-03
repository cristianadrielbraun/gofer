package storage

import (
	"context"
	"fmt"
	"strings"
)

// CompleteCalendarDelete hides only the version the provider confirmed deleting.
// Keep the tombstone for reconciliation; do not fabricate a full-calendar sync.
func (db *DB) CompleteCalendarDelete(ctx context.Context, userID, eventID, sourceID, expectedETag string) error {
	if strings.TrimSpace(expectedETag) == "" {
		return fmt.Errorf("calendar deletion requires an event version")
	}
	result, err := db.Write().ExecContext(ctx, `UPDATE calendar_events
		SET is_deleted = 1, status = 'cancelled', updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND id = ? AND source_id = ? AND etag = ? AND is_deleted = 0
		AND EXISTS (
		    SELECT 1 FROM calendar_sources source
		    JOIN accounts account ON account.id = source.account_id AND account.user_id = source.user_id
		    WHERE source.id = calendar_events.source_id AND source.user_id = calendar_events.user_id
		      AND source.is_selected = 1 AND source.is_deleted = 0 AND COALESCE(account.is_deleting, 0) = 0
		)`, userID, eventID, sourceID, expectedETag)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrCalendarUpdateConflict
	}
	return nil
}
