package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const (
	ScheduledSendPending   = "pending"
	ScheduledSendSending   = "sending"
	ScheduledSendSent      = "sent"
	ScheduledSendFailed    = "failed"
	ScheduledSendAmbiguous = "ambiguous"
)

type ScheduledSend struct {
	ID            string
	AccountID     string
	MessageID     int64
	ScheduledFor  time.Time
	Status        string
	AttemptCount  int
	LastError     string
	SentMessageID string
}

func (db *DB) UpsertScheduledSend(ctx context.Context, accountID string, messageID int64, scheduledFor time.Time) (ScheduledSend, error) {
	if accountID == "" || messageID == 0 || scheduledFor.IsZero() {
		return ScheduledSend{}, fmt.Errorf("missing scheduled send identity")
	}
	id := uuid.NewString()
	scheduledFor = scheduledFor.UTC()
	_, err := db.Write().ExecContext(ctx, `
		INSERT INTO scheduled_sends (id, account_id, message_id, scheduled_for, status, attempt_count, last_error, locked_at, sent_message_id)
		VALUES (?, ?, ?, ?, ?, 0, '', NULL, '')
		ON CONFLICT(message_id) DO UPDATE SET
			account_id = excluded.account_id,
			scheduled_for = excluded.scheduled_for,
			status = excluded.status,
			attempt_count = 0,
			last_error = '',
			locked_at = NULL,
			sent_message_id = '',
			updated_at = CURRENT_TIMESTAMP`, id, accountID, messageID, scheduledFor, ScheduledSendPending)
	if err != nil {
		return ScheduledSend{}, fmt.Errorf("upsert scheduled send: %w", err)
	}
	return db.GetScheduledSendByMessageID(ctx, messageID)
}

func (db *DB) GetScheduledSendByMessageID(ctx context.Context, messageID int64) (ScheduledSend, error) {
	var s ScheduledSend
	var scheduled sqliteNullTime
	err := db.Read().QueryRowContext(ctx, `
		SELECT id, account_id, message_id, scheduled_for, status, attempt_count, last_error, sent_message_id
		FROM scheduled_sends
		WHERE message_id = ?`, messageID).Scan(&s.ID, &s.AccountID, &s.MessageID, &scheduled, &s.Status, &s.AttemptCount, &s.LastError, &s.SentMessageID)
	if err != nil {
		return ScheduledSend{}, err
	}
	if scheduled.Valid {
		s.ScheduledFor = scheduled.Time
	}
	return s, nil
}

func (db *DB) ClaimDueScheduledSends(ctx context.Context, now time.Time, limit int) ([]ScheduledSend, error) {
	if limit <= 0 {
		limit = 10
	}
	now = now.UTC()
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, account_id, message_id, scheduled_for, status, attempt_count, last_error, sent_message_id
		FROM scheduled_sends
		WHERE status = ? AND scheduled_for <= ?
		ORDER BY scheduled_for ASC
		LIMIT ?`, ScheduledSendPending, now, limit)
	if err != nil {
		return nil, fmt.Errorf("select due scheduled sends: %w", err)
	}
	defer rows.Close()

	var sends []ScheduledSend
	for rows.Next() {
		var s ScheduledSend
		var scheduled sqliteNullTime
		if err := rows.Scan(&s.ID, &s.AccountID, &s.MessageID, &scheduled, &s.Status, &s.AttemptCount, &s.LastError, &s.SentMessageID); err != nil {
			return nil, err
		}
		if scheduled.Valid {
			s.ScheduledFor = scheduled.Time
		}
		sends = append(sends, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	for _, s := range sends {
		if _, err := tx.ExecContext(ctx, `
			UPDATE scheduled_sends
			SET status = ?, locked_at = ?, attempt_count = attempt_count + 1, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND status = ?`, ScheduledSendSending, now, s.ID, ScheduledSendPending); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return sends, nil
}

func (db *DB) CompleteScheduledSend(ctx context.Context, id string, sentMessageID string) error {
	_, err := db.Write().ExecContext(ctx, `
		UPDATE scheduled_sends
		SET status = ?, sent_message_id = ?, last_error = '', locked_at = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`, ScheduledSendSent, sentMessageID, id)
	return err
}

func (db *DB) FinishScheduledSendWithError(ctx context.Context, id string, status string, errText string) error {
	if status == "" {
		status = ScheduledSendFailed
	}
	_, err := db.Write().ExecContext(ctx, `
		UPDATE scheduled_sends
		SET status = ?, last_error = ?, locked_at = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`, status, errText, id)
	return err
}

func (db *DB) ResetStaleScheduledSends(ctx context.Context, before time.Time) error {
	_, err := db.Write().ExecContext(ctx, `
		UPDATE scheduled_sends
		SET status = ?, locked_at = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE status = ? AND locked_at IS NOT NULL AND locked_at < ?`, ScheduledSendPending, ScheduledSendSending, before.UTC())
	return err
}

func (db *DB) ScheduledSendForMessage(ctx context.Context, messageID int64) (*ScheduledSend, error) {
	s, err := db.GetScheduledSendByMessageID(ctx, messageID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}
