package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var ErrCalendarUpdateConflict = errors.New("calendar event or calendar configuration changed")

// CompleteCalendarUpdate changes just one confirmed event in place. It neither
// reconciles other events nor reports a successful full-calendar refresh.
func (db *DB) CompleteCalendarUpdate(ctx context.Context, userID, eventID, sourceID, expectedETag string, event CalendarEvent) error {
	return db.completeCalendarUpdate(ctx, userID, eventID, sourceID, expectedETag, event, false)
}

// CompleteCalendarSeriesConversion keeps a versioned tombstone for the former
// single event. A series master is not an appointment: only provider-expanded
// occurrences should be visible, even if the subsequent refresh fails.
func (db *DB) CompleteCalendarSeriesConversion(ctx context.Context, userID, eventID, sourceID, expectedETag string, event CalendarEvent) error {
	return db.completeCalendarUpdate(ctx, userID, eventID, sourceID, expectedETag, event, true)
}

func (db *DB) completeCalendarUpdate(ctx context.Context, userID, eventID, sourceID, expectedETag string, event CalendarEvent, series bool) error {
	if expectedETag == "" || strings.TrimSpace(event.ETag) == "" || strings.TrimSpace(event.RemoteID) == "" || event.IsDeleted || event.SeriesRemoteID != "" {
		return fmt.Errorf("provider did not confirm a versioned event")
	}
	recurrence := strings.TrimSpace(event.RecurrenceJSON)
	hasRecurrence := recurrence != "" && recurrence != "[]" && recurrence != "{}" && recurrence != "null"
	if hasRecurrence != series || (series && !json.Valid([]byte(recurrence))) {
		return fmt.Errorf("provider did not confirm the expected recurrence")
	}
	if !series {
		recurrence = "[]"
	}
	var startDate, endDate, startAt, endAt any
	if event.AllDay {
		if event.StartDate == "" || event.EndDate <= event.StartDate {
			return fmt.Errorf("invalid all-day event")
		}
		startDate, endDate = event.StartDate, event.EndDate
	} else {
		if event.StartAt == nil || event.EndAt == nil || !event.EndAt.After(*event.StartAt) {
			return fmt.Errorf("invalid timed event")
		}
		startAt, endAt = calendarEventTimeValue(event.StartAt), calendarEventTimeValue(event.EndAt)
	}
	result, err := db.Write().ExecContext(ctx, `UPDATE calendar_events
		SET etag = ?, status = ?, summary = ?, description = ?, location = ?,
		    all_day = ?, start_date = ?, end_date = ?, start_at = ?, end_at = ?,
		    start_timezone = ?, end_timezone = ?, provider_updated_at = ?, recurrence_json = ?, is_deleted = ?, updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND id = ? AND source_id = ? AND remote_id = ? AND etag = ? AND is_deleted = 0
		AND EXISTS (
		    SELECT 1 FROM calendar_sources source
		    JOIN accounts account ON account.id = source.account_id AND account.user_id = source.user_id
		    WHERE source.id = calendar_events.source_id AND source.user_id = calendar_events.user_id
		      AND source.is_selected = 1 AND source.is_deleted = 0 AND COALESCE(account.is_deleting, 0) = 0
		)`, event.ETag, normalizeCalendarEventStatus(event.Status, false), event.Summary, event.Description, event.Location,
		calendarBoolInt(event.AllDay), startDate, endDate, startAt, endAt, event.StartTimeZone, event.EndTimeZone,
		calendarEventTimeValue(event.ProviderUpdatedAt), recurrence, calendarBoolInt(series), userID, eventID, sourceID, event.RemoteID, expectedETag)
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
