package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"time"
)

type CalendarEventPublicationKind uint8

const (
	CalendarPublishUpdate CalendarEventPublicationKind = iota + 1
	CalendarPublishOnlineUpdate
	CalendarPublishOccurrenceUpdate
	CalendarPublishSeriesConversion
	CalendarPublishSeriesUpdate
	CalendarPublishDelete
	CalendarPublishOccurrenceDelete
	CalendarPublishSeriesDelete
	CalendarPublishResponse
)

type calendarPublicationRecord struct {
	event   CalendarEvent
	created string
}

type calendarPublicationScanner struct {
	row     interface{ Scan(...any) error }
	created *string
}

func (s calendarPublicationScanner) Scan(values ...any) error {
	return s.row.Scan(append(values, s.created)...)
}

// Include tombstones and creation timestamps, using the same owned source and
// account joins as normal reads. A compound action verifies every intended row.
func calendarPublicationRecordsTx(ctx context.Context, tx *sql.Tx, owner, source, clause string, args ...any) (map[string]calendarPublicationRecord, error) {
	parameters := append([]any{owner, source}, args...)
	rows, err := tx.QueryContext(ctx, calendarEventColumns+`, event.created_at`+calendarEventFrom+` AND event.source_id=?`+clause+` ORDER BY event.id`, parameters...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]calendarPublicationRecord)
	for rows.Next() {
		var record calendarPublicationRecord
		record.event, err = scanCalendarEvent(calendarPublicationScanner{row: rows, created: &record.created})
		if err != nil {
			return nil, err
		}
		result[record.event.ID] = record
	}
	return result, rows.Err()
}

func calendarPublicationFields(event CalendarEvent) CalendarEvent {
	// These fields come from joins, not the event row. Visibility may change
	// independently without invalidating provider authority.
	event.SourceName, event.SourceColor, event.SourceProvider, event.AccountEmail = "", "", "", ""
	event.SourceHidden = false
	for _, at := range []**time.Time{&event.StartAt, &event.EndAt, &event.ProviderCreatedAt, &event.ProviderUpdatedAt} {
		if *at != nil {
			if (*at).IsZero() {
				*at = nil
			} else {
				value := (*at).UTC()
				*at = &value
			}
		}
	}
	return event
}

func calendarPublicationMatches(expected, actual map[string]calendarPublicationRecord) bool {
	if len(expected) != len(actual) {
		return false
	}
	for id, want := range expected {
		got, ok := actual[id]
		if !ok || want.created != got.created || !reflect.DeepEqual(calendarPublicationFields(want.event), calendarPublicationFields(got.event)) {
			return false
		}
	}
	return true
}

func calendarPublicationEditable(existing, event CalendarEvent, recurrence string, tombstone bool) CalendarEvent {
	existing.ETag, existing.Status = event.ETag, normalizeCalendarEventStatus(event.Status, false)
	existing.Summary, existing.Description, existing.Location = event.Summary, event.Description, event.Location
	existing.AllDay = event.AllDay
	existing.StartDate, existing.EndDate, existing.StartAt, existing.EndAt = "", "", nil, nil
	if event.AllDay {
		existing.StartDate, existing.EndDate = event.StartDate, event.EndDate
	} else {
		existing.StartAt, existing.EndAt = event.StartAt, event.EndAt
	}
	existing.StartTimeZone, existing.EndTimeZone = event.StartTimeZone, event.EndTimeZone
	existing.ProviderUpdatedAt, existing.RecurrenceJSON, existing.IsDeleted = event.ProviderUpdatedAt, recurrence, tombstone
	return existing
}

func calendarPublicationOnline(existing, event CalendarEvent) bool {
	return (existing.SourceProvider == "gmail" || existing.SourceProvider == "outlook") && event.ResponseStatus == "organizer" && event.ICalUID != "" &&
		(existing.ICalUID == "" || existing.ICalUID == event.ICalUID) && json.Valid([]byte(event.AttendeesJSON)) && json.Valid([]byte(event.OnlineMeetingJSON))
}

func calendarPublicationOccurrence(event CalendarEvent) bool {
	if event.SeriesRemoteID == "" || event.SeriesRemoteID == event.RemoteID {
		return false
	}
	value := strings.TrimSpace(event.RecurrenceJSON)
	if value == "" || value == "[]" || value == "{}" || value == "null" {
		return true
	}
	var lines []string
	return json.Unmarshal([]byte(value), &lines) == nil && len(lines) == 1 && strings.HasPrefix(lines[0], "RECURRENCE-ID:")
}

