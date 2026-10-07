package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
)

// Persist the original draft and principal with a random reservation identity
// in the existing request_hash column. Passwords and OAuth tokens never enter
// this record; credential repair cannot redirect a pending create to a new
// calendar. The nonce distinguishes delete/reinsert reservations as well.
type userCalendarCreateAuthority struct {
	Format       int
	Binding      [32]byte
	Draft, Nonce string
}

type userCalendarCreateRecord struct {
	Source, Hash, Created string
	Result                CalendarCreateRequest
}

type UserCalendarCreateClaim struct {
	source  *UserCalendarSourceSnapshot
	request string
	record  userCalendarCreateRecord
}

func (c *UserCalendarCreateClaim) Result() CalendarCreateRequest { return c.record.Result }
func (c *UserCalendarCreateClaim) RequestID() string             { return c.request }

func calendarCreateRecordTx(ctx context.Context, tx *sql.Tx, owner, request string) (userCalendarCreateRecord, error) {
	var row userCalendarCreateRecord
	err := tx.QueryRowContext(ctx, `SELECT source_id,request_hash,event_id,remote_id,created_at FROM calendar_create_requests WHERE user_id=? AND request_id=?`, owner, request).Scan(&row.Source, &row.Hash, &row.Result.EventID, &row.Result.RemoteID, &row.Created)
	return row, err
}

func calendarCreateClaimMatchesTx(ctx context.Context, tx *sql.Tx, claim *UserCalendarCreateClaim) error {
	if claim == nil || claim.source == nil || claim.request == "" {
		return ErrCalendarCreateConflict
	}
	row, err := calendarCreateRecordTx(ctx, tx, claim.source.source.UserID, claim.request)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCalendarCreateConflict
	}
	if err != nil {
		return err
	}
	if row != claim.record {
		return ErrCalendarCreateConflict
	}
	return nil
}

// Pending legacy rows have no original-principal proof. Completed legacy rows
// can be read back without another provider write; pending rows require explicit
// migration binding rather than silently adopting the current principal.
func (db *DB) BeginUserCalendarCreate(ctx context.Context, source *UserCalendarSourceSnapshot, request, hash string, binding [32]byte, guard func(*sql.Tx, string) error) (*UserCalendarCreateClaim, error) {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	claim, _, err := beginUserCalendarCreateTx(ctx, tx, source, request, hash, binding, guard)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claim, nil
}

