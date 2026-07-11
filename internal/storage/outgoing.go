package storage

import (
	"context"
	"database/sql"
	"encoding/json"
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
)

const (
	OutgoingTransportSMTP    = "smtp"
	OutgoingTransportGmail   = "gmail"
	OutgoingTransportOutlook = "outlook"
)

type OutgoingSend struct {
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
	Status             string
	AttemptCount       int
	LastError          string
	SentMessageID      string
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
			envelope_recipients, mime_data, message_json, send_after, is_scheduled,
			status, attempt_count, last_error, locked_at, sent_message_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', NULL, '')
		ON CONFLICT(message_id) DO UPDATE SET
			account_id = excluded.account_id,
			draft_id = excluded.draft_id,
			transport = excluded.transport,
			envelope_from = excluded.envelope_from,
			envelope_recipients = excluded.envelope_recipients,
			mime_data = excluded.mime_data,
			message_json = excluded.message_json,
			send_after = excluded.send_after,
			is_scheduled = excluded.is_scheduled,
			status = excluded.status,
			attempt_count = 0,
			last_error = '',
			locked_at = NULL,
			sent_message_id = '',
			updated_at = CURRENT_TIMESTAMP
		WHERE outgoing_sends.status != ?`,
		input.ID, input.AccountID, messageID, input.DraftID, input.Transport, input.EnvelopeFrom,
		string(recipientsJSON), input.MIMEData, string(input.MessageJSON), input.SendAfter, input.IsScheduled,
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
		WHERE status = ? AND send_after <= ? AND mime_data IS NOT NULL AND length(mime_data) > 0
		ORDER BY send_after ASC, created_at ASC
		LIMIT ?`, OutgoingSendPending, now, limit)
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

func (db *DB) CompleteOutgoingSend(ctx context.Context, id, sentMessageID string) error {
	result, err := db.Write().ExecContext(ctx, `
		UPDATE outgoing_sends
		SET status = ?, sent_message_id = ?, last_error = '', locked_at = NULL,
			envelope_recipients = '[]', mime_data = NULL, message_json = '', updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ?`, OutgoingSendSent, sentMessageID, id, OutgoingSendSending)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("outgoing send %s is no longer marked sending", id)
	}
	return nil
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

const outgoingSendSelect = `SELECT id, account_id, message_id, draft_id, transport, envelope_from,
	envelope_recipients, mime_data, message_json, send_after, is_scheduled,
	status, attempt_count, last_error, sent_message_id
	FROM outgoing_sends`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanOutgoingSend(row rowScanner) (OutgoingSend, error) {
	var send OutgoingSend
	var messageID sql.NullInt64
	var recipientsJSON string
	var sendAfter sqliteNullTime
	var scheduled int
	if err := row.Scan(
		&send.ID, &send.AccountID, &messageID, &send.DraftID, &send.Transport, &send.EnvelopeFrom,
		&recipientsJSON, &send.MIMEData, &send.MessageJSON, &sendAfter, &scheduled,
		&send.Status, &send.AttemptCount, &send.LastError, &send.SentMessageID,
	); err != nil {
		return OutgoingSend{}, err
	}
	if messageID.Valid {
		send.MessageID = messageID.Int64
	}
	if sendAfter.Valid {
		send.SendAfter = sendAfter.Time
	}
	send.IsScheduled = scheduled != 0
	if recipientsJSON != "" {
		if err := json.Unmarshal([]byte(recipientsJSON), &send.EnvelopeRecipients); err != nil {
			return OutgoingSend{}, fmt.Errorf("decode outgoing recipients: %w", err)
		}
	}
	return send, nil
}
