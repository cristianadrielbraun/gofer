package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const calendarCreateSchema = `
CREATE TABLE IF NOT EXISTS calendar_create_requests (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    request_id TEXT NOT NULL,
    source_id TEXT NOT NULL REFERENCES calendar_sources(id) ON DELETE CASCADE,
    request_hash TEXT NOT NULL,
    event_id TEXT NOT NULL DEFAULT '',
    remote_id TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, request_id)
);`

func migrateV97ToV98(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(calendarCreateSchema); err != nil {
		return err
	}
	if err := markSchemaVersion(tx, 98); err != nil {
		return err
	}
	return tx.Commit()
}

var ErrCalendarCreateConflict = errors.New("this event request was already used with different details; start a new event")

type CalendarCreateRequest struct {
	EventID  string
	RemoteID string
}

// BeginCalendarCreate binds a stable request ID to one owned source and draft.
// Pending requests may be retried using the provider's same idempotency key.
func (db *DB) BeginCalendarCreate(ctx context.Context, userID, sourceID, requestID, hash string) (CalendarCreateRequest, error) {
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO calendar_create_requests (user_id, request_id, source_id, request_hash)
		SELECT source.user_id, ?, source.id, ? FROM calendar_sources source
		JOIN accounts account ON account.id = source.account_id AND account.user_id = source.user_id
		WHERE source.user_id = ? AND source.id = ? AND source.is_selected = 1
		  AND source.is_deleted = 0 AND COALESCE(account.is_deleting, 0) = 0
		ON CONFLICT(user_id, request_id) DO NOTHING`, requestID, hash, userID, sourceID); err != nil {
		return CalendarCreateRequest{}, err
	}
	var existingSource, existingHash string
	var request CalendarCreateRequest
	err := db.Read().QueryRowContext(ctx, `SELECT source_id, request_hash, event_id, remote_id FROM calendar_create_requests WHERE user_id = ? AND request_id = ?`, userID, requestID).Scan(&existingSource, &existingHash, &request.EventID, &request.RemoteID)
	if err != nil {
		return request, err
	}
	if existingSource != sourceID || existingHash != hash {
		return request, ErrCalendarCreateConflict
	}
	return request, nil
}

// CompleteCalendarCreate inserts only the confirmed event, never reconciles a
// window or claims a full sync. The durable result and cache commit together.
func (db *DB) CompleteCalendarCreate(ctx context.Context, userID, sourceID, requestID, hash string, event CalendarEvent) (CalendarCreateRequest, error) {
	if strings.TrimSpace(event.RemoteID) == "" || event.IsDeleted || event.SeriesRemoteID != "" {
		return CalendarCreateRequest{}, fmt.Errorf("provider did not confirm a single event")
	}
	if event.AllDay {
		if event.StartDate == "" || event.EndDate <= event.StartDate {
			return CalendarCreateRequest{}, fmt.Errorf("invalid all-day event")
		}
	} else if event.StartAt == nil || event.EndAt == nil || !event.EndAt.After(*event.StartAt) {
		return CalendarCreateRequest{}, fmt.Errorf("invalid timed event")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return CalendarCreateRequest{}, err
	}
	defer tx.Rollback()
	var confirmed int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM calendar_sources source
		JOIN accounts account ON account.id = source.account_id AND account.user_id = source.user_id
		WHERE source.user_id = ? AND source.id = ? AND source.is_deleted = 0
		  AND source.is_selected = 1 AND COALESCE(account.is_deleting, 0) = 0`, userID, sourceID).Scan(&confirmed); err != nil {
		return CalendarCreateRequest{}, err
	}
	result := CalendarCreateRequest{EventID: uuid.NewString(), RemoteID: event.RemoteID}
	var previousID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM calendar_events WHERE user_id = ? AND source_id = ? AND remote_id = ?`, userID, sourceID, event.RemoteID).Scan(&previousID)
	if err == nil {
		result.EventID = previousID
	} else if err != sql.ErrNoRows {
		return CalendarCreateRequest{}, err
	}
	var startDate, endDate, startAt, endAt any
	if event.AllDay {
		startDate, endDate = event.StartDate, event.EndDate
	} else {
		startAt, endAt = calendarEventTimeValue(event.StartAt), calendarEventTimeValue(event.EndAt)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO calendar_events (
		id, user_id, source_id, remote_id, ical_uid, etag, status, summary, description, location,
		organizer_name, organizer_email, all_day, start_date, end_date, start_at, end_at,
		start_timezone, end_timezone, html_link, provider_created_at, provider_updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(source_id, remote_id) DO NOTHING`, result.EventID, userID, sourceID, event.RemoteID,
		event.ICalUID, event.ETag, normalizeCalendarEventStatus(event.Status, false), event.Summary, event.Description, event.Location,
		event.OrganizerName, event.OrganizerEmail, calendarBoolInt(event.AllDay), startDate, endDate, startAt, endAt,
		event.StartTimeZone, event.EndTimeZone, event.HTMLLink, calendarEventTimeValue(event.ProviderCreatedAt), calendarEventTimeValue(event.ProviderUpdatedAt)); err != nil {
		return CalendarCreateRequest{}, err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE calendar_create_requests SET event_id = ?, remote_id = ? WHERE user_id = ? AND request_id = ? AND source_id = ? AND request_hash = ?`, result.EventID, result.RemoteID, userID, requestID, sourceID, hash)
	if err != nil {
		return CalendarCreateRequest{}, err
	}
	if n, _ := updated.RowsAffected(); n != 1 {
		return CalendarCreateRequest{}, ErrCalendarCreateConflict
	}
	if err := tx.Commit(); err != nil {
		return CalendarCreateRequest{}, err
	}
	return result, nil
}
