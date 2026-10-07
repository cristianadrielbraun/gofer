package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrCalendarEventChanged = errors.New("calendar event no longer matches current state")
var ErrCalendarSourceChanged = errors.New("calendar source no longer matches current state")

// Creates and capability checks have a selected source but no cached event yet.
// Authority excludes visibility and sync progress; the copied display preference
// is still available to render the result without holding a repository lease.
type UserCalendarSourceSnapshot struct {
	source CalendarSource
	state  [32]byte
}

func (s *UserCalendarSourceSnapshot) Source() CalendarSource { return s.source }

// Fingerprints identify copied authority without persisting provider secrets.
func (s *UserCalendarSourceSnapshot) Fingerprint() [32]byte { return s.state }

func (db *DB) SnapshotUserCalendarSource(ctx context.Context, owner, id string, guard func(*sql.Tx, string) error, inspect func(*sql.Tx, string) error) (*UserCalendarSourceSnapshot, error) {
	if guard == nil || owner == "" || id == "" {
		return nil, ErrCalendarSourceChanged
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var account string
	if err := tx.QueryRowContext(ctx, `SELECT account_id FROM calendar_sources WHERE id=? AND user_id=? AND is_selected=1 AND is_deleted=0`, id, owner).Scan(&account); err != nil {
		return nil, err
	}
	s := &UserCalendarSourceSnapshot{}
	s.source, s.state, err = calendarSyncSourceTx(ctx, tx, owner, account, id, guard)
	if err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT is_hidden FROM calendar_sources WHERE id=? AND user_id=? AND account_id=?`, id, owner, account).Scan(&s.source.IsHidden); err != nil {
		return nil, err
	}
	if inspect != nil {
		if err := inspect(tx, account); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s, nil
}

// Action publications call this after acquiring the writer. A separate earlier
// read validation cannot protect an action which waited for that writer.
func validateUserCalendarSourceTx(ctx context.Context, tx *sql.Tx, s *UserCalendarSourceSnapshot, guard func(*sql.Tx, string) error) error {
	if s == nil || guard == nil || s.source.ID == "" || s.source.UserID == "" || s.source.AccountID == "" {
		return ErrCalendarSourceChanged
	}
	_, state, err := calendarSyncSourceTx(ctx, tx, s.source.UserID, s.source.AccountID, s.source.ID, guard)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCalendarSourceChanged
	}
	if err != nil {
		return err
	}
	if state != s.state {
		return ErrCalendarSourceChanged
	}
	return nil
}

func (db *DB) ValidateUserCalendarSource(ctx context.Context, s *UserCalendarSourceSnapshot, guard func(*sql.Tx, string) error) error {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateUserCalendarSourceTx(ctx, tx, s, guard); err != nil {
		return err
	}
	return tx.Commit()
}

// Event actions capture one owned event and its selected source in the same
// read transaction. The copy survives eviction without retaining a store lease.
// Unlike a sync claim, this snapshot neither starts nor changes sync progress.
type UserCalendarEventSnapshot struct {
	source                  CalendarSource
	event                   CalendarEvent
	sourceState, eventState [32]byte
}

func copyCalendarEvent(event CalendarEvent) CalendarEvent {
	for _, at := range []**time.Time{&event.StartAt, &event.EndAt, &event.ProviderCreatedAt, &event.ProviderUpdatedAt} {
		if *at != nil {
			value := **at
			*at = &value
		}
	}
	return event
}

func (s *UserCalendarEventSnapshot) Event() CalendarEvent   { return copyCalendarEvent(s.event) }
func (s *UserCalendarEventSnapshot) Source() CalendarSource { return s.source }
func (s *UserCalendarEventSnapshot) Fingerprints() (source, event [32]byte) {
	return s.sourceState, s.eventState
}

func calendarEventStateTx(ctx context.Context, tx *sql.Tx, event CalendarEvent) ([32]byte, error) {
	rows, err := tx.QueryContext(ctx, `SELECT * FROM calendar_events WHERE id=? AND user_id=? AND source_id=? AND is_deleted=0`, event.ID, event.UserID, event.SourceID)
	if err != nil {
		return [32]byte{}, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return [32]byte{}, err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return [32]byte{}, err
		}
		return [32]byte{}, ErrCalendarEventChanged
	}
	values, destinations := make([]any, len(columns)), make([]any, len(columns))
	for i := range values {
		destinations[i] = &values[i]
	}
	if err := rows.Scan(destinations...); err != nil {
		return [32]byte{}, err
	}
	wire, err := json.Marshal([]any{columns, values})
	if err != nil {
		return [32]byte{}, err
	}
	if rows.Next() {
		return [32]byte{}, ErrCalendarEventChanged
	}
	if err := rows.Err(); err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(wire), nil
}

// Account identity is derived from the owned cached event, never a browser
// account/source parameter. inspect copies account configuration in this TX.
func (db *DB) SnapshotUserCalendarEvent(ctx context.Context, owner, id string, guard func(*sql.Tx, string) error, inspect func(*sql.Tx, string) error) (*UserCalendarEventSnapshot, error) {
	if guard == nil || owner == "" || id == "" {
		return nil, ErrCalendarEventChanged
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	s := &UserCalendarEventSnapshot{}
	s.event, err = scanCalendarEvent(tx.QueryRowContext(ctx, calendarEventSelect+` AND event.id=?`, owner, id))
	if err != nil {
		return nil, err
	}
	var account string
	if err := tx.QueryRowContext(ctx, `SELECT account_id FROM calendar_sources WHERE id=? AND user_id=?`, s.event.SourceID, owner).Scan(&account); err != nil {
		return nil, err
	}
	s.source, s.sourceState, err = calendarSyncSourceTx(ctx, tx, owner, account, s.event.SourceID, guard)
	if err != nil {
		return nil, err
	}
	if inspect != nil {
		if err := inspect(tx, account); err != nil {
			return nil, err
		}
	}
	s.eventState, err = calendarEventStateTx(ctx, tx, s.event)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s, nil
}

// This helper is also the publication boundary for subsequent action-specific
// transactions: call it after acquiring the writer, before changing local rows.
func validateUserCalendarEventTx(ctx context.Context, tx *sql.Tx, s *UserCalendarEventSnapshot, guard func(*sql.Tx, string) error) error {
	if s == nil || guard == nil || s.event.ID == "" || s.source.UserID == "" || s.source.AccountID == "" {
		return ErrCalendarEventChanged
	}
	_, state, err := calendarSyncSourceTx(ctx, tx, s.source.UserID, s.source.AccountID, s.source.ID, guard)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCalendarEventChanged
	}
	if err != nil {
		return err
	}
	if state != s.sourceState {
		return ErrCalendarEventChanged
	}
	state, err = calendarEventStateTx(ctx, tx, s.event)
	if err != nil {
		return err
	}
	if state != s.eventState {
		return ErrCalendarEventChanged
	}
	return nil
}

func (db *DB) ValidateUserCalendarEvent(ctx context.Context, s *UserCalendarEventSnapshot, guard func(*sql.Tx, string) error) error {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateUserCalendarEventTx(ctx, tx, s, guard); err != nil {
		return err
	}
	return tx.Commit()
}
