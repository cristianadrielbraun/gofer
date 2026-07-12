package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const (
	OutgoingSendPending   = "pending"
	OutgoingSendSending   = "sending"
	OutgoingSendSent      = "sent"
	OutgoingSendFailed    = "failed"
	OutgoingSendAmbiguous = "ambiguous"
	OutgoingSendCanceled  = "canceled"
)

var (
	ErrOutgoingSendAmbiguousConfirmation = errors.New("retrying this send requires explicit confirmation because delivery is ambiguous")
	ErrOutgoingSendNotRetryable          = errors.New("outgoing send is not retryable in its current state")
	ErrOutgoingSendNotCancelable         = errors.New("outgoing send is not cancelable in its current state")
)

const (
	SentCopyNotRequired = "not_required"
	SentCopyPending     = "pending"
	SentCopyCopying     = "copying"
	SentCopyComplete    = "complete"
	SentCopyFailed      = "failed"
	SentCopyAmbiguous   = "ambiguous"
)

const (
	OutgoingTransportSMTP    = "smtp"
	OutgoingTransportGmail   = "gmail"
	OutgoingTransportOutlook = "outlook"
)

type OutgoingSend struct {
	ID                  string
	AccountID           string
	MessageID           int64
	DraftID             string
	Transport           string
	EnvelopeFrom        string
	EnvelopeRecipients  []string
	MIMEData            []byte
	MessageJSON         []byte
	SendAfter           time.Time
	NextAttemptAt       time.Time
	IsScheduled         bool
	Status              string
	AttemptCount        int
	LastError           string
	SentMessageID       string
	SentCopyStatus      string
	SentCopyAttempts    int
	SentCopyLastError   string
	SentCopyNextTry     time.Time
	SentCopyUID         uint32
	SentCopyUIDValidity uint32
}