func beginUserCalendarCreateTx(ctx context.Context, tx *sql.Tx, source *UserCalendarSourceSnapshot, request, hash string, binding [32]byte, guard func(*sql.Tx, string) error) (*UserCalendarCreateClaim, bool, error) {
	if source == nil || guard == nil || request == "" || len(request) > 128 || hash == "" || len(hash) > 4096 || binding == ([32]byte{}) {
		return nil, false, ErrCalendarCreateConflict
	}
	proof := userCalendarCreateAuthority{Format: 1, Binding: binding, Draft: hash, Nonce: uuid.NewString()}
	wire, err := json.Marshal(proof)
	if err != nil {
		return nil, false, err
	}
	if err := validateUserCalendarSourceTx(ctx, tx, source, guard); err != nil {
		return nil, false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO calendar_create_requests(user_id,request_id,source_id,request_hash) VALUES(?,?,?,?) ON CONFLICT(user_id,request_id) DO NOTHING`, source.source.UserID, request, source.source.ID, string(wire))
	if err != nil {
		return nil, false, err
	}
	row, err := calendarCreateRecordTx(ctx, tx, source.source.UserID, request)
	if err != nil {
		return nil, false, err
	}
	if row.Source != source.source.ID {
		return nil, false, ErrCalendarCreateConflict
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	if inserted == 1 && (row.Hash != string(wire) || row.Result != (CalendarCreateRequest{})) {
		return nil, false, ErrCalendarCreateConflict
	}
	var saved userCalendarCreateAuthority
	decoded := json.Unmarshal([]byte(row.Hash), &saved)
	nonce, nonceErr := uuid.Parse(saved.Nonce)
	if decoded != nil || saved.Format != 1 || nonceErr != nil || nonce == uuid.Nil || saved.Nonce != nonce.String() || saved.Draft != hash || saved.Binding != binding {
		if row.Hash != hash || row.Result.RemoteID == "" {
			return nil, false, ErrCalendarCreateConflict
		}
	}
	claim := &UserCalendarCreateClaim{source: source, request: request, record: row}
	if err := validateUserCalendarSourceTx(ctx, tx, source, guard); err != nil {
		return nil, false, err
	}
	if err := calendarCreateClaimMatchesTx(ctx, tx, claim); err != nil {
		return nil, false, err
	}
	return claim, inserted == 1, nil
}

func (db *DB) ValidateUserCalendarCreate(ctx context.Context, claim *UserCalendarCreateClaim, guard func(*sql.Tx, string) error) error {
	if claim == nil || claim.source == nil || guard == nil {
		return ErrCalendarCreateConflict
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateUserCalendarSourceTx(ctx, tx, claim.source, guard); err != nil {
		return err
	}
	if err := calendarCreateClaimMatchesTx(ctx, tx, claim); err != nil {
		return err
	}
	return tx.Commit()
}

// The accepted provider result and durable request commit together. The copied
// source, current service/grant and exact reservation are checked after waiting
// for the writer and again after publication. No provider work holds this TX.
func (db *DB) PublishUserCalendarCreate(ctx context.Context, claim *UserCalendarCreateClaim, event CalendarEvent, series bool, guard func(*sql.Tx, string) error) (CalendarCreateRequest, error) {
	if claim == nil || claim.source == nil || guard == nil || claim.record.Result != (CalendarCreateRequest{}) {
		return CalendarCreateRequest{}, ErrCalendarCreateConflict
	}
	event = copyCalendarEvent(event)
	source := claim.source.source
	event.UserID, event.SourceID = source.UserID, source.ID
	if strings.TrimSpace(event.RemoteID) == "" || event.IsDeleted || event.SeriesRemoteID != "" || (series && !calendarPublicationHasRecurrence(event.RecurrenceJSON)) {
		return CalendarCreateRequest{}, ErrCalendarCreateConflict
	}
	if !series {
		if calendarPublicationHasRecurrence(event.RecurrenceJSON) {
			return CalendarCreateRequest{}, ErrCalendarCreateConflict
		}
		if err := validateCalendarCreateEvent(event); err != nil {
			return CalendarCreateRequest{}, err
		}
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return CalendarCreateRequest{}, err
	}
	defer tx.Rollback()
	if err := validateUserCalendarSourceTx(ctx, tx, claim.source, guard); err != nil {
		return CalendarCreateRequest{}, err
	}
	if err := calendarCreateClaimMatchesTx(ctx, tx, claim); err != nil {
		return CalendarCreateRequest{}, err
	}
	result := CalendarCreateRequest{RemoteID: event.RemoteID}
	var expected map[string]calendarPublicationRecord
	if series {
		updated, err := tx.ExecContext(ctx, `UPDATE calendar_create_requests SET remote_id=? WHERE user_id=? AND request_id=? AND source_id=? AND request_hash=? AND event_id='' AND remote_id=''`, event.RemoteID, source.UserID, claim.request, source.ID, claim.record.Hash)
		if err != nil {
			return CalendarCreateRequest{}, err
		}
		if n, err := updated.RowsAffected(); err != nil || n != 1 {
			return CalendarCreateRequest{}, ErrCalendarCreateConflict
		}
	} else {
		expected, err = calendarPublicationRecordsTx(ctx, tx, source.UserID, source.ID, ` AND event.remote_id=?`, event.RemoteID)
		if err != nil {
			return CalendarCreateRequest{}, err
		}
		result, err = completeCalendarCreateTx(ctx, tx, source.UserID, source.ID, claim.request, claim.record.Hash, event)
		if err != nil {
			return CalendarCreateRequest{}, err
		}
	}
	if err := validateUserCalendarSourceTx(ctx, tx, claim.source, guard); err != nil {
		return CalendarCreateRequest{}, err
	}
	finished := *claim
	finished.record.Result = result
	if err := calendarCreateClaimMatchesTx(ctx, tx, &finished); err != nil {
		return CalendarCreateRequest{}, err
	}
	if !series {
		actual, err := calendarPublicationRecordsTx(ctx, tx, source.UserID, source.ID, ` AND event.remote_id=?`, event.RemoteID)
		if err != nil {
			return CalendarCreateRequest{}, err
		}
		if len(expected) > 0 {
			// A sync may already have cached the provider-confirmed identity. Preserve
			// that row exactly instead of overwriting newer provider content.
			if !calendarPublicationMatches(expected, actual) {
				return CalendarCreateRequest{}, ErrCalendarCreateConflict
			}
		} else {
			row, ok := actual[result.EventID]
			want := event
			want.ID, want.SourceProvider = result.EventID, source.Provider
			want.Status = normalizeCalendarEventStatus(event.Status, false)
			want.AttendeesJSON, want.OnlineMeetingJSON, want.RecurrenceJSON = calendarJSON(event.AttendeesJSON, "[]"), calendarJSON(event.OnlineMeetingJSON, "{}"), "[]"
			want.SeriesRemoteID = ""
			if want.AllDay {
				want.StartAt, want.EndAt = nil, nil
			} else {
				want.StartDate, want.EndDate = "", ""
			}
			if !ok || len(actual) != 1 || !calendarPublicationMatches(map[string]calendarPublicationRecord{result.EventID: {event: want, created: row.created}}, actual) {
				return CalendarCreateRequest{}, ErrCalendarCreateConflict
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return CalendarCreateRequest{}, err
	}
	return result, nil
}
