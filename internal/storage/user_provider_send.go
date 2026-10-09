package storage

import (
	"context"
	"database/sql"
	"errors"
)

// This extension belongs only to marked user stores. It keeps provider delivery
// acceptance separate from the retryable local Sent cache and its immutable MIME.
func ensureUserMailDeliverySchema(ctx context.Context, db *DB) error {
	var owner string
	if err := db.Read().QueryRowContext(ctx, `SELECT user_id FROM gofer_user_store WHERE singleton=1`).Scan(&owner); err != nil {
		return err
	}
	if err := ensureLayoutSchema(ctx, db, userLayoutSchema); err != nil {
		return err
	}
	db.userMailDelivery = true
	return nil
}

func checkUserSendAttempt(ctx context.Context, tx *sql.Tx, send OutgoingSend, subject string, copying bool) error {
	if send.Transport != OutgoingTransportGmail && send.Transport != OutgoingTransportOutlook {
		return errors.New("provider send transport is required")
	}
	var count int
	status, attempt, clause := OutgoingSendSending, send.AttemptCount, " AND o.attempt_count=?"
	if copying {
		status, attempt, clause = OutgoingSendSent, send.SentCopyAttempts, " AND o.sent_copy_status='copying' AND o.sent_copy_attempt_count=?"
	}
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM outgoing_sends o JOIN accounts a ON a.id=o.account_id
		WHERE o.id=? AND o.account_id=? AND o.transport=? AND o.status=?`+clause+`
		AND a.provider=o.transport AND a.auth_method='oauth2' AND COALESCE(a.provider_account_id,'')=? AND COALESCE(a.is_deleting,0)=0`, send.ID, send.AccountID, send.Transport, status, attempt, subject).Scan(&count)
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrMessageMutationSuperseded
	}
	return nil
}

func (db *DB) CompleteUserProviderSend(ctx context.Context, send OutgoingSend, subject, internetID, providerID string) error {
	if internetID == "" {
		return errors.New("sent message identity is required")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkUserSendAttempt(ctx, tx, send, subject, false); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gofer_provider_send_receipts(send_id,provider,mailbox_subject,provider_message_id) VALUES(?,?,?,?)`, send.ID, send.Transport, subject, providerID); err != nil {
		return err
	}
	// Keep MIME and snapshot until local Sent publication succeeds. No subsequent
	// cache failure can return this accepted delivery to the pending send queue.
	if _, err := tx.ExecContext(ctx, `UPDATE outgoing_sends SET status='sent',sent_message_id=?,last_error='',locked_at=NULL,
		sent_copy_status='pending',sent_copy_attempt_count=0,sent_copy_last_error='',sent_copy_locked_at=NULL,sent_copy_next_attempt_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=?`, internetID, send.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) UserProviderSendReceipt(ctx context.Context, sendID, accountID string) (string, error) {
	var id string
	err := db.Read().QueryRowContext(ctx, `SELECT r.provider_message_id FROM gofer_provider_send_receipts r JOIN outgoing_sends o ON o.id=r.send_id JOIN accounts a ON a.id=o.account_id
		WHERE o.id=? AND o.account_id=? AND o.status='sent' AND r.provider=o.transport AND a.provider=r.provider AND a.auth_method='oauth2' AND a.provider_account_id=r.mailbox_subject AND COALESCE(a.is_deleting,0)=0`, sendID, accountID).Scan(&id)
	return id, err
}

func (db *DB) CompleteUserProviderSentCache(ctx context.Context, send OutgoingSend, subject, internetID, folder, providerID string) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkUserSendAttempt(ctx, tx, send, subject, true); err != nil {
		return err
	}
	var storedID string
	if err := tx.QueryRowContext(ctx, `SELECT provider_message_id FROM gofer_provider_send_receipts WHERE send_id=? AND provider=? AND mailbox_subject=?`, send.ID, send.Transport, subject).Scan(&storedID); err != nil {
		return err
	}
	if storedID != providerID {
		return errors.New("provider send receipt changed")
	}
	var messageID int64
	if err := tx.QueryRowContext(ctx, `SELECT m.id FROM messages m JOIN message_folder_state s ON s.message_id=m.id JOIN folders f ON f.id=s.folder_id
		WHERE m.account_id=? AND m.internet_message_id=? AND f.id=? AND f.account_id=m.account_id AND f.role='sent' AND s.is_deleted=0`, send.AccountID, internetID, folder).Scan(&messageID); err != nil {
		return err
	}
	if providerID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET remote_message_id=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, providerID, messageID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE outgoing_sends SET sent_copy_status='complete',sent_copy_last_error='',sent_copy_locked_at=NULL,
		envelope_recipients='[]',mime_data=NULL,message_json='',updated_at=CURRENT_TIMESTAMP WHERE id=?`, send.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gofer_provider_send_receipts WHERE send_id=?`, send.ID); err != nil {
		return err
	}
	return tx.Commit()
}

const providerSendReceiptSchema = `CREATE TABLE IF NOT EXISTS gofer_provider_send_receipts (
		send_id TEXT PRIMARY KEY REFERENCES outgoing_sends(id) ON DELETE CASCADE,
		provider TEXT NOT NULL CHECK(provider IN ('gmail','outlook')),
		mailbox_subject TEXT NOT NULL,
		provider_message_id TEXT NOT NULL DEFAULT ''
	)`
