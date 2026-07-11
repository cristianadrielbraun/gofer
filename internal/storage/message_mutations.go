package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	MessageMutationRead    = "read"
	MessageMutationStarred = "starred"
	MessageMutationMove    = "move"
)

const (
	MessageMutationProviderGmail   = "gmail"
	MessageMutationProviderOutlook = "outlook"
	MessageMutationProviderIMAP    = "imap"
)

const (
	MessageMutationPending    = "pending"
	MessageMutationProcessing = "processing"
	MessageMutationFailed     = "failed"
	MessageMutationApplied    = "applied"
)

type MessageMutation struct {
	ID                  string
	AccountID           string
	MessageID           int64
	FolderID            string
	DestinationFolderID string
	ProviderType        string
	Kind                string
	TargetValue         bool
	Status              string
	AttemptCount        int
	LastError           string
	NextAttemptAt       time.Time
}

func (db *DB) MoveMessageAndQueue(ctx context.Context, messageID int64, sourceFolderID, destinationFolderID string) error {
	return db.MoveMessagesAndQueue(ctx, []int64{messageID}, sourceFolderID, destinationFolderID)
}

func (db *DB) MoveMessagesAndQueue(ctx context.Context, messageIDs []int64, sourceFolderID, destinationFolderID string) error {
	messageIDs = uniquePositiveInt64s(messageIDs)
	sourceFolderID = strings.TrimSpace(sourceFolderID)
	destinationFolderID = strings.TrimSpace(destinationFolderID)
	if len(messageIDs) == 0 {
		return nil
	}
	if sourceFolderID == "" || destinationFolderID == "" || sourceFolderID == destinationFolderID {
		return fmt.Errorf("different source and destination folders are required")
	}

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var destinationAccountID, destinationRemoteID string
	if err := tx.QueryRowContext(ctx, `SELECT account_id, COALESCE(remote_id, '') FROM folders WHERE id = ?`, destinationFolderID).Scan(&destinationAccountID, &destinationRemoteID); err != nil {
		return fmt.Errorf("load destination folder: %w", err)
	}
	if strings.TrimSpace(destinationRemoteID) == "" {
		return fmt.Errorf("destination folder has no remote identity")
	}

	refreshFolders := map[string]struct{}{sourceFolderID: {}, destinationFolderID: {}}
	for _, messageID := range messageIDs {
		var accountID, provider string
		var isRead, isStarred int
		if err := tx.QueryRowContext(ctx, `
			SELECT m.account_id, a.provider, mfs.is_read, mfs.is_starred
			FROM messages m
			JOIN accounts a ON a.id = m.account_id
			JOIN message_folder_state mfs ON mfs.message_id = m.id
			WHERE m.id = ? AND mfs.folder_id = ? AND mfs.is_deleted = 0`, messageID, sourceFolderID).
			Scan(&accountID, &provider, &isRead, &isStarred); err != nil {
			return fmt.Errorf("load message %d in source folder: %w", messageID, err)
		}
		if accountID != destinationAccountID {
			return fmt.Errorf("message %d and destination folder belong to different accounts", messageID)
		}

		providerType := messageMutationProviderType(provider)
		var mutationID, remoteFolderID, status string
		err := tx.QueryRowContext(ctx, `
			SELECT id, folder_id, status
			FROM message_mutations
			WHERE message_id = ? AND kind = ?
			ORDER BY created_at LIMIT 1`, messageID, MessageMutationMove).
			Scan(&mutationID, &remoteFolderID, &status)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == sql.ErrNoRows {
			mutationID = uuid.NewString()
			remoteFolderID = sourceFolderID
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO message_mutations (
					id, account_id, message_id, folder_id, provider_type, kind, target_value,
					destination_folder_id, status, attempt_count, last_error, locked_at, next_attempt_at
				) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, 0, '', NULL, CURRENT_TIMESTAMP)`,
				mutationID, accountID, messageID, remoteFolderID, providerType, MessageMutationMove,
				destinationFolderID, MessageMutationPending); err != nil {
				return fmt.Errorf("queue move for message %d: %w", messageID, err)
			}
		} else if remoteFolderID == destinationFolderID && status != MessageMutationProcessing {
			if _, err := tx.ExecContext(ctx, `DELETE FROM message_mutations WHERE id = ?`, mutationID); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
				UPDATE message_mutations
				SET account_id = ?, provider_type = ?, destination_folder_id = ?, status = ?,
				    attempt_count = 0, last_error = '', locked_at = NULL,
				    next_attempt_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
				WHERE id = ?`, accountID, providerType, destinationFolderID, MessageMutationPending, mutationID); err != nil {
				return fmt.Errorf("update queued move for message %d: %w", messageID, err)
			}
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE message_folder_state SET is_deleted = 1, synced_at = CURRENT_TIMESTAMP
			WHERE message_id = ? AND folder_id = ?`, messageID, sourceFolderID); err != nil {
			return fmt.Errorf("hide source folder for message %d: %w", messageID, err)
		}
		preserveRemoteUID := remoteFolderID == destinationFolderID
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO message_folder_state (
				message_id, folder_id, remote_uid, is_read, is_starred, is_flagged, is_draft, is_deleted, synced_at
			) VALUES (?, ?, NULL, ?, ?, 0, 0, 0, CURRENT_TIMESTAMP)
			ON CONFLICT(message_id, folder_id) DO UPDATE SET
				remote_uid = CASE WHEN ? THEN message_folder_state.remote_uid ELSE NULL END,
				is_read = excluded.is_read,
				is_starred = excluded.is_starred,
				is_deleted = 0,
				synced_at = CURRENT_TIMESTAMP`,
			messageID, destinationFolderID, isRead, isStarred, boolInt(preserveRemoteUID)); err != nil {
			return fmt.Errorf("show destination folder for message %d: %w", messageID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for folderID := range refreshFolders {
		_, _ = db.RefreshFolderUnreadCount(ctx, folderID)
		_ = db.RefreshFolderThreadState(ctx, folderID)
	}
	return nil
}

func (db *DB) SetMessageReadAndQueue(ctx context.Context, messageID int64, read bool) error {
	return db.SetMessagesReadAndQueue(ctx, []int64{messageID}, read)
}

func (db *DB) SetMessagesReadAndQueue(ctx context.Context, messageIDs []int64, read bool) error {
	return db.setMessageStateAndQueue(ctx, messageIDs, MessageMutationRead, read)
}

func (db *DB) SetMessageStarredAndQueue(ctx context.Context, messageID int64, starred bool) error {
	return db.SetMessagesStarredAndQueue(ctx, []int64{messageID}, starred)
}

func (db *DB) SetMessagesStarredAndQueue(ctx context.Context, messageIDs []int64, starred bool) error {
	return db.setMessageStateAndQueue(ctx, messageIDs, MessageMutationStarred, starred)
}

func (db *DB) setMessageStateAndQueue(ctx context.Context, messageIDs []int64, kind string, target bool) error {
	if kind != MessageMutationRead && kind != MessageMutationStarred {
		return fmt.Errorf("unsupported message mutation kind %q", kind)
	}
	messageIDs = uniquePositiveInt64s(messageIDs)
	if len(messageIDs) == 0 {
		return nil
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	folderSet := make(map[string]struct{})
	for _, messageID := range messageIDs {
		var accountID, provider string
		if err := tx.QueryRowContext(ctx, `
			SELECT m.account_id, a.provider
			FROM messages m JOIN accounts a ON a.id = m.account_id
			WHERE m.id = ?`, messageID).Scan(&accountID, &provider); err != nil {
			return fmt.Errorf("load message %d for mutation: %w", messageID, err)
		}
		providerType := messageMutationProviderType(provider)
		scopes := []string{""}
		if providerType == MessageMutationProviderIMAP {
			scopes, err = messageMutationFolderScopesTx(ctx, tx, messageID)
			if err != nil {
				return err
			}
			if len(scopes) == 0 {
				return fmt.Errorf("message %d has no IMAP folder identity", messageID)
			}
		}
		for _, folderID := range scopes {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO message_mutations (
					id, account_id, message_id, folder_id, provider_type, kind, target_value,
					status, attempt_count, last_error, locked_at, next_attempt_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, '', NULL, CURRENT_TIMESTAMP)
				ON CONFLICT(message_id, kind, folder_id) DO UPDATE SET
					account_id = excluded.account_id,
					provider_type = excluded.provider_type,
					target_value = excluded.target_value,
					status = excluded.status,
					attempt_count = 0,
					last_error = '',
					locked_at = NULL,
					next_attempt_at = CURRENT_TIMESTAMP,
					updated_at = CURRENT_TIMESTAMP`,
				uuid.NewString(), accountID, messageID, folderID, providerType, kind, boolInt(target), MessageMutationPending); err != nil {
				return fmt.Errorf("queue %s mutation for message %d: %w", kind, messageID, err)
			}
		}
		column := "is_read"
		if kind == MessageMutationStarred {
			column = "is_starred"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE message_folder_state SET `+column+` = ? WHERE message_id = ?`, boolInt(target), messageID); err != nil {
			return fmt.Errorf("update local %s state for message %d: %w", kind, messageID, err)
		}
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT folder_id FROM message_folder_state WHERE message_id = ?`, messageID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var folderID string
			if err := rows.Scan(&folderID); err != nil {
				rows.Close()
				return err
			}
			folderSet[folderID] = struct{}{}
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for folderID := range folderSet {
		if kind == MessageMutationRead {
			_, _ = db.RefreshFolderUnreadCount(ctx, folderID)
		} else {
			_ = db.RefreshFolderThreadState(ctx, folderID)
		}
	}
	return nil
}

func messageMutationFolderScopesTx(ctx context.Context, tx *sql.Tx, messageID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT mfs.folder_id
		FROM message_folder_state mfs
		JOIN folders f ON f.id = mfs.folder_id
		WHERE mfs.message_id = ? AND mfs.is_deleted = 0 AND COALESCE(f.remote_id, '') != ''`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var folderIDs []string
	for rows.Next() {
		var folderID string
		if err := rows.Scan(&folderID); err != nil {
			return nil, err
		}
		folderIDs = append(folderIDs, folderID)
	}
	return folderIDs, rows.Err()
}

func messageMutationProviderType(provider string) string {
	switch strings.TrimSpace(provider) {
	case MessageMutationProviderGmail:
		return MessageMutationProviderGmail
	case MessageMutationProviderOutlook:
		return MessageMutationProviderOutlook
	default:
		return MessageMutationProviderIMAP
	}
}

func uniquePositiveInt64s(values []int64) []int64 {
	seen := make(map[int64]struct{}, len(values))
	out := make([]int64, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (db *DB) ClaimDueMessageMutations(ctx context.Context, now time.Time, limit int) ([]MessageMutation, error) {
	if limit <= 0 {
		limit = 25
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, messageMutationSelect+`
		WHERE status IN (?, ?) AND next_attempt_at <= ?
		ORDER BY created_at ASC LIMIT ?`, MessageMutationPending, MessageMutationFailed, now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	var mutations []MessageMutation
	for rows.Next() {
		mutation, err := scanMessageMutation(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		mutations = append(mutations, mutation)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range mutations {
		result, err := tx.ExecContext(ctx, `
			UPDATE message_mutations
			SET status = ?, attempt_count = attempt_count + 1, locked_at = ?, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND status = ?`, MessageMutationProcessing, now.UTC(), mutations[i].ID, mutations[i].Status)
		if err != nil {
			return nil, err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return nil, fmt.Errorf("message mutation %s was claimed concurrently", mutations[i].ID)
		}
		mutations[i].AttemptCount++
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return mutations, nil
}

func (db *DB) MarkInterruptedMessageMutationsPending(ctx context.Context) (int64, error) {
	result, err := db.Write().ExecContext(ctx, `
		UPDATE message_mutations
		SET status = ?, locked_at = NULL, next_attempt_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE status = ?`, MessageMutationPending, MessageMutationProcessing)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (db *DB) CompleteMessageMutation(ctx context.Context, id string) error {
	_, err := db.Write().ExecContext(ctx, `
		UPDATE message_mutations
		SET status = ?, last_error = '', locked_at = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ?`, MessageMutationApplied, id, MessageMutationProcessing)
	return err
}

func (db *DB) AdvanceMessageMoveMutation(ctx context.Context, id, destinationFolderID, providerMessageID string, destinationUID uint32) error {
	destinationFolderID = strings.TrimSpace(destinationFolderID)
	if strings.TrimSpace(id) == "" || destinationFolderID == "" {
		return fmt.Errorf("move mutation and destination folder are required")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var messageID int64
	var providerType, previousFolderID string
	if err := tx.QueryRowContext(ctx, `
		SELECT message_id, provider_type, folder_id FROM message_mutations WHERE id = ? AND kind = ?`, id, MessageMutationMove).
		Scan(&messageID, &providerType, &previousFolderID); err != nil {
		return err
	}
	if providerMessageID = strings.TrimSpace(providerMessageID); providerMessageID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET remote_message_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, providerMessageID, messageID); err != nil {
			return err
		}
	}
	if destinationUID > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM message_folder_state WHERE folder_id = ? AND remote_uid = ? AND message_id != ?`, destinationFolderID, destinationUID, messageID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE message_folder_state SET remote_uid = ?, synced_at = CURRENT_TIMESTAMP WHERE message_id = ? AND folder_id = ?`, destinationUID, messageID, destinationFolderID); err != nil {
			return err
		}
	}
	if providerType == MessageMutationProviderIMAP && previousFolderID != destinationFolderID {
		for _, kind := range []string{MessageMutationRead, MessageMutationStarred} {
			var destinationMutationID string
			err := tx.QueryRowContext(ctx, `SELECT id FROM message_mutations WHERE message_id = ? AND kind = ? AND folder_id = ?`, messageID, kind, destinationFolderID).Scan(&destinationMutationID)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if err == nil {
				if _, err := tx.ExecContext(ctx, `DELETE FROM message_mutations WHERE message_id = ? AND kind = ? AND folder_id = ?`, messageID, kind, previousFolderID); err != nil {
					return err
				}
			} else if _, err := tx.ExecContext(ctx, `UPDATE message_mutations SET folder_id = ?, updated_at = CURRENT_TIMESTAMP WHERE message_id = ? AND kind = ? AND folder_id = ?`, destinationFolderID, messageID, kind, previousFolderID); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE message_mutations SET folder_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, destinationFolderID, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) FinishMessageMutationWithError(ctx context.Context, id, errorText string, nextAttempt time.Time) error {
	_, err := db.Write().ExecContext(ctx, `
		UPDATE message_mutations
		SET status = ?, last_error = ?, locked_at = NULL, next_attempt_at = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ?`, MessageMutationFailed, errorText, nextAttempt.UTC(), id, MessageMutationProcessing)
	return err
}

func (db *DB) DiscardMessageMutation(ctx context.Context, id string) error {
	_, err := db.Write().ExecContext(ctx, `DELETE FROM message_mutations WHERE id = ? AND status = ?`, id, MessageMutationProcessing)
	return err
}

func (db *DB) GetMessageMutation(ctx context.Context, id string) (MessageMutation, error) {
	return scanMessageMutation(db.Read().QueryRowContext(ctx, messageMutationSelect+` WHERE id = ?`, id))
}

func resolveMessageMutationTargetsTx(ctx context.Context, tx *sql.Tx, messageID int64, folderID string, read, starred *bool) error {
	rows, err := tx.QueryContext(ctx, messageMutationSelect+`
		WHERE message_id = ? AND (folder_id = '' OR folder_id = ?)`, messageID, folderID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var mutations []MessageMutation
	for rows.Next() {
		mutation, err := scanMessageMutation(rows)
		if err != nil {
			return err
		}
		mutations = append(mutations, mutation)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, mutation := range mutations {
		var incoming *bool
		switch mutation.Kind {
		case MessageMutationRead:
			incoming = read
		case MessageMutationStarred:
			incoming = starred
		case MessageMutationMove:
			if mutation.Status == MessageMutationApplied && mutation.DestinationFolderID == folderID {
				if _, err := tx.ExecContext(ctx, `DELETE FROM message_mutations WHERE id = ? AND status = ?`, mutation.ID, MessageMutationApplied); err != nil {
					return err
				}
			}
			continue
		}
		if incoming == nil {
			continue
		}
		if mutation.Status == MessageMutationApplied && *incoming == mutation.TargetValue {
			if _, err := tx.ExecContext(ctx, `DELETE FROM message_mutations WHERE id = ? AND status = ?`, mutation.ID, MessageMutationApplied); err != nil {
				return err
			}
			continue
		}
		*incoming = mutation.TargetValue
	}
	return nil
}

func protectProviderMoveFolderTx(ctx context.Context, tx *sql.Tx, messageID int64, folderID string) (bool, error) {
	var destinationFolderID string
	err := tx.QueryRowContext(ctx, `
		SELECT destination_folder_id
		FROM message_mutations
		WHERE message_id = ? AND kind = ?
		ORDER BY created_at LIMIT 1`, messageID, MessageMutationMove).Scan(&destinationFolderID)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if folderID == destinationFolderID {
		return true, nil
	}
	var deleted int
	err = tx.QueryRowContext(ctx, `SELECT is_deleted FROM message_folder_state WHERE message_id = ? AND folder_id = ?`, messageID, folderID).Scan(&deleted)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return deleted == 0, nil
}

func resolveProviderMoveFoldersTx(ctx context.Context, tx *sql.Tx, messageID int64, observed, desired map[string]bool) error {
	mutation, err := scanMessageMutation(tx.QueryRowContext(ctx, messageMutationSelect+`
		WHERE message_id = ? AND kind = ? ORDER BY created_at LIMIT 1`, messageID, MessageMutationMove))
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT folder_id FROM message_folder_state WHERE message_id = ? AND is_deleted = 1`, messageID)
	if err != nil {
		return err
	}
	var tombstones []string
	for rows.Next() {
		var folderID string
		if err := rows.Scan(&folderID); err != nil {
			rows.Close()
			return err
		}
		tombstones = append(tombstones, folderID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	confirmed := mutation.Status == MessageMutationApplied && observed[mutation.DestinationFolderID]
	for _, folderID := range tombstones {
		if observed[folderID] {
			confirmed = false
		}
	}
	if confirmed {
		_, err := tx.ExecContext(ctx, `DELETE FROM message_mutations WHERE id = ? AND status = ?`, mutation.ID, MessageMutationApplied)
		return err
	}
	desired[mutation.DestinationFolderID] = true
	for _, folderID := range tombstones {
		delete(desired, folderID)
	}
	return nil
}

const messageMutationSelect = `SELECT
	id, account_id, message_id, folder_id, destination_folder_id, provider_type, kind, target_value,
	status, attempt_count, last_error, next_attempt_at
	FROM message_mutations`

func scanMessageMutation(row rowScanner) (MessageMutation, error) {
	var mutation MessageMutation
	var target int
	var nextAttempt sqliteNullTime
	if err := row.Scan(
		&mutation.ID, &mutation.AccountID, &mutation.MessageID, &mutation.FolderID, &mutation.DestinationFolderID,
		&mutation.ProviderType, &mutation.Kind, &target, &mutation.Status,
		&mutation.AttemptCount, &mutation.LastError, &nextAttempt,
	); err != nil {
		return MessageMutation{}, err
	}
	mutation.TargetValue = target != 0
	if nextAttempt.Valid {
		mutation.NextAttemptAt = nextAttempt.Time
	}
	return mutation, nil
}
