package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CalendarSource is the local representation of one provider calendar. A
// source is kept even when it is not selected so a later discovery can
// preserve the user's choice.
type CalendarSource struct {
	ID          string
	UserID      string
	AccountID   string
	Provider    string
	RemoteID    string
	Name        string
	Description string
	TimeZone    string
	Color       string
	AccessRole  string
	IsPrimary   bool
	IsSelected  bool
	IsDeleted   bool
}

// CalendarEvent is the local representation of one normalized provider
// event. Provider-specific JSON is retained so the read-only cache does not
// discard recurrence, attendee, or online-meeting details needed later.
type CalendarEvent struct {
	ID                string
	UserID            string
	SourceID          string
	RemoteID          string
	ICalUID           string
	SeriesRemoteID    string
	ETag              string
	Status            string
	Summary           string
	Description       string
	Location          string
	OrganizerName     string
	OrganizerEmail    string
	AllDay            bool
	StartDate         string
	EndDate           string
	StartAt           *time.Time
	EndAt             *time.Time
	StartTimeZone     string
	EndTimeZone       string
	RecurrenceJSON    string
	AttendeesJSON     string
	OnlineMeetingJSON string
	HTMLLink          string
	ProviderCreatedAt *time.Time
	ProviderUpdatedAt *time.Time
	IsDeleted         bool
	SourceName        string
	SourceColor       string
}

