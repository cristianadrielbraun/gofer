package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Organizer notifications have no response reservation: they are queued before
// the DAV mutation and may outlive the cached event, including after deletion.
// The immutable outgoing row supplies the attempt identity instead.
func (db *DB) QueueUserCalendarNotification(ctx context.Context, source *UserCalendarSourceSnapshot, input QueueOutgoingSendInput, guard func(*sql.Tx, string) error, event ...*UserCalendarEventSnapshot) (OutgoingSend, error) {
	if len(event) > 1 || (len(event) == 1 && (event[0] == nil || source == nil || event[0].source.ID != source.source.ID || event[0].source.UserID != source.source.UserID || event[0].source.AccountID != source.source.AccountID)) {
		return OutgoingSend{}, ErrCalendarSourceChanged
	}
	if source == nil || source.source.Provider != CalendarSourceProviderCalDAV || guard == nil || input.ID == "" || input.AccountID != source.source.AccountID || input.Transport != OutgoingTransportSMTP || input.EnvelopeFrom == "" || len(input.EnvelopeRecipients) == 0 || len(input.MIMEData) == 0 || !json.Valid(input.MessageJSON) || input.MessageID != 0 || input.DraftID != "" || input.IsScheduled || !input.SendAfter.IsZero() {
		return OutgoingSend{}, ErrCalendarSourceChanged
	}
	input.MIMEData = append([]byte(nil), input.MIMEData...)
	input.MessageJSON = append([]byte(nil), input.MessageJSON...)
	input.EnvelopeRecipients = append([]string(nil), input.EnvelopeRecipients...)
	event = append([]*UserCalendarEventSnapshot(nil), event...)
	recipients, err := json.Marshal(input.EnvelopeRecipients)
	if err != nil {
		return OutgoingSend{}, err
	}
	expected := OutgoingSend{ID: input.ID, AccountID: input.AccountID, Transport: input.Transport, EnvelopeFrom: input.EnvelopeFrom, EnvelopeRecipients: input.EnvelopeRecipients, MIMEData: input.MIMEData, MessageJSON: input.MessageJSON, Status: OutgoingSendPending}
	state, err := CalendarReplySendFingerprint(expected)
	if err != nil {
		return OutgoingSend{}, err
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return OutgoingSend{}, err
	}
	defer tx.Rollback()
	validate := func() error {
		if err := validateUserCalendarSourceTx(ctx, tx, source, guard); err != nil {
			return err
		}
		if len(event) == 1 {
			return validateUserCalendarEventTx(ctx, tx, event[0], guard)
		}
		return nil
	}
	if err := validate(); err != nil {
		return OutgoingSend{}, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO outgoing_sends(id,account_id,transport,envelope_from,envelope_recipients,mime_data,message_json,send_after,next_attempt_at,status) VALUES(?,?,?,?,?,?,?,?,?,'pending')`, input.ID, input.AccountID, input.Transport, input.EnvelopeFrom, string(recipients), input.MIMEData, string(input.MessageJSON), now, now); err != nil {
		return OutgoingSend{}, err
	}
	actual, err := validateUserCalendarNotificationAttemptTx(ctx, tx, source.source.UserID, expected, state, guard)
	if err != nil {
		return OutgoingSend{}, err
	}
	var unlocked bool
	if err := tx.QueryRowContext(ctx, `SELECT locked_at IS NULL FROM outgoing_sends WHERE id=?`, input.ID).Scan(&unlocked); err != nil {
		return OutgoingSend{}, err
	}
	if !unlocked || actual.SentCopyStatus != SentCopyNotRequired || actual.SentCopyAttempts != 0 || actual.SentCopyLastError != "" || actual.SentCopyUID != 0 || actual.SentCopyUIDValidity != 0 || actual.LastError != "" || actual.SentMessageID != "" || !actual.SendAfter.Equal(now) || !actual.NextAttemptAt.Equal(now) {
		return OutgoingSend{}, ErrCalendarSourceChanged
	}
	if err := validate(); err != nil {
		return OutgoingSend{}, err
	}
	if err := tx.Commit(); err != nil {
		return OutgoingSend{}, err
	}
	return actual, nil
}

func validateUserCalendarNotificationAttemptTx(ctx context.Context, tx *sql.Tx, owner string, expected OutgoingSend, state [32]byte, guard func(*sql.Tx, string) error) (OutgoingSend, error) {
	if guard == nil || owner == "" || expected.ID == "" || expected.AccountID == "" || expected.Transport != OutgoingTransportSMTP || expected.MessageID != 0 || expected.DraftID != "" || expected.IsScheduled {
		return OutgoingSend{}, ErrCalendarSourceChanged
	}
	if err := guard(tx, expected.AccountID); err != nil {
		return OutgoingSend{}, err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM accounts a JOIN users u ON u.id=a.user_id WHERE a.id=? AND a.user_id=? AND COALESCE(a.is_deleting,0)=0 AND u.status='active' AND u.deletion_pending=0`, expected.AccountID, owner).Scan(&n); err != nil {
		return OutgoingSend{}, err
	}
	if n != 1 {
		return OutgoingSend{}, ErrAccountRoute
	}
	actual, err := scanOutgoingSend(tx.QueryRowContext(ctx, outgoingSendSelect+` WHERE id=?`, expected.ID))
	if err != nil {
		return OutgoingSend{}, err
	}
	actualState, err := CalendarReplySendFingerprint(actual)
	if err != nil {
		return OutgoingSend{}, err
	}
	if actual.Status != expected.Status || actual.AttemptCount != expected.AttemptCount || actual.MessageID != 0 || actual.DraftID != "" || actual.IsScheduled || actualState != state {
		return OutgoingSend{}, ErrCalendarSourceChanged
	}
	if err := guard(tx, expected.AccountID); err != nil {
		return OutgoingSend{}, err
	}
	return actual, nil
}