// OutgoingSendSummary is the user-safe subset of an outbox row. It deliberately
// excludes MIME bytes, recipient envelopes, and provider credentials.
type OutgoingSendSummary struct {
	ID                  string
	AccountID           string
	MessageID           int64
	DraftID             string
	Transport           string
	SendAfter           time.Time
	NextAttemptAt       time.Time
	IsScheduled         bool
	Status              string
	AttemptCount        int
	LastError           string
	SentCopyStatus      string
	SentCopyAttempts    int
	SentCopyLastError   string
	SentCopyNextTry     time.Time
	SentCopyUID         uint32
	SentCopyUIDValidity uint32
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type QueueOutgoingSendInput struct {
	ID                 string
	AccountID          string
	MessageID          int64
	DraftID            string
	Transport          string
	EnvelopeFrom       string
	EnvelopeRecipients []string
	MIMEData           []byte
	MessageJSON        []byte
	SendAfter          time.Time
	IsScheduled        bool
}

func (db *DB) QueueOutgoingSend(ctx context.Context, input QueueOutgoingSendInput) (OutgoingSend, error) {
	if input.AccountID == "" || input.Transport == "" || input.EnvelopeFrom == "" || len(input.EnvelopeRecipients) == 0 || len(input.MIMEData) == 0 || len(input.MessageJSON) == 0 {
		return OutgoingSend{}, fmt.Errorf("missing outgoing send payload")
	}
	if input.ID == "" {
		input.ID = uuid.NewString()
	}
	if input.SendAfter.IsZero() {
		input.SendAfter = time.Now().UTC()
	} else {
		input.SendAfter = input.SendAfter.UTC()
	}
	recipientsJSON, err := json.Marshal(input.EnvelopeRecipients)
	if err != nil {
		return OutgoingSend{}, fmt.Errorf("encode outgoing recipients: %w", err)
	}
	var messageID any
	if input.MessageID > 0 {
		messageID = input.MessageID
	}
	result, err := db.Write().ExecContext(ctx, `
		INSERT INTO outgoing_sends (
			id, account_id, message_id, draft_id, transport, envelope_from,
			envelope_recipients, mime_data, message_json, send_after, next_attempt_at, is_scheduled,
			status, attempt_count, last_error, locked_at, sent_message_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', NULL, '')
		ON CONFLICT(message_id) DO UPDATE SET
			account_id = excluded.account_id,
			draft_id = excluded.draft_id,
			transport = excluded.transport,
			envelope_from = excluded.envelope_from,
			envelope_recipients = excluded.envelope_recipients,
			mime_data = excluded.mime_data,
			message_json = excluded.message_json,
			send_after = excluded.send_after,
			next_attempt_at = excluded.next_attempt_at,
			is_scheduled = excluded.is_scheduled,
			status = excluded.status,
			attempt_count = 0,
			last_error = '',
			locked_at = NULL,
			sent_message_id = '',
			sent_copy_status = 'not_required',
			sent_copy_attempt_count = 0,
			sent_copy_last_error = '',
			sent_copy_locked_at = NULL,
			sent_copy_next_attempt_at = CURRENT_TIMESTAMP,
			sent_copy_uid = 0,
			sent_copy_uid_validity = 0,
			updated_at = CURRENT_TIMESTAMP
		WHERE outgoing_sends.status != ?`,
		input.ID, input.AccountID, messageID, input.DraftID, input.Transport, input.EnvelopeFrom,
		string(recipientsJSON), input.MIMEData, string(input.MessageJSON), input.SendAfter, time.Now().UTC(), input.IsScheduled,
		OutgoingSendPending, OutgoingSendSending)
	if err != nil {
		return OutgoingSend{}, fmt.Errorf("queue outgoing send: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return OutgoingSend{}, fmt.Errorf("message is already being sent")
	}
	if input.MessageID > 0 {
		return db.GetOutgoingSendByMessageID(ctx, input.MessageID)
	}
	return db.GetOutgoingSend(ctx, input.ID)
}

func (db *DB) CancelOutgoingSendForMessage(ctx context.Context, messageID int64) error {
	if messageID <= 0 {
		return nil
	}
	_, err := db.Write().ExecContext(ctx, `
		DELETE FROM outgoing_sends
		WHERE message_id = ? AND status != ?`, messageID, OutgoingSendSending)
	return err
}

func (db *DB) RetryOutgoingSend(ctx context.Context, userID, id string, confirmAmbiguous bool) (OutgoingSend, error) {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return OutgoingSend{}, err
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRowContext(ctx, `
		SELECT os.status
		FROM outgoing_sends os
		JOIN accounts a ON a.id = os.account_id
		WHERE os.id = ? AND a.user_id = ?`, id, userID).Scan(&status); err != nil {
		return OutgoingSend{}, err
	}
	switch status {
	case OutgoingSendFailed:
	case OutgoingSendAmbiguous:
		if !confirmAmbiguous {
			return OutgoingSend{}, ErrOutgoingSendAmbiguousConfirmation
		}
	default:
		return OutgoingSend{}, ErrOutgoingSendNotRetryable
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE outgoing_sends
		SET status = ?, last_error = '', locked_at = NULL, next_attempt_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)`, OutgoingSendPending, id, userID); err != nil {
		return OutgoingSend{}, err
	}
	if err := tx.Commit(); err != nil {
		return OutgoingSend{}, err
	}
	return db.GetOutgoingSend(ctx, id)
}

func (db *DB) RetryOutgoingSendNow(ctx context.Context, userID, id string) (OutgoingSend, error) {
	result, err := db.Write().ExecContext(ctx, `
		UPDATE outgoing_sends
		SET send_after = CASE WHEN send_after > CURRENT_TIMESTAMP THEN CURRENT_TIMESTAMP ELSE send_after END,
			is_scheduled = 0, next_attempt_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ? AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)`, id, OutgoingSendPending, userID)
	if err != nil {
		return OutgoingSend{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		var status string
		if err := db.Read().QueryRowContext(ctx, `
			SELECT os.status
			FROM outgoing_sends os
			JOIN accounts a ON a.id = os.account_id
			WHERE os.id = ? AND a.user_id = ?`, id, userID).Scan(&status); err != nil {
			return OutgoingSend{}, err
		}
		return OutgoingSend{}, ErrOutgoingSendNotRetryable
	}
	return db.GetOutgoingSend(ctx, id)
}

func (db *DB) CancelOutgoingSend(ctx context.Context, userID, id string) (OutgoingSend, error) {
	result, err := db.Write().ExecContext(ctx, `
		UPDATE outgoing_sends
		SET status = ?, last_error = 'Canceled by the user', locked_at = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status IN (?, ?, ?) AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)`, OutgoingSendCanceled, id, OutgoingSendPending, OutgoingSendFailed, OutgoingSendAmbiguous, userID)
	if err != nil {
		return OutgoingSend{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		var status string
		if err := db.Read().QueryRowContext(ctx, `
			SELECT os.status
			FROM outgoing_sends os
			JOIN accounts a ON a.id = os.account_id
			WHERE os.id = ? AND a.user_id = ?`, id, userID).Scan(&status); err != nil {
			return OutgoingSend{}, err
		}
		return OutgoingSend{}, ErrOutgoingSendNotCancelable
	}
	return db.GetOutgoingSend(ctx, id)
}

func (db *DB) GetOutgoingSend(ctx context.Context, id string) (OutgoingSend, error) {
	return scanOutgoingSend(db.Read().QueryRowContext(ctx, outgoingSendSelect+` WHERE id = ?`, id))
}

func (db *DB) GetOutgoingSendByMessageID(ctx context.Context, messageID int64) (OutgoingSend, error) {
	return scanOutgoingSend(db.Read().QueryRowContext(ctx, outgoingSendSelect+` WHERE message_id = ?`, messageID))
}

func (db *DB) OutgoingSendForMessage(ctx context.Context, messageID int64) (*OutgoingSend, error) {
	send, err := db.GetOutgoingSendByMessageID(ctx, messageID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &send, nil
}

func (db *DB) ClaimDueOutgoingSends(ctx context.Context, now time.Time, limit int) ([]OutgoingSend, error) {
	if limit <= 0 {
		limit = 10
	}
	now = now.UTC()
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin outgoing claim: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, outgoingSendSelect+`
		WHERE status = ? AND send_after <= ? AND next_attempt_at <= ? AND mime_data IS NOT NULL AND length(mime_data) > 0
		ORDER BY next_attempt_at ASC, send_after ASC, created_at ASC
		LIMIT ?`, OutgoingSendPending, now, now, limit)
	if err != nil {
		return nil, fmt.Errorf("select due outgoing sends: %w", err)
	}
	var sends []OutgoingSend
	for rows.Next() {
		send, err := scanOutgoingSend(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		sends = append(sends, send)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range sends {
		result, err := tx.ExecContext(ctx, `
			UPDATE outgoing_sends
			SET status = ?, locked_at = ?, attempt_count = attempt_count + 1, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND status = ?`, OutgoingSendSending, now, sends[i].ID, OutgoingSendPending)
		if err != nil {
			return nil, err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return nil, fmt.Errorf("outgoing send %s was claimed concurrently", sends[i].ID)
		}
		sends[i].Status = OutgoingSendSending
		sends[i].AttemptCount++
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return sends, nil
}

func (db *DB) CompleteOutgoingSend(ctx context.Context, id, sentMessageID string, needsSentCopy bool) error {
	query := `UPDATE outgoing_sends
		SET status = ?, sent_message_id = ?, last_error = '', locked_at = NULL,
			sent_copy_status = ?, sent_copy_attempt_count = 0, sent_copy_last_error = '',
			sent_copy_locked_at = NULL, sent_copy_next_attempt_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ?`
	sentCopyStatus := SentCopyPending
	if !needsSentCopy {
		sentCopyStatus = SentCopyNotRequired
		query = `UPDATE outgoing_sends
			SET status = ?, sent_message_id = ?, last_error = '', locked_at = NULL,
				sent_copy_status = ?, sent_copy_attempt_count = 0, sent_copy_last_error = '',
				sent_copy_locked_at = NULL, sent_copy_next_attempt_at = CURRENT_TIMESTAMP,
				envelope_recipients = '[]', mime_data = NULL, message_json = '', updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND status = ?`
	}
	result, err := db.Write().ExecContext(ctx, query, OutgoingSendSent, sentMessageID, sentCopyStatus, id, OutgoingSendSending)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("outgoing send %s is no longer marked sending", id)
	}
	return nil
}

func (db *DB) ClaimDueSentCopies(ctx context.Context, now time.Time, limit int) ([]OutgoingSend, error) {
	if limit <= 0 {
		limit = 10
	}
	now = now.UTC()
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin sent copy claim: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, outgoingSendSelect+`
		WHERE status = ?
		  AND sent_copy_status IN (?, ?, ?)
		  AND sent_copy_next_attempt_at <= ?
		  AND mime_data IS NOT NULL AND length(mime_data) > 0
		ORDER BY sent_copy_next_attempt_at ASC, updated_at ASC
		LIMIT ?`, OutgoingSendSent, SentCopyPending, SentCopyFailed, SentCopyAmbiguous, now, limit)
	if err != nil {
		return nil, fmt.Errorf("select due sent copies: %w", err)
	}
	var sends []OutgoingSend
	for rows.Next() {
		send, err := scanOutgoingSend(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		sends = append(sends, send)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range sends {
		result, err := tx.ExecContext(ctx, `
			UPDATE outgoing_sends
			SET sent_copy_status = ?, sent_copy_locked_at = ?,
				sent_copy_attempt_count = sent_copy_attempt_count + 1, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND sent_copy_status = ?`, SentCopyCopying, now, sends[i].ID, sends[i].SentCopyStatus)
		if err != nil {
			return nil, err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return nil, fmt.Errorf("sent copy %s was claimed concurrently", sends[i].ID)
		}
		sends[i].SentCopyAttempts++
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return sends, nil
}

func (db *DB) CompleteSentCopy(ctx context.Context, id string, uid, uidValidity uint32) error {
	result, err := db.Write().ExecContext(ctx, `
		UPDATE outgoing_sends
		SET sent_copy_status = ?, sent_copy_last_error = '', sent_copy_locked_at = NULL,
			sent_copy_uid = ?, sent_copy_uid_validity = ?,
			envelope_recipients = '[]', mime_data = NULL, message_json = '', updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ? AND sent_copy_status = ?`,
		SentCopyComplete, uid, uidValidity, id, OutgoingSendSent, SentCopyCopying)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("sent copy %s is no longer being copied", id)
	}
	return nil
}

func (db *DB) FinishSentCopyWithError(ctx context.Context, id, status, errText string, nextAttempt time.Time) error {
	if status != SentCopyAmbiguous {
		status = SentCopyFailed
	}
	_, err := db.Write().ExecContext(ctx, `
		UPDATE outgoing_sends
		SET sent_copy_status = ?, sent_copy_last_error = ?, sent_copy_locked_at = NULL,
			sent_copy_next_attempt_at = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ? AND sent_copy_status = ?`,
		status, errText, nextAttempt.UTC(), id, OutgoingSendSent, SentCopyCopying)
	return err
}

func (db *DB) MarkInterruptedSentCopiesAmbiguous(ctx context.Context, reason string) (int64, error) {
	result, err := db.Write().ExecContext(ctx, `
		UPDATE outgoing_sends
		SET sent_copy_status = ?, sent_copy_last_error = ?, sent_copy_locked_at = NULL,
			sent_copy_next_attempt_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE status = ? AND sent_copy_status = ?`,
		SentCopyAmbiguous, reason, OutgoingSendSent, SentCopyCopying)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (db *DB) FinishOutgoingSendWithError(ctx context.Context, id, status, errText string) error {
	if status != OutgoingSendAmbiguous {
		status = OutgoingSendFailed
	}
	_, err := db.Write().ExecContext(ctx, `
		UPDATE outgoing_sends
		SET status = ?, last_error = ?, locked_at = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ?`, status, errText, id, OutgoingSendSending)
	return err
}

func (db *DB) FinishOutgoingSendWithRetry(ctx context.Context, id, errText string, nextAttempt time.Time) error {
	_, err := db.Write().ExecContext(ctx, `
		UPDATE outgoing_sends
		SET status = ?, last_error = ?, locked_at = NULL, next_attempt_at = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ?`, OutgoingSendPending, errText, nextAttempt.UTC(), id, OutgoingSendSending)
	return err
}

func (db *DB) MarkPendingOutgoingSendFailed(ctx context.Context, id, errText string) error {
	_, err := db.Write().ExecContext(ctx, `
		UPDATE outgoing_sends
		SET status = ?, last_error = ?, locked_at = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ?`, OutgoingSendFailed, errText, id, OutgoingSendPending)
	return err
}

func (db *DB) MarkInterruptedOutgoingSendsAmbiguous(ctx context.Context, reason string) (int64, error) {
	result, err := db.Write().ExecContext(ctx, `
		UPDATE outgoing_sends
		SET status = ?, last_error = ?, locked_at = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE status = ?`, OutgoingSendAmbiguous, reason, OutgoingSendSending)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (db *DB) ListUnpreparedOutgoingSends(ctx context.Context) ([]OutgoingSend, error) {
	rows, err := db.Read().QueryContext(ctx, outgoingSendSelect+`
		WHERE status = ? AND (mime_data IS NULL OR length(mime_data) = 0 OR message_json = '')
		ORDER BY send_after ASC`, OutgoingSendPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sends []OutgoingSend
	for rows.Next() {
		send, err := scanOutgoingSend(rows)
		if err != nil {
			return nil, err
		}
		sends = append(sends, send)
	}
	return sends, rows.Err()
}

func (db *DB) PrepareOutgoingSend(ctx context.Context, id, draftID, transport, envelopeFrom string, recipients []string, mimeData, messageJSON []byte) error {
	if id == "" || draftID == "" || transport == "" || envelopeFrom == "" || len(recipients) == 0 || len(mimeData) == 0 || len(messageJSON) == 0 {
		return fmt.Errorf("missing outgoing send payload")
	}
	recipientsJSON, err := json.Marshal(recipients)
	if err != nil {
		return err
	}
	_, err = db.Write().ExecContext(ctx, `
		UPDATE outgoing_sends
		SET draft_id = ?, transport = ?, envelope_from = ?, envelope_recipients = ?,
			mime_data = ?, message_json = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ?`, draftID, transport, envelopeFrom, string(recipientsJSON), mimeData, string(messageJSON), id, OutgoingSendPending)
	return err
}

func (db *DB) ListOutgoingSendSummariesForUser(ctx context.Context, userID string) ([]OutgoingSendSummary, error) {
	rows, err := db.Read().QueryContext(ctx, outgoingSendSummarySelect+`
		JOIN accounts a ON a.id = os.account_id
		WHERE a.user_id = ?
		  AND (os.status IN (?, ?, ?, ?)
		       OR (os.status = ? AND os.sent_copy_status IN (?, ?, ?, ?)))
		ORDER BY os.updated_at DESC
		LIMIT 100`, userID,
		OutgoingSendPending, OutgoingSendSending, OutgoingSendFailed, OutgoingSendAmbiguous,
		OutgoingSendSent, SentCopyPending, SentCopyCopying, SentCopyFailed, SentCopyAmbiguous)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var summaries []OutgoingSendSummary
	for rows.Next() {
		summary, err := scanOutgoingSendSummary(rows)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, summary)
	}
	return summaries, rows.Err()
}

func (db *DB) GetOutgoingSendSummaryForUser(ctx context.Context, userID, id string) (OutgoingSendSummary, error) {
	return scanOutgoingSendSummary(db.Read().QueryRowContext(ctx, outgoingSendSummarySelect+`
		JOIN accounts a ON a.id = os.account_id
		WHERE a.user_id = ? AND os.id = ?`, userID, id))
}

const outgoingSendSelect = `SELECT id, account_id, message_id, draft_id, transport, envelope_from,
	envelope_recipients, mime_data, message_json, send_after, next_attempt_at, is_scheduled,
	status, attempt_count, last_error, sent_message_id,
	sent_copy_status, sent_copy_attempt_count, sent_copy_last_error,
	sent_copy_next_attempt_at, sent_copy_uid, sent_copy_uid_validity
	FROM outgoing_sends`

const outgoingSendSummarySelect = `SELECT os.id, os.account_id, COALESCE(os.message_id, 0), os.draft_id, os.transport,
	os.send_after, os.next_attempt_at, os.is_scheduled, os.status, os.attempt_count, os.last_error,
	os.sent_copy_status, os.sent_copy_attempt_count, os.sent_copy_last_error, os.sent_copy_next_attempt_at,
	os.sent_copy_uid, os.sent_copy_uid_validity, os.created_at, os.updated_at
	FROM outgoing_sends os`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanOutgoingSend(row rowScanner) (OutgoingSend, error) {
	var send OutgoingSend
	var messageID sql.NullInt64
	var recipientsJSON string
	var sendAfter sqliteNullTime
	var nextAttemptAt sqliteNullTime
	var sentCopyNextTry sqliteNullTime
	var scheduled int
	var sentCopyUID, sentCopyUIDValidity int64
	if err := row.Scan(
		&send.ID, &send.AccountID, &messageID, &send.DraftID, &send.Transport, &send.EnvelopeFrom,
		&recipientsJSON, &send.MIMEData, &send.MessageJSON, &sendAfter, &nextAttemptAt, &scheduled,
		&send.Status, &send.AttemptCount, &send.LastError, &send.SentMessageID,
		&send.SentCopyStatus, &send.SentCopyAttempts, &send.SentCopyLastError,
		&sentCopyNextTry, &sentCopyUID, &sentCopyUIDValidity,
	); err != nil {
		return OutgoingSend{}, err
	}
	if messageID.Valid {
		send.MessageID = messageID.Int64
	}
	if sendAfter.Valid {
		send.SendAfter = sendAfter.Time
	}
	if nextAttemptAt.Valid {
		send.NextAttemptAt = nextAttemptAt.Time
	}
	send.IsScheduled = scheduled != 0
	if sentCopyNextTry.Valid {
		send.SentCopyNextTry = sentCopyNextTry.Time
	}
	if sentCopyUID > 0 {
		send.SentCopyUID = uint32(sentCopyUID)
	}
	if sentCopyUIDValidity > 0 {
		send.SentCopyUIDValidity = uint32(sentCopyUIDValidity)
	}
	if recipientsJSON != "" {
		if err := json.Unmarshal([]byte(recipientsJSON), &send.EnvelopeRecipients); err != nil {
			return OutgoingSend{}, fmt.Errorf("decode outgoing recipients: %w", err)
		}
	}
	return send, nil
}

func scanOutgoingSendSummary(row rowScanner) (OutgoingSendSummary, error) {
	var summary OutgoingSendSummary
	var sendAfter, nextAttempt, sentCopyNext, createdAt, updatedAt sqliteNullTime
	var scheduled int
	var sentCopyUID, sentCopyUIDValidity int64
	if err := row.Scan(
		&summary.ID, &summary.AccountID, &summary.MessageID, &summary.DraftID, &summary.Transport,
		&sendAfter, &nextAttempt, &scheduled, &summary.Status, &summary.AttemptCount, &summary.LastError,
		&summary.SentCopyStatus, &summary.SentCopyAttempts, &summary.SentCopyLastError, &sentCopyNext,
		&sentCopyUID, &sentCopyUIDValidity, &createdAt, &updatedAt,
	); err != nil {
		return OutgoingSendSummary{}, err
	}
	if sendAfter.Valid {
		summary.SendAfter = sendAfter.Time
	}
	if nextAttempt.Valid {
		summary.NextAttemptAt = nextAttempt.Time
	}
	if sentCopyNext.Valid {
		summary.SentCopyNextTry = sentCopyNext.Time
	}
	if createdAt.Valid {
		summary.CreatedAt = createdAt.Time
	}
	if updatedAt.Valid {
		summary.UpdatedAt = updatedAt.Time
	}
	summary.IsScheduled = scheduled != 0
	if sentCopyUID > 0 {
		summary.SentCopyUID = uint32(sentCopyUID)
	}
	if sentCopyUIDValidity > 0 {
		summary.SentCopyUIDValidity = uint32(sentCopyUIDValidity)
	}
	return summary, nil
}