// No public owner/source/local-event argument can retarget the result. Callers
// pass a repository-bound snapshot and a copied provider result. Both event
// fingerprint and exact source/config/grant guards run after the writer wait.
func (db *DB) PublishUserCalendarEvent(ctx context.Context, snapshot *UserCalendarEventSnapshot, kind CalendarEventPublicationKind, event CalendarEvent, guard func(*sql.Tx, string) error) error {
	if snapshot == nil || guard == nil || kind < CalendarPublishUpdate || kind > CalendarPublishResponse {
		return ErrCalendarEventChanged
	}
	event = copyCalendarEvent(event)
	existing := snapshot.Event()
	event.ID, event.UserID, event.SourceID, event.SourceProvider = existing.ID, existing.UserID, existing.SourceID, snapshot.source.Provider
	series := existing.SeriesRemoteID != "" || calendarPublicationHasRecurrence(existing.RecurrenceJSON)
	singleUpdate := kind == CalendarPublishUpdate || kind == CalendarPublishOnlineUpdate || kind == CalendarPublishSeriesConversion
	if singleUpdate && series {
		return ErrCalendarUpdateConflict
	}
	if kind == CalendarPublishDelete && series {
		return ErrCalendarUpdateConflict
	}
	if kind == CalendarPublishOccurrenceUpdate && (!calendarPublicationOccurrence(event) || event.SeriesRemoteID != existing.SeriesRemoteID || event.ICalUID != existing.ICalUID) {
		return ErrCalendarUpdateConflict
	}
	if kind == CalendarPublishOccurrenceDelete && (existing.SeriesRemoteID == "" || existing.SeriesRemoteID == existing.RemoteID || event.SeriesRemoteID != existing.SeriesRemoteID || event.ICalUID != existing.ICalUID || event.ETag == "") {
		return ErrCalendarUpdateConflict
	}
	if kind == CalendarPublishOnlineUpdate && !calendarPublicationOnline(existing, event) {
		return ErrCalendarUpdateConflict
	}
	if kind != CalendarPublishSeriesUpdate && kind != CalendarPublishSeriesDelete && event.RemoteID != existing.RemoteID {
		return ErrCalendarUpdateConflict
	}
	masterID := existing.SeriesRemoteID
	if masterID == "" {
		masterID = existing.RemoteID
	}
	if kind == CalendarPublishSeriesUpdate || kind == CalendarPublishSeriesDelete {
		if !series || event.RemoteID != masterID || event.SeriesRemoteID != "" {
			return ErrCalendarUpdateConflict
		}
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateUserCalendarEventTx(ctx, tx, snapshot, guard); err != nil {
		return err
	}
	clause := ` AND event.id=?`
	args := []any{existing.ID}
	if kind == CalendarPublishSeriesUpdate || kind == CalendarPublishSeriesDelete {
		clause = ` AND (event.series_remote_id=? OR event.remote_id=?)`
		args = []any{masterID, masterID}
	}
	expected, err := calendarPublicationRecordsTx(ctx, tx, existing.UserID, existing.SourceID, clause, args...)
	if err != nil {
		return err
	}
	if _, ok := expected[existing.ID]; !ok {
		return ErrCalendarEventChanged
	}
	var siblings map[string]calendarPublicationRecord
	if (kind == CalendarPublishOccurrenceUpdate || kind == CalendarPublishOccurrenceDelete) && snapshot.source.Provider == CalendarSourceProviderCalDAV {
		siblings, err = calendarPublicationRecordsTx(ctx, tx, existing.UserID, existing.SourceID, ` AND event.series_remote_id=? AND event.ical_uid=?`, existing.SeriesRemoteID, existing.ICalUID)
		if err != nil {
			return err
		}
	}
	switch kind {
	case CalendarPublishUpdate, CalendarPublishOnlineUpdate, CalendarPublishOccurrenceUpdate, CalendarPublishSeriesConversion:
		conversion, occurrence, online := kind == CalendarPublishSeriesConversion, kind == CalendarPublishOccurrenceUpdate, kind == CalendarPublishOnlineUpdate
		err = completeCalendarUpdateTx(ctx, tx, existing.UserID, existing.ID, existing.SourceID, existing.ETag, event, conversion, occurrence, online)
		recurrence := strings.TrimSpace(event.RecurrenceJSON)
		if (!conversion && !occurrence) || recurrence == "" {
			recurrence = "[]"
		}
		row := expected[existing.ID]
		row.event = calendarPublicationEditable(row.event, event, recurrence, conversion)
		if online {
			row.event.ICalUID, row.event.OrganizerName, row.event.OrganizerEmail = event.ICalUID, event.OrganizerName, event.OrganizerEmail
			row.event.ResponseStatus, row.event.AttendeesJSON, row.event.OnlineMeetingJSON = event.ResponseStatus, event.AttendeesJSON, event.OnlineMeetingJSON
		}
		expected[existing.ID] = row
	case CalendarPublishDelete, CalendarPublishOccurrenceDelete:
		var occurrence *CalendarEvent
		if kind == CalendarPublishOccurrenceDelete {
			occurrence = &event
		}
		err = completeCalendarDeleteTx(ctx, tx, existing.UserID, existing.ID, existing.SourceID, existing.ETag, occurrence)
		row := expected[existing.ID]
		row.event.IsDeleted, row.event.Status = true, "cancelled"
		// Legacy DAV occurrence deletion advances the selected resource ETag
		// together with its siblings; Graph/Google keep the deleted version.
		if occurrence != nil && snapshot.source.Provider == CalendarSourceProviderCalDAV {
			row.event.ETag = event.ETag
		}
		expected[existing.ID] = row
	case CalendarPublishSeriesUpdate, CalendarPublishSeriesDelete:
		deleted := kind == CalendarPublishSeriesDelete
		err = completeCalendarSeriesMutationTx(ctx, tx, existing.UserID, existing.ID, existing.SourceID, existing.ETag, event, deleted)
		for id, row := range expected {
			row.event.IsDeleted = true
			if deleted {
				row.event.Status = "cancelled"
			}
			expected[id] = row
		}
	case CalendarPublishResponse:
		err = completeCalendarResponseTx(ctx, tx, existing, event, false)
		row := expected[existing.ID]
		row.event = calendarPublicationEditable(row.event, event, calendarJSON(event.RecurrenceJSON, "[]"), false)
		row.event.OrganizerName, row.event.OrganizerEmail = event.OrganizerName, event.OrganizerEmail
		row.event.ResponseStatus, row.event.AttendeesJSON, row.event.OnlineMeetingJSON, row.event.HTMLLink = event.ResponseStatus, event.AttendeesJSON, calendarJSON(event.OnlineMeetingJSON, "{}"), event.HTMLLink
		expected[existing.ID] = row
	}
	if err != nil {
		return err
	}
	// Original event fingerprint changes because of our publication. The
	// captured source/config and central grant must still match before commit.
	source := &UserCalendarSourceSnapshot{source: snapshot.source, state: snapshot.sourceState}
	if err := validateUserCalendarSourceTx(ctx, tx, source, guard); err != nil {
		return err
	}
	actual, err := calendarPublicationRecordsTx(ctx, tx, existing.UserID, existing.SourceID, clause, args...)
	if err != nil {
		return err
	}
	if !calendarPublicationMatches(expected, actual) {
		return ErrCalendarUpdateConflict
	}
	if siblings != nil {
		// Verify the entire same-parent/UID group, including siblings at a
		// different version which must stay unchanged. Use bounded parameters
		// rather than an IN list proportional to the number of occurrences.
		for id, row := range siblings {
			if row.event.ETag == existing.ETag {
				row.event.ETag = event.ETag
			}
			if id == existing.ID {
				row = expected[id]
			}
			siblings[id] = row
		}
		actual, err := calendarPublicationRecordsTx(ctx, tx, existing.UserID, existing.SourceID, ` AND event.series_remote_id=? AND event.ical_uid=?`, existing.SeriesRemoteID, existing.ICalUID)
		if err != nil {
			return err
		}
		if !calendarPublicationMatches(siblings, actual) {
			return ErrCalendarUpdateConflict
		}
	}
	return tx.Commit()
}

func calendarPublicationHasRecurrence(raw string) bool {
	value := strings.TrimSpace(raw)
	return value != "" && value != "[]" && value != "{}" && value != "null"
}
