package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// CompleteCalendarDelete hides only the version the provider confirmed deleting.
// Keep the tombstone for reconciliation; do not fabricate a full-calendar sync.
func (db *DB) CompleteCalendarDelete(ctx context.Context, userID, eventID, sourceID, expectedETag string) error {
	return db.completeCalendarDelete(ctx, userID, eventID, sourceID, expectedETag, nil)
}

func (db *DB) CompleteCalendarOccurrenceDelete(ctx context.Context, userID, eventID, sourceID, expectedETag string, event CalendarEvent) error {
	if event.SeriesRemoteID == "" || event.RemoteID == "" || event.SeriesRemoteID == event.RemoteID || event.ETag == "" {
		return fmt.Errorf("provider did not confirm an occurrence deletion")
	}
	return db.completeCalendarDelete(ctx, userID, eventID, sourceID, expectedETag, &event)
}

func (db *DB) completeCalendarDelete(ctx context.Context, userID, eventID, sourceID, expectedETag string, occurrence *CalendarEvent) error {
	if strings.TrimSpace(expectedETag) == "" {
		return fmt.Errorf("calendar deletion requires an event version")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := completeCalendarDeleteTx(ctx, tx, userID, eventID, sourceID, expectedETag, occurrence); err != nil {
		return err
	}
	return tx.Commit()
}

func completeCalendarDeleteTx(ctx context.Context, tx *sql.Tx, userID, eventID, sourceID, expectedETag string, occurrence *CalendarEvent) error {
	if strings.TrimSpace(expectedETag) == "" {
		return fmt.Errorf("calendar deletion requires an event version")
	}
	if occurrence != nil {
		var matches bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM calendar_events WHERE user_id=? AND source_id=? AND id=? AND remote_id=? AND series_remote_id=?)`, userID, sourceID, eventID, occurrence.RemoteID, occurrence.SeriesRemoteID).Scan(&matches); err != nil {
			return err
		}
		if !matches {
			return ErrCalendarUpdateConflict
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE calendar_events
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
	if occurrence != nil {
		if err := advanceCalendarOccurrenceResourceVersion(ctx, tx, userID, sourceID, expectedETag, *occurrence); err != nil {
			return err
		}
	}
	return nil
}
