package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var ErrCalendarSyncChanged = errors.New("calendar synchronization no longer matches current state")

// A claim copies source authority and a streamed cache fingerprint. It retains
// neither a lease nor the event collection while the provider is being read.
type UserCalendarSyncClaim struct {
	source                   CalendarSource
	start, end               time.Time
	sourceState, eventsState [32]byte
	attempt                  int
}

func (c *UserCalendarSyncClaim) Source() CalendarSource         { return c.source }
func (c *UserCalendarSyncClaim) Window() (time.Time, time.Time) { return c.start, c.end }
func (c *UserCalendarSyncClaim) Attempt() int                   { return c.attempt }

func calendarSyncSourceTx(ctx context.Context, tx *sql.Tx, owner, account, sourceID string, guard func(*sql.Tx, string) error) (CalendarSource, [32]byte, error) {
	catalog, err := calendarDiscoverySnapshotTx(ctx, tx, owner, account, guard)
	if err != nil {
		return CalendarSource{}, [32]byte{}, err
	}
	for _, source := range catalog.catalog {
		if source.ID == sourceID && source.IsSelected && !source.IsDeleted {
			wire, err := json.Marshal(source)
			return source, sha256.Sum256(wire), err
		}
	}
	return CalendarSource{}, [32]byte{}, sql.ErrNoRows
}

// Include tombstones, versions and every stored event field. Timestamp-only
// revisions would miss two updates committed within the same SQLite second.
func calendarSyncEventsStateTx(ctx context.Context, tx *sql.Tx, owner, source string) ([32]byte, error) {
	var foreign int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_events WHERE source_id=? AND user_id<>?`, source, owner).Scan(&foreign); err != nil {
		return [32]byte{}, err
	}
	if foreign != 0 {
		return [32]byte{}, ErrCalendarSyncChanged
	}
	rows, err := tx.QueryContext(ctx, `SELECT * FROM calendar_events WHERE user_id=? AND source_id=? ORDER BY id`, owner, source)
	if err != nil {
		return [32]byte{}, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return [32]byte{}, err
	}
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	if err := encoder.Encode(columns); err != nil {
		return [32]byte{}, err
	}
	values, destinations := make([]any, len(columns)), make([]any, len(columns))
	for i := range values {
		destinations[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(destinations...); err != nil {
			return [32]byte{}, err
		}
		if err := encoder.Encode(values); err != nil {
			return [32]byte{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return [32]byte{}, err
	}
	var result [32]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func (db *DB) StartUserCalendarSync(ctx context.Context, owner, account, source string, start, end time.Time, guard func(*sql.Tx, string) error, inspect func(*sql.Tx) error) (*UserCalendarSyncClaim, error) {
	if start.IsZero() || end.IsZero() || !end.After(start) || guard == nil {
		return nil, ErrCalendarSyncChanged
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	c := &UserCalendarSyncClaim{start: start, end: end}
	c.source, c.sourceState, err = calendarSyncSourceTx(ctx, tx, owner, account, source, guard)
	if err != nil {
		return nil, err
	}
	if inspect != nil {
		if err := inspect(tx); err != nil {
			return nil, err
		}
	}
	c.eventsState, err = calendarSyncEventsStateTx(ctx, tx, owner, source)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO calendar_sync_state(source_id) VALUES(?) ON CONFLICT DO NOTHING`, source); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT attempt_count FROM calendar_sync_state WHERE source_id=?`, source).Scan(&c.attempt); err != nil {
		return nil, err
	}
	c.attempt++
	result, err := tx.ExecContext(ctx, `UPDATE calendar_sync_state SET state='syncing',attempt_count=attempt_count+1,last_started_at=CURRENT_TIMESTAMP,last_error='',updated_at=CURRENT_TIMESTAMP WHERE source_id=?`, source)
	if err := calendarSyncOneRow(result, err); err != nil {
		return nil, err
	}
	if err := validateUserCalendarSyncTx(ctx, tx, c, guard, false); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return c, nil
}

func calendarSyncOneRow(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrCalendarSyncChanged
	}
	return nil
}

func validateUserCalendarSyncTx(ctx context.Context, tx *sql.Tx, c *UserCalendarSyncClaim, guard func(*sql.Tx, string) error, cache bool) error {
	if c == nil || c.source.UserID == "" || c.source.AccountID == "" || c.source.ID == "" || c.attempt < 1 || guard == nil {
		return ErrCalendarSyncChanged
	}
	_, state, err := calendarSyncSourceTx(ctx, tx, c.source.UserID, c.source.AccountID, c.source.ID, guard)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCalendarSyncChanged
	}
	if err != nil {
		return err
	}
	if state != c.sourceState {
		return ErrCalendarSyncChanged
	}
	var found int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_sync_state WHERE source_id=? AND state='syncing' AND attempt_count=?`, c.source.ID, c.attempt).Scan(&found); err != nil {
		return err
	}
	if found != 1 {
		return ErrCalendarSyncChanged
	}
	if cache {
		state, err := calendarSyncEventsStateTx(ctx, tx, c.source.UserID, c.source.ID)
		if err != nil {
			return err
		}
		if state != c.eventsState {
			return ErrCalendarSyncChanged
		}
	}
	return nil
}

