package storage

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrMessageMutationSuperseded = errors.New("message mutation attempt was superseded")

const mutationAttemptPredicate = `id=? AND account_id=? AND message_id=? AND folder_id=? AND destination_folder_id=? AND provider_type=? AND kind=? AND target_value=? AND attempt_count=? AND status='processing'`

func mutationAttemptArgs(m MessageMutation) []any {
	return []any{m.ID, m.AccountID, m.MessageID, m.FolderID, m.DestinationFolderID, m.ProviderType, m.Kind, m.TargetValue, m.AttemptCount}
}

// FinishMessageMutationAttempt cannot overwrite a newer browser action that
// reset this row while the old provider request was in flight.
func (db *DB) FinishMessageMutationAttempt(ctx context.Context, m MessageMutation, failure string, next time.Time) error {
	args := append([]any{failure, next.UTC()}, mutationAttemptArgs(m)...)
	_, err := db.Write().ExecContext(ctx, `UPDATE message_mutations SET status='failed',last_error=?,next_attempt_at=?,locked_at=NULL,updated_at=CURRENT_TIMESTAMP WHERE `+mutationAttemptPredicate, args...)
	return err
}

// CompleteProviderMutation publishes the provider identity/move and attempt
// completion atomically. No provider HTTP belongs inside this transaction.
func (db *DB) CompleteProviderMutation(ctx context.Context, m MessageMutation, subject, originalID, originalInternetID, movedID, destinationProviderID string, alreadyGone bool) error {
	if m.ProviderType != "gmail" && m.ProviderType != "outlook" {
		return fmt.Errorf("unsupported provider mutation")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var matches int
	args := mutationAttemptArgs(m)
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM message_mutations WHERE `+mutationAttemptPredicate, args...).Scan(&matches); err != nil {
		return err
	}
	if matches != 1 {
		return ErrMessageMutationSuperseded
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM messages m JOIN accounts a ON a.id=m.account_id WHERE m.id=? AND m.account_id=? AND COALESCE(m.remote_message_id,'')=? AND COALESCE(m.internet_message_id,'')=? AND a.provider=? AND a.provider_account_id=? AND a.auth_method='oauth2' AND COALESCE(a.is_deleting,0)=0`, m.MessageID, m.AccountID, originalID, originalInternetID, m.ProviderType, subject).Scan(&matches); err != nil {
		return err
	}
	if matches != 1 {
		return fmt.Errorf("message or mailbox identity changed during provider mutation")
	}
	if originalID == "" && movedID != "" && m.Kind != MessageMutationMove {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET remote_message_id=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, movedID, m.MessageID); err != nil {
			return err
		}
	}
	if m.Kind == MessageMutationMove {
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM folders WHERE id=? AND account_id=? AND COALESCE(provider_remote_id,'')=?`, m.DestinationFolderID, m.AccountID, destinationProviderID).Scan(&matches); err != nil {
			return err
		}
		if matches != 1 {
			return fmt.Errorf("provider move destination changed")
		}
		if err := advanceMessageMoveMutationTx(ctx, tx, m.ID, m.DestinationFolderID, movedID, 0); err != nil {
			return err
		}
	}
	if m.Kind == MessageMutationDelete {
		if _, err := tx.ExecContext(ctx, `DELETE FROM message_mutations WHERE message_id=? AND id!=?`, m.MessageID, m.ID); err != nil {
			return err
		}
	}
	if alreadyGone && m.Kind == MessageMutationDelete {
		_, err = tx.ExecContext(ctx, `DELETE FROM message_mutations WHERE id=?`, m.ID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE message_mutations SET status='applied',last_error='',locked_at=NULL,updated_at=CURRENT_TIMESTAMP WHERE id=?`, m.ID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