func (db *DB) ValidateUserCalendarNotificationAttempt(ctx context.Context, owner string, send OutgoingSend, state [32]byte, guard func(*sql.Tx, string) error) error {
	if send.Status != OutgoingSendSending {
		return ErrCalendarSourceChanged
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := validateUserCalendarNotificationAttemptTx(ctx, tx, owner, send, state, guard); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) ValidateUserCalendarNotificationSource(ctx context.Context, source *UserCalendarSourceSnapshot, send OutgoingSend, state [32]byte, guard func(*sql.Tx, string) error) error {
	if source == nil || source.source.Provider != CalendarSourceProviderCalDAV || send.Status != OutgoingSendSending {
		return ErrCalendarSourceChanged
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateUserCalendarSourceTx(ctx, tx, source, guard); err != nil {
		return err
	}
	if _, err := validateUserCalendarNotificationAttemptTx(ctx, tx, source.source.UserID, send, state, guard); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) RetryUserCalendarNotification(ctx context.Context, source *UserCalendarSourceSnapshot, send OutgoingSend, state [32]byte, messageJSON []byte, confirmAmbiguous bool, guard func(*sql.Tx, string) error, event ...*UserCalendarEventSnapshot) (OutgoingSend, error) {
	if len(event) > 1 || (len(event) == 1 && (event[0] == nil || source == nil || event[0].source.ID != source.source.ID || event[0].source.UserID != source.source.UserID || event[0].source.AccountID != source.source.AccountID)) {
		return OutgoingSend{}, ErrCalendarEventChanged
	}
	if source == nil || source.source.Provider != CalendarSourceProviderCalDAV || send.AccountID != source.source.AccountID || !json.Valid(messageJSON) {
		return OutgoingSend{}, ErrCalendarSourceChanged
	}
	switch send.Status {
	case OutgoingSendFailed:
	case OutgoingSendAmbiguous:
		if !confirmAmbiguous {
			return OutgoingSend{}, ErrOutgoingSendAmbiguousConfirmation
		}
	default:
		return OutgoingSend{}, ErrOutgoingSendNotRetryable
	}
	messageJSON = append([]byte(nil), messageJSON...)
	event = append([]*UserCalendarEventSnapshot(nil), event...)
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return OutgoingSend{}, err
	}
	defer tx.Rollback()
	validate := func() error {
		if err := validateUserCalendarSourceTx(ctx, tx, source, guard); err != nil {
			return err
		}
		if len(event) == 1 {
			return validateUserCalendarEventTx(ctx, tx, event[0], guard)
		}
		return nil
	}
	if err := validate(); err != nil {
		return OutgoingSend{}, err
	}
	if _, err := validateUserCalendarNotificationAttemptTx(ctx, tx, source.source.UserID, send, state, guard); err != nil {
		return OutgoingSend{}, err
	}
	now := time.Now().UTC()
	changed, err := tx.ExecContext(ctx, `UPDATE outgoing_sends SET status='pending',last_error='',locked_at=NULL,next_attempt_at=?,message_json=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status=? AND attempt_count=?`, now, string(messageJSON), send.ID, send.Status, send.AttemptCount)
	if err != nil {
		return OutgoingSend{}, err
	}
	if n, err := changed.RowsAffected(); err != nil || n != 1 {
		return OutgoingSend{}, ErrCalendarSourceChanged
	}
	send.Status, send.MessageJSON = OutgoingSendPending, messageJSON
	state, err = CalendarReplySendFingerprint(send)
	if err != nil {
		return OutgoingSend{}, err
	}
	actual, err := validateUserCalendarNotificationAttemptTx(ctx, tx, source.source.UserID, send, state, guard)
	if err != nil {
		return OutgoingSend{}, err
	}
	var unlocked bool
	if err := tx.QueryRowContext(ctx, `SELECT locked_at IS NULL FROM outgoing_sends WHERE id=?`, send.ID).Scan(&unlocked); err != nil {
		return OutgoingSend{}, err
	}
	if !unlocked || actual.LastError != "" || !actual.NextAttemptAt.Equal(now) {
		return OutgoingSend{}, ErrCalendarSourceChanged
	}
	if err := validate(); err != nil {
		return OutgoingSend{}, err
	}
	if err := tx.Commit(); err != nil {
		return OutgoingSend{}, err
	}
	return actual, nil
}

// Only lifecycle and the original claimed bytes gate result publication. A
// changed/deleted event or repaired SMTP configuration cannot erase acceptance.
func (db *DB) FinishUserCalendarNotificationSend(ctx context.Context, owner string, send OutgoingSend, state [32]byte, result CalendarReplySendResult, guard func(*sql.Tx, string) error) error {
	if send.Status != OutgoingSendSending {
		return ErrCalendarSourceChanged
	}
	switch result.Status {
	case OutgoingSendSent:
		if result.InternetID == "" || result.Error != "" {
			return ErrCalendarSourceChanged
		}
	case OutgoingSendPending:
		if result.NextAttemptAt.IsZero() || result.InternetID != "" {
			return ErrCalendarSourceChanged
		}
	case OutgoingSendFailed, OutgoingSendAmbiguous:
		if result.InternetID != "" {
			return ErrCalendarSourceChanged
		}
	default:
		return ErrCalendarSourceChanged
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := validateUserCalendarNotificationAttemptTx(ctx, tx, owner, send, state, guard); err != nil {
		return err
	}
	next, copyStatus := send.NextAttemptAt, send.SentCopyStatus
	if result.Status == OutgoingSendPending {
		next = result.NextAttemptAt.UTC()
	}
	if result.Status == OutgoingSendSent {
		copyStatus = SentCopyPending
	}
	changed, err := tx.ExecContext(ctx, `UPDATE outgoing_sends SET status=?,last_error=?,locked_at=NULL,sent_message_id=?,next_attempt_at=?,sent_copy_status=?,sent_copy_attempt_count=0,sent_copy_last_error='',sent_copy_locked_at=NULL,sent_copy_next_attempt_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status='sending' AND attempt_count=?`, result.Status, result.Error, result.InternetID, next, copyStatus, send.ID, send.AttemptCount)
	if err != nil {
		return err
	}
	if n, err := changed.RowsAffected(); err != nil || n != 1 {
		return ErrCalendarSourceChanged
	}
	send.Status = result.Status
	actual, err := validateUserCalendarNotificationAttemptTx(ctx, tx, owner, send, state, guard)
	if err != nil {
		return err
	}
	var unlocked bool
	if err := tx.QueryRowContext(ctx, `SELECT locked_at IS NULL AND sent_copy_locked_at IS NULL FROM outgoing_sends WHERE id=?`, send.ID).Scan(&unlocked); err != nil {
		return err
	}
	if !unlocked || actual.LastError != result.Error || actual.SentMessageID != result.InternetID || !actual.NextAttemptAt.Equal(next) || actual.SentCopyStatus != copyStatus || actual.SentCopyAttempts != 0 || actual.SentCopyLastError != "" {
		return ErrCalendarSourceChanged
	}
	return tx.Commit()
}
