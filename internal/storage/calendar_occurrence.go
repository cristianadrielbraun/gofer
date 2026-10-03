package storage

import (
	"context"
	"database/sql"
)

// CalDAV versions the entire resource, not each occurrence. The provider has
// confirmed that only the selected occurrence changed and preserved all others.
// Advance only siblings cached at that same resource version, without changing
// their content, or the next one-off edit would falsely report a conflict.
func advanceCalendarOccurrenceResourceVersion(ctx context.Context, tx *sql.Tx, userID, sourceID, previous string, event CalendarEvent) error {
	_, err := tx.ExecContext(ctx, `UPDATE calendar_events SET etag=?
		WHERE user_id=? AND source_id=? AND series_remote_id=? AND ical_uid=? AND etag=?
		AND EXISTS (SELECT 1 FROM calendar_sources s WHERE s.id=calendar_events.source_id
		AND s.user_id=calendar_events.user_id AND s.provider='caldav')`,
		event.ETag, userID, sourceID, event.SeriesRemoteID, event.ICalUID, previous)
	return err
}