func (db *DB) ValidateUserCalendarSync(ctx context.Context, c *UserCalendarSyncClaim, guard func(*sql.Tx, string) error) error {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Dispatch checks authority/attempt identity. The complete cache comparison
	// runs at publication, rather than rescanning all events for every page.
	if err := validateUserCalendarSyncTx(ctx, tx, c, guard, false); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) PublishUserCalendarSync(ctx context.Context, c *UserCalendarSyncClaim, events []CalendarEvent, next time.Time, guard func(*sql.Tx, string) error) error {
	if c == nil || guard == nil || next.IsZero() {
		return ErrCalendarSyncChanged
	}
	inputs := append([]CalendarEvent(nil), events...)
	for i := range inputs {
		event := &inputs[i]
		if strings.TrimSpace(event.RemoteID) == "" {
			return ErrCalendarSyncChanged
		}
		event.ID, event.UserID, event.SourceID = "", c.source.UserID, c.source.ID
		for _, at := range []**time.Time{&event.StartAt, &event.EndAt, &event.ProviderCreatedAt, &event.ProviderUpdatedAt} {
			if *at != nil {
				value := **at
				*at = &value
			}
		}
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateUserCalendarSyncTx(ctx, tx, c, guard, true); err != nil {
		return err
	}
	if err := replaceCalendarEventsTx(ctx, tx, c.source.UserID, c.source.ID, inputs, c.start, c.end); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE calendar_sync_state SET next_attempt_at=? WHERE source_id=? AND state='ok' AND attempt_count=?`, formatDBTime(next), c.source.ID, c.attempt)
	if err := calendarSyncOneRow(result, err); err != nil {
		return err
	}
	return tx.Commit()
}

// Failure never changes event data. A replaced claim/configuration cannot mark
// a newer attempt failed. Cancellation uses the caller's joined runtime context.
func (db *DB) FailUserCalendarSync(ctx context.Context, c *UserCalendarSyncClaim, message string, next time.Time, guard func(*sql.Tx, string) error) error {
	if c == nil || guard == nil || next.IsZero() {
		return ErrCalendarSyncChanged
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateUserCalendarSyncTx(ctx, tx, c, guard, false); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE calendar_sync_state SET state='failed',last_error=?,next_attempt_at=?,updated_at=CURRENT_TIMESTAMP WHERE source_id=? AND state='syncing' AND attempt_count=?`, message, formatDBTime(next), c.source.ID, c.attempt)
	if err := calendarSyncOneRow(result, err); err != nil {
		return err
	}
	return tx.Commit()
}
