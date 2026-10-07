package storage

import (
	"context"
	"database/sql"
	"time"
)

// A strictly increasing persisted attempted_at value distinguishes each local
// followup generation. It survives eviction/restart without a new schema table.
// Retrying always reads the desired DAV representation before any conditional
// PUT, so an uncertain previous save cannot trigger another email.
type UserCalendarReplyFollowupClaim struct {
	job       CalendarReplyJob
	response  CalendarResponseClaimIdentity
	source    *UserCalendarSourceSnapshot
	send      OutgoingSend
	sendState [32]byte
	at        time.Time
}

func calendarReplyFollowupGenerationTx(ctx context.Context, tx *sql.Tx, claim *UserCalendarReplyFollowupClaim) error {
	if claim == nil || claim.at.IsZero() {
		return ErrCalendarEventChanged
	}
	var at sqliteNullTime
	if err := tx.QueryRowContext(ctx, `SELECT attempted_at FROM calendar_reply_jobs WHERE id=? AND user_id=?`, claim.job.ID, claim.job.UserID).Scan(&at); err != nil {
		return err
	}
	if !at.Valid || !at.Time.Equal(claim.at) {
		return ErrCalendarEventChanged
	}
	return nil
}

func (db *DB) StartUserCalendarReplyFollowup(ctx context.Context, job CalendarReplyJob, response CalendarResponseClaimIdentity, source *UserCalendarSourceSnapshot, send OutgoingSend, state [32]byte, guard func(*sql.Tx, string) error) (*UserCalendarReplyFollowupClaim, error) {
	if job.State != "pending" || job.SendStatus != OutgoingSendSent || send.Status != OutgoingSendSent {
		return nil, ErrCalendarEventChanged
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := validateUserCalendarReplyProofTx(ctx, tx, job, response, source, nil, send, state, guard); err != nil {
		return nil, err
	}
	var previous sqliteNullTime
	if err := tx.QueryRowContext(ctx, `SELECT attempted_at FROM calendar_reply_jobs WHERE id=? AND user_id=?`, job.ID, job.UserID).Scan(&previous); err != nil {
		return nil, err
	}
	at := time.Now().UTC()
	if previous.Valid && !at.After(previous.Time) {
		at = previous.Time.Add(time.Nanosecond)
	}
	result, err := tx.ExecContext(ctx, `UPDATE calendar_reply_jobs SET attempted_at=? WHERE id=? AND user_id=? AND state='pending'`, at, job.ID, job.UserID)
	if err != nil {
		return nil, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return nil, ErrCalendarEventChanged
	}
	claim := &UserCalendarReplyFollowupClaim{job: job, response: response, source: source, send: send, sendState: state, at: at}
	if err := calendarReplyFollowupGenerationTx(ctx, tx, claim); err != nil {
		return nil, err
	}
	if err := validateUserCalendarReplyProofTx(ctx, tx, job, response, source, nil, send, state, guard); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claim, nil
}

func (db *DB) ValidateUserCalendarReplyFollowup(ctx context.Context, claim *UserCalendarReplyFollowupClaim, guard func(*sql.Tx, string) error) error {
	if claim == nil {
		return ErrCalendarEventChanged
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := calendarReplyFollowupGenerationTx(ctx, tx, claim); err != nil {
		return err
	}
	if err := validateUserCalendarReplyProofTx(ctx, tx, claim.job, claim.response, claim.source, nil, claim.send, claim.sendState, guard); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) FinishUserCalendarReplyFollowup(ctx context.Context, claim *UserCalendarReplyFollowupClaim, state string, guard func(*sql.Tx, string) error) error {
	if claim == nil || (state != "complete" && state != "conflict") {
		return ErrCalendarEventChanged
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := calendarReplyFollowupGenerationTx(ctx, tx, claim); err != nil {
		return err
	}
	if err := validateUserCalendarReplyProofTx(ctx, tx, claim.job, claim.response, claim.source, nil, claim.send, claim.sendState, guard); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE calendar_reply_jobs SET state=? WHERE id=? AND user_id=? AND state='pending' AND attempted_at=?`, state, claim.job.ID, claim.job.UserID, claim.at)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrCalendarEventChanged
	}
	expected := claim.job
	expected.State = state
	if err := calendarReplyFollowupGenerationTx(ctx, tx, claim); err != nil {
		return err
	}
	if err := validateUserCalendarReplyProofTx(ctx, tx, expected, claim.response, claim.source, nil, claim.send, claim.sendState, guard); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) ListAccountCalendarReplyFollowups(ctx context.Context, owner, account string, limit int) ([]CalendarReplyJob, error) {
	if owner == "" || account == "" {
		return nil, ErrAccountRoute
	}
	if limit <= 0 || limit > 20 {
		limit = 20
	}
	rows, err := db.Read().QueryContext(ctx, calendarReplySelect+`WHERE j.user_id=? AND s.account_id=? AND j.state='pending' AND s.status='sent'
 AND EXISTS(SELECT 1 FROM accounts a WHERE a.id=s.account_id AND a.user_id=j.user_id AND COALESCE(a.is_deleting,0)=0)
 ORDER BY j.attempted_at,j.created_at LIMIT ?`, owner, account, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []CalendarReplyJob
	for rows.Next() {
		job, err := scanCalendarReply(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// A purely local conflict notice stops Calendar retries for a known accepted
// send. It does not release a reservation or grant authority on a new source.
func calendarReplySentRecordTx(ctx context.Context, tx *sql.Tx, job CalendarReplyJob, send OutgoingSend, guard func(*sql.Tx, string) error) error {
	if guard == nil || job.UserID == "" || job.ID != send.ID || job.SendStatus != OutgoingSendSent || send.Status != OutgoingSendSent || send.Transport != OutgoingTransportSMTP {
		return ErrCalendarEventChanged
	}
	if err := guard(tx, send.AccountID); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM accounts a JOIN users u ON u.id=a.user_id WHERE a.id=? AND a.user_id=? AND COALESCE(a.is_deleting,0)=0 AND u.status='active' AND u.deletion_pending=0`, send.AccountID, job.UserID).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return ErrAccountRoute
	}
	actual, err := scanCalendarReply(tx.QueryRowContext(ctx, calendarReplySelect+`WHERE j.id=? AND j.user_id=?`, job.ID, job.UserID))
	if err != nil {
		return err
	}
	if actual != job {
		return ErrCalendarEventChanged
	}
	current, err := scanOutgoingSend(tx.QueryRowContext(ctx, outgoingSendSelect+` WHERE id=?`, send.ID))
	if err != nil {
		return err
	}
	if current.AccountID != send.AccountID || current.Transport != send.Transport || current.Status != send.Status || current.AttemptCount != send.AttemptCount || current.SentMessageID != send.SentMessageID {
		return ErrCalendarEventChanged
	}
	return guard(tx, send.AccountID)
}
func (db *DB) ValidateUserCalendarReplySentRecord(ctx context.Context, job CalendarReplyJob, send OutgoingSend, guard func(*sql.Tx, string) error) error {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := calendarReplySentRecordTx(ctx, tx, job, send, guard); err != nil {
		return err
	}
	return tx.Commit()
}
func (db *DB) MarkUserCalendarReplyConflict(ctx context.Context, job CalendarReplyJob, send OutgoingSend, guard func(*sql.Tx, string) error) error {
	if job.State != "pending" {
		return ErrCalendarEventChanged
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := calendarReplySentRecordTx(ctx, tx, job, send, guard); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE calendar_reply_jobs SET state='conflict' WHERE id=? AND user_id=? AND state='pending'`, job.ID, job.UserID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrCalendarEventChanged
	}
	job.State = "conflict"
	if err := calendarReplySentRecordTx(ctx, tx, job, send, guard); err != nil {
		return err
	}
	return tx.Commit()
}