// ReplaceCalendarSources reconciles one provider account's discovered
// calendars. Existing selection state is preserved; new sources use the
// caller's default selection. Sources no longer returned by the provider are
// soft-deleted and deselected.
func (db *DB) ReplaceCalendarSources(ctx context.Context, userID, accountID, provider string, sources []CalendarSource) error {
	userID = strings.TrimSpace(userID)
	accountID = strings.TrimSpace(accountID)
	provider = strings.TrimSpace(provider)
	if userID == "" || accountID == "" || provider == "" {
		return fmt.Errorf("calendar source reconciliation requires user, account, and provider")
	}

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin calendar source reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var accountExists int
	if err := tx.QueryRowContext(ctx, `
		SELECT 1
		FROM accounts
		WHERE id = ? AND user_id = ? AND provider = ? AND COALESCE(is_deleting, 0) = 0`,
		accountID, userID, provider).Scan(&accountExists); err != nil {
		if err == sql.ErrNoRows {
			return sql.ErrNoRows
		}
		return fmt.Errorf("verify calendar account: %w", err)
	}

	remoteIDs := make([]string, 0, len(sources))
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		remoteID := strings.TrimSpace(source.RemoteID)
		if remoteID == "" {
			continue
		}
		if _, exists := seen[remoteID]; exists {
			return fmt.Errorf("duplicate calendar remote id %q", remoteID)
		}
		seen[remoteID] = struct{}{}
		remoteIDs = append(remoteIDs, remoteID)

		sourceID := strings.TrimSpace(source.ID)
		selected := source.IsSelected
		var existingSelected int
		err := tx.QueryRowContext(ctx, `
			SELECT id, is_selected
			FROM calendar_sources
			WHERE account_id = ? AND remote_id = ?`, accountID, remoteID).Scan(&sourceID, &existingSelected)
		if err == nil {
			selected = existingSelected == 1
		} else if err != sql.ErrNoRows {
			return fmt.Errorf("load calendar source %q: %w", remoteID, err)
		}
		if sourceID == "" {
			sourceID = uuid.NewString()
		}

		if _, err = tx.ExecContext(ctx, `
			INSERT INTO calendar_sources (
				id, user_id, account_id, provider, remote_id, name, description,
				timezone, color, access_role, is_primary, is_selected, is_deleted
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
			ON CONFLICT(account_id, remote_id) DO UPDATE SET
				user_id = excluded.user_id,
				provider = excluded.provider,
				name = excluded.name,
				description = excluded.description,
				timezone = excluded.timezone,
				color = excluded.color,
				access_role = excluded.access_role,
				is_primary = excluded.is_primary,
				is_selected = excluded.is_selected,
				is_deleted = 0,
				updated_at = CURRENT_TIMESTAMP`,
			sourceID, userID, accountID, provider, remoteID,
			strings.TrimSpace(source.Name), strings.TrimSpace(source.Description),
			strings.TrimSpace(source.TimeZone), strings.TrimSpace(source.Color), strings.TrimSpace(source.AccessRole),
			calendarBoolInt(source.IsPrimary), calendarBoolInt(selected)); err != nil {
			return fmt.Errorf("upsert calendar source %q: %w", remoteID, err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO calendar_sync_state (source_id)
			VALUES (?)
			ON CONFLICT(source_id) DO NOTHING`, sourceID); err != nil {
			return fmt.Errorf("create calendar sync state %q: %w", sourceID, err)
		}
	}

	markMissingQuery := `
		UPDATE calendar_sources
		SET is_deleted = 1, is_selected = 0, updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND account_id = ? AND provider = ?`
	markMissingArgs := []any{userID, accountID, provider}
	if len(remoteIDs) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(remoteIDs)), ",")
		markMissingQuery += ` AND remote_id NOT IN (` + placeholders + `)`
		for _, remoteID := range remoteIDs {
			markMissingArgs = append(markMissingArgs, remoteID)
		}
	}
	if _, err := tx.ExecContext(ctx, markMissingQuery, markMissingArgs...); err != nil {
		return fmt.Errorf("mark missing calendar sources: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit calendar source reconciliation: %w", err)
	}
	return nil
}

func (db *DB) ListCalendarSourcesForAccount(ctx context.Context, userID, accountID string) ([]CalendarSource, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT id, user_id, account_id, provider, remote_id, name, description,
		       timezone, color, access_role, is_primary, is_selected, is_deleted
		FROM calendar_sources
		WHERE user_id = ? AND account_id = ? AND is_deleted = 0
		ORDER BY is_primary DESC, name COLLATE NOCASE, remote_id`, userID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sources []CalendarSource
	for rows.Next() {
		var source CalendarSource
		var isPrimary, isSelected, isDeleted int
		if err := rows.Scan(
			&source.ID, &source.UserID, &source.AccountID, &source.Provider,
			&source.RemoteID, &source.Name, &source.Description, &source.TimeZone,
			&source.Color, &source.AccessRole, &isPrimary, &isSelected, &isDeleted,
		); err != nil {
			return nil, err
		}
		source.IsPrimary = isPrimary == 1
		source.IsSelected = isSelected == 1
		source.IsDeleted = isDeleted == 1
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

// ListSelectedCalendarSources returns active sources that should be included
// in the user's Calendar view. Sources remain account-scoped so provider
// tokens can be resolved without exposing unselected calendars.
func (db *DB) ListSelectedCalendarSources(ctx context.Context, userID string) ([]CalendarSource, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT source.id, source.user_id, source.account_id, source.provider,
		       source.remote_id, source.name, source.description, source.timezone,
		       source.color, source.access_role, source.is_primary,
		       source.is_selected, source.is_deleted
		FROM calendar_sources source
		JOIN accounts account ON account.id = source.account_id
		WHERE source.user_id = ?
		  AND account.user_id = source.user_id
		  AND COALESCE(account.is_deleting, 0) = 0
		  AND source.is_deleted = 0
		  AND source.is_selected = 1
		ORDER BY source.is_primary DESC, source.name COLLATE NOCASE, source.remote_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sources []CalendarSource
	for rows.Next() {
		var source CalendarSource
		var isPrimary, isSelected, isDeleted int
		if err := rows.Scan(
			&source.ID, &source.UserID, &source.AccountID, &source.Provider,
			&source.RemoteID, &source.Name, &source.Description, &source.TimeZone,
			&source.Color, &source.AccessRole, &isPrimary, &isSelected, &isDeleted,
		); err != nil {
			return nil, err
		}
		source.IsPrimary = isPrimary == 1
		source.IsSelected = isSelected == 1
		source.IsDeleted = isDeleted == 1
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

// ReplaceCalendarEvents reconciles one source for a complete time window.
// Events absent from the provider response inside that window are soft
// deleted, which keeps moved and cancelled events out of the cache without
// destroying their provider identity.
func (db *DB) ReplaceCalendarEvents(ctx context.Context, userID, sourceID string, events []CalendarEvent, windowStart, windowEnd time.Time) error {
	userID = strings.TrimSpace(userID)
	sourceID = strings.TrimSpace(sourceID)
	if userID == "" || sourceID == "" {
		return fmt.Errorf("calendar event reconciliation requires user and source")
	}
	if windowStart.IsZero() || windowEnd.IsZero() || !windowEnd.After(windowStart) {
		return fmt.Errorf("calendar event reconciliation requires a valid time window")
	}

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin calendar event reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var sourceExists int
	if err := tx.QueryRowContext(ctx, `
		SELECT 1
		FROM calendar_sources source
		JOIN accounts account ON account.id = source.account_id
		WHERE source.id = ? AND source.user_id = ?
		  AND account.user_id = source.user_id
		  AND COALESCE(account.is_deleting, 0) = 0`, sourceID, userID).Scan(&sourceExists); err != nil {
		if err == sql.ErrNoRows {
			return sql.ErrNoRows
		}
		return fmt.Errorf("verify calendar source: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO calendar_sync_state (source_id)
		VALUES (?)
		ON CONFLICT(source_id) DO NOTHING`, sourceID); err != nil {
		return fmt.Errorf("ensure calendar sync state: %w", err)
	}

	remoteIDs := make([]string, 0, len(events))
	seen := make(map[string]struct{}, len(events))
	for _, event := range events {
		remoteID := strings.TrimSpace(event.RemoteID)
		if remoteID == "" {
			continue
		}
		if _, exists := seen[remoteID]; exists {
			return fmt.Errorf("duplicate calendar event remote id %q", remoteID)
		}
		seen[remoteID] = struct{}{}
		remoteIDs = append(remoteIDs, remoteID)

		eventID := strings.TrimSpace(event.ID)
		var existingID string
		if err := tx.QueryRowContext(ctx, `
			SELECT id FROM calendar_events WHERE source_id = ? AND remote_id = ?`, sourceID, remoteID).Scan(&existingID); err == nil {
			eventID = existingID
		} else if err != sql.ErrNoRows {
			return fmt.Errorf("load calendar event %q: %w", remoteID, err)
		}
		if eventID == "" {
			eventID = uuid.NewString()
		}

		status := normalizeCalendarEventStatus(event.Status, event.IsDeleted)
		allDay := calendarBoolInt(event.AllDay)
		startDate := strings.TrimSpace(event.StartDate)
		endDate := strings.TrimSpace(event.EndDate)
		startAt := calendarEventTimeValue(event.StartAt)
		endAt := calendarEventTimeValue(event.EndAt)
		if !event.IsDeleted {
			if event.AllDay {
				if startDate == "" || endDate == "" || startDate >= endDate {
					return fmt.Errorf("calendar all-day event %q has an invalid date range", remoteID)
				}
				startAt = nil
				endAt = nil
			} else if event.StartAt == nil || event.EndAt == nil || !event.EndAt.After(*event.StartAt) {
				return fmt.Errorf("calendar timed event %q has an invalid time range", remoteID)
			} else {
				startDate = ""
				endDate = ""
			}
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO calendar_events (
				id, user_id, source_id, remote_id, ical_uid, series_remote_id, etag,
				status, summary, description, location, organizer_name, organizer_email,
				all_day, start_date, end_date, start_at, end_at, start_timezone,
				end_timezone, recurrence_json, attendees_json, online_meeting_json,
				html_link, provider_created_at, provider_updated_at, is_deleted
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(source_id, remote_id) DO UPDATE SET
				user_id = excluded.user_id,
				ical_uid = excluded.ical_uid,
				series_remote_id = excluded.series_remote_id,
				etag = excluded.etag,
				status = excluded.status,
				summary = excluded.summary,
				description = excluded.description,
				location = excluded.location,
				organizer_name = excluded.organizer_name,
				organizer_email = excluded.organizer_email,
				all_day = excluded.all_day,
				start_date = excluded.start_date,
				end_date = excluded.end_date,
				start_at = excluded.start_at,
				end_at = excluded.end_at,
				start_timezone = excluded.start_timezone,
				end_timezone = excluded.end_timezone,
				recurrence_json = excluded.recurrence_json,
				attendees_json = excluded.attendees_json,
				online_meeting_json = excluded.online_meeting_json,
				html_link = excluded.html_link,
				provider_created_at = excluded.provider_created_at,
				provider_updated_at = excluded.provider_updated_at,
				is_deleted = excluded.is_deleted,
				updated_at = CURRENT_TIMESTAMP`,
			eventID, userID, sourceID, remoteID,
			strings.TrimSpace(event.ICalUID), strings.TrimSpace(event.SeriesRemoteID), strings.TrimSpace(event.ETag),
			status, strings.TrimSpace(event.Summary), strings.TrimSpace(event.Description), strings.TrimSpace(event.Location),
			strings.TrimSpace(event.OrganizerName), strings.TrimSpace(event.OrganizerEmail), allDay,
			nullableCalendarString(startDate), nullableCalendarString(endDate), startAt, endAt,
			strings.TrimSpace(event.StartTimeZone), strings.TrimSpace(event.EndTimeZone),
			calendarJSON(event.RecurrenceJSON, "[]"), calendarJSON(event.AttendeesJSON, "[]"),
			calendarJSON(event.OnlineMeetingJSON, "{}"), strings.TrimSpace(event.HTMLLink),
			calendarEventTimeValue(event.ProviderCreatedAt), calendarEventTimeValue(event.ProviderUpdatedAt), calendarBoolInt(event.IsDeleted)); err != nil {
			return fmt.Errorf("upsert calendar event %q: %w", remoteID, err)
		}
	}

	windowStartDate := windowStart.Format("2006-01-02")
	windowEndDate := windowEnd.Format("2006-01-02")
	markMissingQuery := `
		UPDATE calendar_events
		SET is_deleted = 1, updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND source_id = ? AND is_deleted = 0
		  AND (
			(all_day = 1 AND start_date < ? AND end_date > ?)
			OR (all_day = 0 AND start_at < ? AND end_at > ?)
		  )`
	markMissingArgs := []any{
		userID, sourceID, windowEndDate, windowStartDate,
		formatDBTime(windowEnd.UTC()), formatDBTime(windowStart.UTC()),
	}
	if len(remoteIDs) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(remoteIDs)), ",")
		markMissingQuery += ` AND remote_id NOT IN (` + placeholders + `)`
		for _, remoteID := range remoteIDs {
			markMissingArgs = append(markMissingArgs, remoteID)
		}
	}
	if _, err := tx.ExecContext(ctx, markMissingQuery, markMissingArgs...); err != nil {
		return fmt.Errorf("mark missing calendar events: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE calendar_sync_state
		SET state = 'ok', window_start = ?, window_end = ?, full_sync_required = 0,
			last_success_at = CURRENT_TIMESTAMP, next_attempt_at = CURRENT_TIMESTAMP,
			last_error = '', updated_at = CURRENT_TIMESTAMP
		WHERE source_id = ?`, formatDBTime(windowStart.UTC()), formatDBTime(windowEnd.UTC()), sourceID); err != nil {
		return fmt.Errorf("update calendar sync state: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit calendar event reconciliation: %w", err)
	}
	return nil
}

// ListCalendarEvents returns cached events that overlap the requested window
// on selected, active sources.
func (db *DB) ListCalendarEvents(ctx context.Context, userID string, windowStart, windowEnd time.Time) ([]CalendarEvent, error) {
	if strings.TrimSpace(userID) == "" || windowStart.IsZero() || windowEnd.IsZero() || !windowEnd.After(windowStart) {
		return nil, fmt.Errorf("calendar event listing requires user and valid time window")
	}

	rows, err := db.Read().QueryContext(ctx, `
		SELECT event.id, event.user_id, event.source_id, event.remote_id,
		       event.ical_uid, event.series_remote_id, event.etag, event.status,
		       event.summary, event.description, event.location, event.organizer_name,
		       event.organizer_email, event.all_day, event.start_date, event.end_date,
		       event.start_at, event.end_at, event.start_timezone, event.end_timezone,
		       event.recurrence_json, event.attendees_json, event.online_meeting_json,
		       event.html_link, event.provider_created_at, event.provider_updated_at,
		       event.is_deleted, source.name, source.color
		FROM calendar_events event
		JOIN calendar_sources source ON source.id = event.source_id
		JOIN accounts account ON account.id = source.account_id
		WHERE event.user_id = ?
		  AND source.user_id = event.user_id
		  AND source.is_deleted = 0
		  AND source.is_selected = 1
		  AND COALESCE(account.is_deleting, 0) = 0
		  AND event.is_deleted = 0
		  AND (
			(event.all_day = 1 AND event.start_date < ? AND event.end_date > ?)
			OR (event.all_day = 0 AND event.start_at < ? AND event.end_at > ?)
		  )
		ORDER BY CASE WHEN event.all_day = 1 THEN event.start_date ELSE event.start_at END,
		         event.summary COLLATE NOCASE, event.id`,
		userID,
		windowEnd.Format("2006-01-02"), windowStart.Format("2006-01-02"),
		formatDBTime(windowEnd.UTC()), formatDBTime(windowStart.UTC()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []CalendarEvent
	for rows.Next() {
		var event CalendarEvent
		var allDay, isDeleted int
		var startDate, endDate sql.NullString
		var startAt, endAt sqliteNullTime
		var providerCreatedAt, providerUpdatedAt sqliteNullTime
		if err := rows.Scan(
			&event.ID, &event.UserID, &event.SourceID, &event.RemoteID,
			&event.ICalUID, &event.SeriesRemoteID, &event.ETag, &event.Status,
			&event.Summary, &event.Description, &event.Location, &event.OrganizerName,
			&event.OrganizerEmail, &allDay, &startDate, &endDate, &startAt, &endAt,
			&event.StartTimeZone, &event.EndTimeZone, &event.RecurrenceJSON, &event.AttendeesJSON,
			&event.OnlineMeetingJSON, &event.HTMLLink, &providerCreatedAt, &providerUpdatedAt,
			&isDeleted, &event.SourceName, &event.SourceColor,
		); err != nil {
			return nil, err
		}
		event.AllDay = allDay == 1
		event.IsDeleted = isDeleted == 1
		if startDate.Valid {
			event.StartDate = startDate.String
		}
		if endDate.Valid {
			event.EndDate = endDate.String
		}
		if startAt.Valid {
			value := startAt.Time
			event.StartAt = &value
		}
		if endAt.Valid {
			value := endAt.Time
			event.EndAt = &value
		}
		if providerCreatedAt.Valid {
			value := providerCreatedAt.Time
			event.ProviderCreatedAt = &value
		}
		if providerUpdatedAt.Valid {
			value := providerUpdatedAt.Time
			event.ProviderUpdatedAt = &value
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func normalizeCalendarEventStatus(status string, deleted bool) string {
	if deleted || strings.EqualFold(strings.TrimSpace(status), "cancelled") {
		return "cancelled"
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "tentative":
		return "tentative"
	default:
		return "confirmed"
	}
}

func calendarEventTimeValue(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return formatDBTime(value.UTC())
}

func nullableCalendarString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.TrimSpace(value)
}

func calendarJSON(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" || !json.Valid([]byte(value)) {
		return fallback
	}
	return value
}

func calendarBoolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
