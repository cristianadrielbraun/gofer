package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"time"
)

// Explicit retry retains the same immutable message, original invitation and
// original reservation. It only renews the private validated configuration
// proof, after native preflight, and makes this exact attempt eligible again.
func (db *DB) RequeueUserCalendarReply(ctx context.Context, control *UserCalendarReplyControlSnapshot, event *UserCalendarEventSnapshot, original CalendarResponseClaimIdentity, payload string, confirmed bool, guard func(*sql.Tx, string) error) error {
	if control == nil || event == nil || original.ID == "" || !json.Valid([]byte(payload)) || control.job.State != "pending" || control.job.UserID != event.event.UserID || control.job.SourceID != event.source.ID || control.send.AccountID != event.source.AccountID || event.source.Provider != CalendarSourceProviderCalDAV || original.RemoteID != control.job.RemoteID || original.Version != control.job.Version || original.Response != control.job.Response {
		return ErrCalendarEventChanged
	}
	if (confirmed && control.send.Status != OutgoingSendAmbiguous) || (!confirmed && control.send.Status != OutgoingSendFailed) {
		return ErrOutgoingSendNotRetryable
	}
	remote, err := calendarResponseClaimTarget(event, original.Scope, original.Version, original.Response)
	if err != nil || remote != original.RemoteID {
		return ErrCalendarEventChanged
	}
	response := &UserCalendarResponseClaim{snapshot: event, id: original.ID, remote: original.RemoteID, version: original.Version, response: original.Response, scope: original.Scope}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateCalendarReplyControlTx(ctx, tx, control, guard); err != nil {
		return err
	}
	validate := func() error {
		if err := validateUserCalendarEventTx(ctx, tx, event, guard); err != nil {
			return err
		}
		return calendarResponseClaimMatchesTx(ctx, tx, response)
	}
	if err := validate(); err != nil {
		return err
	}
	at := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE outgoing_sends SET status='pending',last_error='',locked_at=NULL,next_attempt_at=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND account_id=? AND status=? AND attempt_count=?`, at, control.send.ID, control.send.AccountID, control.send.Status, control.send.AttemptCount)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrCalendarEventChanged
	}
	result, err = tx.ExecContext(ctx, `UPDATE calendar_reply_jobs SET payload=? WHERE id=? AND user_id=? AND state='pending' AND payload=?`, payload, control.job.ID, control.job.UserID, control.job.Payload)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrCalendarEventChanged
	}
	expected := *control
	expected.job.Payload, expected.job.SendStatus = payload, OutgoingSendPending
	expected.send.Status, expected.send.LastError, expected.send.NextAttemptAt = OutgoingSendPending, "", at
	if err := validateCalendarReplyControlTx(ctx, tx, &expected, guard); err != nil {
		return err
	}
	var unlocked bool
	if err := tx.QueryRowContext(ctx, `SELECT locked_at IS NULL FROM outgoing_sends WHERE id=?`, control.send.ID).Scan(&unlocked); err != nil {
		return err
	}
	if !unlocked {
		return ErrCalendarEventChanged
	}
	current, err := scanOutgoingSend(tx.QueryRowContext(ctx, outgoingSendSelect+` WHERE id=?`, control.send.ID))
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, expected.send) {
		return ErrCalendarEventChanged
	}
	if err := validate(); err != nil {
		return err
	}
	return tx.Commit()
}
