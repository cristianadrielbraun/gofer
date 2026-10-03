package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// A confirmed master update invalidates ALL cached instances of that series,
// including those outside the window about to be refreshed. Never manufacture
// occurrences locally or leave removed/moved occurrences in another month.
func (db *DB) CompleteCalendarSeriesUpdate(ctx context.Context, userID, occurrenceID, sourceID, occurrenceETag string, master CalendarEvent) error {
	return db.completeCalendarSeriesMutation(ctx, userID, occurrenceID, sourceID, occurrenceETag, master, false)
}

// Call only after the provider confirms deletion of this exact master version.
// Tombstone the entire series, across all cached windows, in one transaction.
func (db *DB) CompleteCalendarSeriesDelete(ctx context.Context, userID, occurrenceID, sourceID, occurrenceETag string, master CalendarEvent) error {
	return db.completeCalendarSeriesMutation(ctx, userID, occurrenceID, sourceID, occurrenceETag, master, true)
}

func (db *DB) completeCalendarSeriesMutation(ctx context.Context, userID, occurrenceID, sourceID, occurrenceETag string, master CalendarEvent, deleted bool) error {
	recurrence := strings.TrimSpace(master.RecurrenceJSON)
	if master.RemoteID == "" || master.ETag == "" || master.IsDeleted || master.SeriesRemoteID != "" ||
		!json.Valid([]byte(recurrence)) || recurrence == "[]" || recurrence == "{}" || recurrence == "null" {
		return fmt.Errorf("provider did not confirm a versioned series master")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var allowed bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM calendar_events e JOIN calendar_sources s ON s.id=e.source_id AND s.user_id=e.user_id
		JOIN accounts a ON a.id=s.account_id AND a.user_id=s.user_id
		WHERE e.id=? AND e.user_id=? AND e.source_id=? AND e.etag=? AND e.is_deleted=0
		AND (e.series_remote_id=? OR (e.series_remote_id='' AND e.remote_id=?))
		AND s.is_selected=1 AND s.is_deleted=0 AND COALESCE(a.is_deleting,0)=0
	)`, occurrenceID, userID, sourceID, occurrenceETag, master.RemoteID, master.RemoteID).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrCalendarUpdateConflict
	}
	_, err = tx.ExecContext(ctx, `UPDATE calendar_events SET is_deleted=1,
		status=CASE WHEN ? THEN 'cancelled' ELSE status END, updated_at=CURRENT_TIMESTAMP
		WHERE user_id=? AND source_id=? AND (series_remote_id=? OR remote_id=?)`, deleted, userID, sourceID, master.RemoteID, master.RemoteID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
