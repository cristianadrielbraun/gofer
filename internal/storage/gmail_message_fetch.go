package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

type GmailMessageFetchQueueEntry struct {
	AccountID         string
	ProviderMessageID string
	HistoryID         string
	Attempts          int
	LastError         string
}

func (db *DB) EnqueueGmailMessageFetch(ctx context.Context, accountID, providerMessageID, historyID string, fetchErr error) error {
	accountID = strings.TrimSpace(accountID)
	providerMessageID = strings.TrimSpace(providerMessageID)
	historyID = strings.TrimSpace(historyID)
	if accountID == "" || providerMessageID == "" {
		return nil
	}
	lastError := ""
	if fetchErr != nil {
		lastError = strings.TrimSpace(fetchErr.Error())
	}
	nextAttemptAt := time.Now().UTC().Add(gmailMessageFetchRetryDelay(1))
	_, err := db.Write().ExecContext(ctx, `
		INSERT INTO gmail_message_fetch_queue (
			account_id, provider_message_id, history_id, attempts,
			next_attempt_at, last_error, updated_at
		) VALUES (?, ?, ?, 1, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(account_id, provider_message_id) DO UPDATE SET
			history_id = CASE WHEN excluded.history_id != '' THEN excluded.history_id ELSE gmail_message_fetch_queue.history_id END,
			attempts = CASE WHEN gmail_message_fetch_queue.attempts > 0 THEN gmail_message_fetch_queue.attempts ELSE 1 END,
			last_error = excluded.last_error,
			updated_at = CURRENT_TIMESTAMP`,
		accountID, providerMessageID, historyID, formatDBTime(nextAttemptAt), lastError)
	return err
}

func (db *DB) ListDueGmailMessageFetches(ctx context.Context, accountID string, limit int) ([]GmailMessageFetchQueueEntry, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := db.Read().QueryContext(ctx, `
		SELECT account_id, provider_message_id, history_id, attempts, last_error
		FROM gmail_message_fetch_queue
		WHERE account_id = ? AND next_attempt_at <= ?
		ORDER BY next_attempt_at ASC, first_seen_at ASC, provider_message_id ASC
		LIMIT ?`, accountID, formatDBTime(time.Now().UTC()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []GmailMessageFetchQueueEntry
	for rows.Next() {
		var entry GmailMessageFetchQueueEntry
		if err := rows.Scan(&entry.AccountID, &entry.ProviderMessageID, &entry.HistoryID, &entry.Attempts, &entry.LastError); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (db *DB) HasDueGmailMessageFetch(ctx context.Context, accountID string) (bool, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return false, nil
	}
	var due int
	err := db.Read().QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM gmail_message_fetch_queue
			WHERE account_id = ? AND next_attempt_at <= ?
		)`, accountID, formatDBTime(time.Now().UTC())).Scan(&due)
	return due != 0, err
}

func (db *DB) MarkGmailMessageFetchError(ctx context.Context, entry GmailMessageFetchQueueEntry, fetchErr error) error {
	accountID := strings.TrimSpace(entry.AccountID)
	providerMessageID := strings.TrimSpace(entry.ProviderMessageID)
	if accountID == "" || providerMessageID == "" {
		return nil
	}
	attempts := entry.Attempts + 1
	if attempts < 1 {
		attempts = 1
	}
	lastError := ""
	if fetchErr != nil {
		lastError = strings.TrimSpace(fetchErr.Error())
	}
	nextAttemptAt := time.Now().UTC().Add(gmailMessageFetchRetryDelay(attempts))
	_, err := db.Write().ExecContext(ctx, `
		UPDATE gmail_message_fetch_queue
		SET attempts = ?, next_attempt_at = ?, last_error = ?, updated_at = CURRENT_TIMESTAMP
		WHERE account_id = ? AND provider_message_id = ?`,
		attempts, formatDBTime(nextAttemptAt), lastError, accountID, providerMessageID)
	return err
}

func (db *DB) CompleteGmailMessageFetch(ctx context.Context, accountID, providerMessageID string) error {
	accountID = strings.TrimSpace(accountID)
	providerMessageID = strings.TrimSpace(providerMessageID)
	if accountID == "" || providerMessageID == "" {
		return nil
	}
	_, err := db.Write().ExecContext(ctx, `
		DELETE FROM gmail_message_fetch_queue
		WHERE account_id = ? AND provider_message_id = ?`, accountID, providerMessageID)
	return err
}

func gmailMessageFetchRetryDelay(attempts int) time.Duration {
	if attempts <= 0 {
		attempts = 1
	}
	if attempts > 9 {
		attempts = 9
	}
	minutes := 1 << (attempts - 1)
	if minutes > 360 {
		minutes = 360
	}
	return time.Duration(minutes) * time.Minute
}

// MarkProviderMessageDeleted hides a provider-confirmed deletion without
// physically removing the cached message, so a later full sync can restore it.
func (db *DB) MarkProviderMessageDeleted(ctx context.Context, accountID, providerMessageID string) ([]string, error) {
	accountID = strings.TrimSpace(accountID)
	providerMessageID = strings.TrimSpace(providerMessageID)
	if accountID == "" || providerMessageID == "" {
		return nil, nil
	}

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT mfs.folder_id
		FROM message_folder_state mfs
		JOIN messages m ON m.id = mfs.message_id
		WHERE m.account_id = ? AND m.remote_message_id = ? AND mfs.is_deleted = 0`, accountID, providerMessageID)
	if err != nil {
		return nil, err
	}
	folderSet := map[string]bool{}
	for rows.Next() {
		var folderID string
		if err := rows.Scan(&folderID); err != nil {
			rows.Close()
			return nil, err
		}
		if folderID = strings.TrimSpace(folderID); folderID != "" {
			folderSet[folderID] = true
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE message_folder_state
		SET is_deleted = 1, synced_at = CURRENT_TIMESTAMP
		WHERE message_id IN (
			SELECT id FROM messages WHERE account_id = ? AND remote_message_id = ?
		)`, accountID, providerMessageID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM message_mutations
		WHERE kind = ? AND status = ? AND message_id IN (
			SELECT id FROM messages WHERE account_id = ? AND remote_message_id = ?
		)`, MessageMutationDelete, MessageMutationApplied, accountID, providerMessageID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	folderIDs := make([]string, 0, len(folderSet))
	for folderID := range folderSet {
		folderIDs = append(folderIDs, folderID)
	}
	sort.Strings(folderIDs)
	for _, folderID := range folderIDs {
		if _, err := db.RefreshFolderUnreadCount(ctx, folderID); err != nil {
			return folderIDs, fmt.Errorf("refresh deleted provider message unread count %s: %w", folderID, err)
		}
		if err := db.RefreshFolderThreadState(ctx, folderID); err != nil {
			return folderIDs, fmt.Errorf("refresh deleted provider message thread state %s: %w", folderID, err)
		}
	}
	return folderIDs, nil
}
