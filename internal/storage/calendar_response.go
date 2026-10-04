package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

func migrateV98ToV99(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 99 {
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE calendar_events ADD COLUMN response_status TEXT NOT NULL DEFAULT '';
		CREATE TABLE IF NOT EXISTS calendar_response_requests (
		user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		source_id TEXT NOT NULL REFERENCES calendar_sources(id) ON DELETE CASCADE,
		remote_id TEXT NOT NULL, version TEXT NOT NULL,
		response TEXT NOT NULL CHECK (response IN ('accepted', 'tentative', 'declined')),
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (user_id, source_id, remote_id, version));`); err != nil {
		return err
	}
	if err := markSchemaVersion(tx, 99); err != nil {
		return err
	}
	return tx.Commit()
}

var ErrCalendarResponsePending = errors.New("a response was already submitted for this event version")

// Reserve before any network write, including after process restarts. Providers
// may send mail when responding and Graph actions have no idempotency key.
func (db *DB) BeginCalendarResponse(ctx context.Context, userID, sourceID, remoteID, version, response string) error {
	if remoteID == "" || version == "" {
		return ErrCalendarUpdateConflict
	}
	result, err := db.Write().ExecContext(ctx, `INSERT INTO calendar_response_requests (user_id, source_id, remote_id, version, response)
		SELECT source.user_id, source.id, ?, ?, ? FROM calendar_sources source
		JOIN accounts account ON account.id = source.account_id AND account.user_id = source.user_id
		WHERE source.user_id = ? AND source.id = ? AND source.is_selected = 1 AND source.is_deleted = 0
		AND source.provider IN ('gmail', 'outlook', 'caldav') AND COALESCE(account.is_deleting, 0) = 0
		ON CONFLICT DO NOTHING`, remoteID, version, response, userID, sourceID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrCalendarResponsePending
	}
	return nil
}

// Release only when no provider write happened, or the provider definitively
// rejected it. Timeouts and unconfirmed successful replies retain the guard.
func (db *DB) ReleaseCalendarResponse(ctx context.Context, userID, sourceID, remoteID, version string) error {
	_, err := db.Write().ExecContext(ctx, `DELETE FROM calendar_response_requests WHERE user_id = ? AND source_id = ? AND remote_id = ? AND version = ?`, userID, sourceID, remoteID, version)
	return err
}

// Cache the confirmed single event or occurrence, including any meeting changes
// observed by the post-response GET. Never attach a new ETag to stale details.
// Series responses are refreshed from the provider: never copy a master's
// version or response blindly over individually overridden occurrences.
func (db *DB) CompleteCalendarResponse(ctx context.Context, existing CalendarEvent, event CalendarEvent) error {
	return db.completeCalendarResponse(ctx, existing, event, false)
}

func (db *DB) CompleteCalendarIncomingResponse(ctx context.Context, existing CalendarEvent, event CalendarEvent) error {
	return db.completeCalendarResponse(ctx, existing, event, true)
}

func (db *DB) completeCalendarResponse(ctx context.Context, existing CalendarEvent, event CalendarEvent, organizer bool) error {
	validResponse := event.ResponseStatus == "accepted" || event.ResponseStatus == "tentative" || event.ResponseStatus == "declined"
	if organizer {
		validResponse = existing.SourceProvider == CalendarSourceProviderCalDAV && event.ResponseStatus == "organizer" && existing.AccountEmail != "" && strings.EqualFold(event.OrganizerEmail, existing.AccountEmail) && event.ICalUID == existing.ICalUID && event.SeriesRemoteID == ""
	}
	if event.RemoteID != existing.RemoteID || event.SeriesRemoteID != existing.SeriesRemoteID || event.ETag == "" || event.IsDeleted || event.Status == "cancelled" || !json.Valid([]byte(event.AttendeesJSON)) ||
		!validResponse {
		return ErrCalendarUpdateConflict
	}
	var startDate, endDate, startAt, endAt any
	if event.AllDay {
		if event.StartDate == "" || event.EndDate <= event.StartDate {
			return ErrCalendarUpdateConflict
		}
		startDate, endDate = event.StartDate, event.EndDate
	} else {
		if event.StartAt == nil || event.EndAt == nil || !event.EndAt.After(*event.StartAt) {
			return ErrCalendarUpdateConflict
		}
		startAt, endAt = calendarEventTimeValue(event.StartAt), calendarEventTimeValue(event.EndAt)
	}
	result, err := db.Write().ExecContext(ctx, `UPDATE calendar_events SET etag = ?, response_status = ?, attendees_json = ?, provider_updated_at = ?,
		status = ?, summary = ?, description = ?, location = ?, organizer_name = ?, organizer_email = ?,
		all_day = ?, start_date = ?, end_date = ?, start_at = ?, end_at = ?, start_timezone = ?, end_timezone = ?,
		recurrence_json = ?, online_meeting_json = ?, html_link = ?, updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND id = ? AND source_id = ? AND remote_id = ? AND etag = ? AND series_remote_id = ? AND is_deleted = 0
		AND EXISTS (SELECT 1 FROM calendar_sources source JOIN accounts account ON account.id = source.account_id AND account.user_id = source.user_id
		WHERE source.id = calendar_events.source_id AND source.user_id = calendar_events.user_id AND source.is_selected = 1 AND source.is_deleted = 0 AND COALESCE(account.is_deleting, 0) = 0)`,
		event.ETag, event.ResponseStatus, event.AttendeesJSON, calendarEventTimeValue(event.ProviderUpdatedAt),
		normalizeCalendarEventStatus(event.Status, false), event.Summary, event.Description, event.Location, event.OrganizerName, event.OrganizerEmail,
		calendarBoolInt(event.AllDay), startDate, endDate, startAt, endAt, event.StartTimeZone, event.EndTimeZone,
		calendarJSON(event.RecurrenceJSON, "[]"), calendarJSON(event.OnlineMeetingJSON, "{}"), event.HTMLLink,
		existing.UserID, existing.ID, existing.SourceID, existing.RemoteID, existing.ETag, existing.SeriesRemoteID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrCalendarUpdateConflict
	}
	return nil
}
