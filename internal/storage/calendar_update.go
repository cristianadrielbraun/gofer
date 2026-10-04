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
	return db.completeCalendarUpdate(ctx, userID, eventID, sourceID, expectedETag, event, false, false, false)
}

// Keep the newly confirmed Outlook guest list and conferencing metadata on the
// same version as its editable fields. Never attach a new ETag to old guests.
func (db *DB) CompleteCalendarOutlookOnlineUpdate(ctx context.Context, existing CalendarEvent, event CalendarEvent) error {
	if existing.SourceProvider != "outlook" || event.ResponseStatus != "organizer" || event.ICalUID == "" || (existing.ICalUID != "" && existing.ICalUID != event.ICalUID) || !json.Valid([]byte(event.AttendeesJSON)) || !json.Valid([]byte(event.OnlineMeetingJSON)) {
		return ErrCalendarUpdateConflict
	}
	return db.completeCalendarUpdate(ctx, existing.UserID, existing.ID, existing.SourceID, existing.ETag, event, false, false, true)
}

// Keep both the occurrence identity and its parent unchanged; no other cached
// member of the series is reconciled or invalidated by a one-off edit.
func (db *DB) CompleteCalendarOccurrenceUpdate(ctx context.Context, userID, eventID, sourceID, expectedETag string, event CalendarEvent) error {
	if event.SeriesRemoteID == "" || event.SeriesRemoteID == event.RemoteID {
		return fmt.Errorf("provider did not confirm an occurrence")
	}
	if value := strings.TrimSpace(event.RecurrenceJSON); value != "" && value != "[]" && value != "{}" && value != "null" {
		var lines []string
		if json.Unmarshal([]byte(value), &lines) != nil || len(lines) != 1 || !strings.HasPrefix(lines[0], "RECURRENCE-ID:") {
			return fmt.Errorf("occurrence update cannot change repeat settings")
		}
	}
	return db.completeCalendarUpdate(ctx, userID, eventID, sourceID, expectedETag, event, false, true, false)
}

// CompleteCalendarSeriesConversion keeps a versioned tombstone for the former
// single event. A series master is not an appointment: only provider-expanded
// occurrences should be visible, even if the subsequent refresh fails.
func (db *DB) CompleteCalendarSeriesConversion(ctx context.Context, userID, eventID, sourceID, expectedETag string, event CalendarEvent) error {
	return db.completeCalendarUpdate(ctx, userID, eventID, sourceID, expectedETag, event, true, false, false)
}

func (db *DB) completeCalendarUpdate(ctx context.Context, userID, eventID, sourceID, expectedETag string, event CalendarEvent, series, occurrence, onlineMeeting bool) error {
	if expectedETag == "" || strings.TrimSpace(event.ETag) == "" || strings.TrimSpace(event.RemoteID) == "" || event.IsDeleted || (!occurrence && event.SeriesRemoteID != "") {
		return fmt.Errorf("provider did not confirm a versioned event")
	}
	recurrence := strings.TrimSpace(event.RecurrenceJSON)
	hasRecurrence := recurrence != "" && recurrence != "[]" && recurrence != "{}" && recurrence != "null"
	if (!occurrence && hasRecurrence != series) || ((series || hasRecurrence) && !json.Valid([]byte(recurrence))) {
		return fmt.Errorf("provider did not confirm the expected recurrence")
	}
	if !series && !occurrence || recurrence == "" {
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
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE calendar_events
		SET etag = ?, status = ?, summary = ?, description = ?, location = ?,
		    all_day = ?, start_date = ?, end_date = ?, start_at = ?, end_at = ?,
		    start_timezone = ?, end_timezone = ?, provider_updated_at = ?, recurrence_json = ?, is_deleted = ?, updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND id = ? AND source_id = ? AND remote_id = ? AND etag = ? AND is_deleted = 0 AND series_remote_id = ?
		AND EXISTS (
		    SELECT 1 FROM calendar_sources source
		    JOIN accounts account ON account.id = source.account_id AND account.user_id = source.user_id
		    WHERE source.id = calendar_events.source_id AND source.user_id = calendar_events.user_id
		      AND source.is_selected = 1 AND source.is_deleted = 0 AND COALESCE(account.is_deleting, 0) = 0
		)`, event.ETag, normalizeCalendarEventStatus(event.Status, false), event.Summary, event.Description, event.Location,
		calendarBoolInt(event.AllDay), startDate, endDate, startAt, endAt, event.StartTimeZone, event.EndTimeZone,
		calendarEventTimeValue(event.ProviderUpdatedAt), recurrence, calendarBoolInt(series), userID, eventID, sourceID, event.RemoteID, expectedETag, event.SeriesRemoteID)
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
	if onlineMeeting {
		result, err := tx.ExecContext(ctx, `UPDATE calendar_events SET ical_uid=?,organizer_name=?,organizer_email=?,response_status=?,attendees_json=?,online_meeting_json=?
 WHERE id=? AND user_id=? AND source_id=? AND etag=? AND EXISTS(SELECT 1 FROM calendar_sources s WHERE s.id=calendar_events.source_id AND s.provider='outlook')`, event.ICalUID, event.OrganizerName, event.OrganizerEmail, event.ResponseStatus, event.AttendeesJSON, event.OnlineMeetingJSON, eventID, userID, sourceID, event.ETag)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil {
			return err
		} else if count != 1 {
			return ErrCalendarUpdateConflict
		}
	}
	if occurrence {
		if err := advanceCalendarOccurrenceResourceVersion(ctx, tx, userID, sourceID, expectedETag, event); err != nil {
			return err
		}
	}
	return tx.Commit()
}
