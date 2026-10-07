package storage

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
)

// A local recovery snapshot grants no provider access. It retains the exact
// job and delivery attempt even when Calendar settings are no longer usable.
type UserCalendarReplyControlSnapshot struct {
	job  CalendarReplyJob
	send OutgoingSend
	at   sqliteNullTime
}

func (s *UserCalendarReplyControlSnapshot) Job() CalendarReplyJob { return s.job }
func (s *UserCalendarReplyControlSnapshot) AccountID() string     { return s.send.AccountID }

func calendarReplyControlOwnerTx(ctx context.Context, tx *sql.Tx, owner, account string, guard func(*sql.Tx, string) error) error {
	if owner == "" || account == "" || guard == nil {
		return ErrAccountRoute
	}
	if err := guard(tx, account); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM accounts a JOIN users u ON u.id=a.user_id WHERE a.id=? AND a.user_id=? AND COALESCE(a.is_deleting,0)=0 AND u.status='active' AND u.deletion_pending=0`, account, owner).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return ErrAccountRoute
	}
	return nil
}

func (db *DB) SnapshotUserCalendarReplyControl(ctx context.Context, owner, id string, guard func(*sql.Tx, string) error) (*UserCalendarReplyControlSnapshot, error) {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	job, err := scanCalendarReply(tx.QueryRowContext(ctx, calendarReplySelect+`WHERE j.id=? AND j.user_id=?`, id, owner))
	if err != nil {
		return nil, err
	}
	send, err := scanOutgoingSend(tx.QueryRowContext(ctx, outgoingSendSelect+` WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	if err := calendarReplyControlOwnerTx(ctx, tx, owner, send.AccountID, guard); err != nil {
		return nil, err
	}
	s := &UserCalendarReplyControlSnapshot{job: job, send: send}
	if err := tx.QueryRowContext(ctx, `SELECT attempted_at FROM calendar_reply_jobs WHERE id=? AND user_id=?`, id, owner).Scan(&s.at); err != nil {
		return nil, err
	}
	if err := guard(tx, send.AccountID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s, nil
}

func validateCalendarReplyControlTx(ctx context.Context, tx *sql.Tx, s *UserCalendarReplyControlSnapshot, guard func(*sql.Tx, string) error) error {
	if s == nil || s.job.ID == "" || s.job.ID != s.send.ID || s.job.SendStatus != s.send.Status {
		return ErrCalendarEventChanged
	}
	if err := calendarReplyControlOwnerTx(ctx, tx, s.job.UserID, s.send.AccountID, guard); err != nil {
		return err
	}
	job, err := scanCalendarReply(tx.QueryRowContext(ctx, calendarReplySelect+`WHERE j.id=? AND j.user_id=?`, s.job.ID, s.job.UserID))
	if err != nil {
		return err
	}
	if job != s.job {
		return ErrCalendarEventChanged
	}
	var at sqliteNullTime
	if err := tx.QueryRowContext(ctx, `SELECT attempted_at FROM calendar_reply_jobs WHERE id=? AND user_id=?`, s.job.ID, s.job.UserID).Scan(&at); err != nil {
		return err
	}
	if at.Valid != s.at.Valid || (at.Valid && !at.Time.Equal(s.at.Time)) {
		return ErrCalendarEventChanged
	}
	send, err := scanOutgoingSend(tx.QueryRowContext(ctx, outgoingSendSelect+` WHERE id=?`, s.job.ID))
	if err != nil {
		return err
	}
	if s.send.Status == OutgoingSendSent {
		// Sent-copy cleanup is independent of dismissing the Calendar notice.
		if send.AccountID != s.send.AccountID || send.Transport != s.send.Transport || send.Status != s.send.Status || send.AttemptCount != s.send.AttemptCount || send.SentMessageID != s.send.SentMessageID || send.MessageID != s.send.MessageID || send.DraftID != s.send.DraftID || send.IsScheduled != s.send.IsScheduled {
			return ErrCalendarEventChanged
		}
	} else if !reflect.DeepEqual(send, s.send) {
		return ErrCalendarEventChanged
	}
	return guard(tx, s.send.AccountID)
}

type calendarReplyControlReservation struct {
	present         bool
	nonce, response string
	created         sqliteNullTime
}

func readCalendarReplyControlReservation(ctx context.Context, tx *sql.Tx, job CalendarReplyJob) (calendarReplyControlReservation, error) {
	var r calendarReplyControlReservation
	err := tx.QueryRowContext(ctx, `SELECT claim_id,response,created_at FROM calendar_response_requests WHERE user_id=? AND source_id=? AND remote_id=? AND version=?`, job.UserID, job.SourceID, job.RemoteID, job.Version).Scan(&r.nonce, &r.response, &r.created)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	r.present = err == nil
	return r, err
}

func calendarReplyControlReservationEqual(a, b calendarReplyControlReservation) bool {
	return a.present == b.present && a.nonce == b.nonce && a.response == b.response && a.created.Valid == b.created.Valid && (!a.created.Valid || a.created.Time.Equal(b.created.Time))
}

// Cancel and confirm-sent never dispatch mail. Dismiss only retires a notice.
// A known original nonce can be released on cancellation; historical or
// replaced reservations remain intact rather than being inferred from row IDs.
func (db *DB) ApplyUserCalendarReplyControl(ctx context.Context, s *UserCalendarReplyControlSnapshot, action string, original CalendarResponseClaimIdentity, guard func(*sql.Tx, string) error) error {
	if s == nil {
		return ErrCalendarEventChanged
	}
	switch action {
	case "cancel":
		if s.job.State != "pending" || (s.send.Status != OutgoingSendPending && s.send.Status != OutgoingSendFailed) {
			return ErrOutgoingSendNotCancelable
		}
	case "confirm-sent":
		if s.job.State != "pending" || s.send.Status != OutgoingSendAmbiguous {
			return ErrOutgoingSendNotRetryable
		}
	case "dismiss":
		if s.job.State != "conflict" {
			return ErrCalendarEventChanged
		}
	default:
		return ErrCalendarEventChanged
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateCalendarReplyControlTx(ctx, tx, s, guard); err != nil {
		return err
	}
	reservation, err := readCalendarReplyControlReservation(ctx, tx, s.job)
	if err != nil {
		return err
	}
	expected := *s
	if action == "cancel" || action == "confirm-sent" {
		status, note := OutgoingSendSent, ""
		if action == "cancel" {
			status, note = OutgoingSendCanceled, "Canceled by the user"
		}
		result, err := tx.ExecContext(ctx, `UPDATE outgoing_sends SET status=?,last_error=?,locked_at=NULL,updated_at=CURRENT_TIMESTAMP WHERE id=? AND account_id=? AND status=? AND attempt_count=?`, status, note, s.send.ID, s.send.AccountID, s.send.Status, s.send.AttemptCount)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrCalendarEventChanged
		}
		expected.send.Status, expected.send.LastError, expected.job.SendStatus = status, note, status
		var unlocked bool
		if err := tx.QueryRowContext(ctx, `SELECT locked_at IS NULL FROM outgoing_sends WHERE id=?`, s.send.ID).Scan(&unlocked); err != nil {
			return err
		}
		if !unlocked {
			return ErrCalendarEventChanged
		}
		// Confirmation must preserve every original payload and Sent-copy field.
		actual, err := scanOutgoingSend(tx.QueryRowContext(ctx, outgoingSendSelect+` WHERE id=?`, s.send.ID))
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(actual, expected.send) {
			return ErrCalendarEventChanged
		}
	}
	if action == "cancel" {
		if original.ID != "" && original.RemoteID == s.job.RemoteID && original.Version == s.job.Version && original.Response == s.job.Response && reservation.present && reservation.nonce == original.ID && reservation.response == original.Response {
			result, err := tx.ExecContext(ctx, `DELETE FROM calendar_response_requests WHERE user_id=? AND source_id=? AND remote_id=? AND version=? AND claim_id=? AND response=?`, s.job.UserID, s.job.SourceID, s.job.RemoteID, s.job.Version, original.ID, original.Response)
			if err != nil {
				return err
			}
			if n, err := result.RowsAffected(); err != nil || n != 1 {
				return ErrCalendarEventChanged
			}
			reservation = calendarReplyControlReservation{}
		}
		expected.job.State = "canceled"
	} else if action == "dismiss" {
		expected.job.State = "dismissed"
	}
	if expected.job.State != s.job.State {
		result, err := tx.ExecContext(ctx, `UPDATE calendar_reply_jobs SET state=? WHERE id=? AND user_id=? AND state=?`, expected.job.State, s.job.ID, s.job.UserID, s.job.State)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrCalendarEventChanged
		}
	}
	if err := validateCalendarReplyControlTx(ctx, tx, &expected, guard); err != nil {
		return err
	}
	current, err := readCalendarReplyControlReservation(ctx, tx, s.job)
	if err != nil {
		return err
	}
	if !calendarReplyControlReservationEqual(current, reservation) {
		return ErrCalendarEventChanged
	}
	return tx.Commit()
}
