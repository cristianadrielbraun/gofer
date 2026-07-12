package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	FolderDiscoveryIMAP    = "imap"
	FolderDiscoveryGmail   = "gmail"
	FolderDiscoveryOutlook = "outlook"

	folderRemovalGracePeriod = 24 * time.Hour
)

// FolderDiscoveryResult describes lifecycle changes caused by one complete
// provider folder listing. A missing folder is intentionally not removed on
// the first omission; it becomes removable only after the grace period.
type FolderDiscoveryResult struct {
	MissingIDs   []string
	RemovedIDs   []string
	RecoveredIDs []string
}

func (r FolderDiscoveryResult) Changed() bool {
	return len(r.MissingIDs) > 0 || len(r.RemovedIDs) > 0 || len(r.RecoveredIDs) > 0
}

// ReconcileDiscoveredFolders applies one complete folder discovery result.
// seenIdentities must contain exact provider identities: IMAP mailbox names,
// Gmail label IDs, or Outlook Graph folder IDs. The caller must not invoke it
// after a partial or failed provider listing.
func (db *DB) ReconcileDiscoveredFolders(ctx context.Context, accountID, providerKind string, seenIdentities []string, completedAt time.Time) (FolderDiscoveryResult, error) {
	accountID = strings.TrimSpace(accountID)
	providerKind = strings.ToLower(strings.TrimSpace(providerKind))
	if accountID == "" {
		return FolderDiscoveryResult{}, nil
	}
	if providerKind != FolderDiscoveryGmail && providerKind != FolderDiscoveryOutlook {
		providerKind = FolderDiscoveryIMAP
	}
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	} else {
		completedAt = completedAt.UTC()
	}

	seen := uniqueFolderDiscoveryIdentities(seenIdentities)
	identityExpr := "COALESCE(remote_id, '')"
	if providerKind == FolderDiscoveryGmail || providerKind == FolderDiscoveryOutlook {
		identityExpr = "COALESCE(provider_remote_id, '')"
	}

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return FolderDiscoveryResult{}, fmt.Errorf("begin folder reconciliation: %w", err)
	}
	defer tx.Rollback()

	result := FolderDiscoveryResult{}
	seenClause, seenArgs := folderDiscoverySeenClause(seen)

	missingQuery := `SELECT f.id
		FROM folders f
		WHERE f.account_id = ?
		  AND COALESCE(f.discovery_state, 'active') = 'active'
		  AND ` + identityExpr + ` != ''` + folderDiscoveryNotSeenClause(identityExpr, seen)
	missingRows, err := tx.QueryContext(ctx, missingQuery, append([]any{accountID}, seenArgs...)...)
	if err != nil {
		return FolderDiscoveryResult{}, fmt.Errorf("find missing folders: %w", err)
	}
	result.MissingIDs, err = scanFolderIDs(missingRows)
	if err != nil {
		return FolderDiscoveryResult{}, fmt.Errorf("scan missing folders: %w", err)
	}

	if len(seen) > 0 {
		recoveredQuery := `SELECT f.id
			FROM folders f
			WHERE f.account_id = ?
			  AND COALESCE(f.discovery_state, 'active') != 'active'
			  AND ` + identityExpr + ` IN (` + sqlPlaceholders(len(seen)) + `)`
		recoveredRows, err := tx.QueryContext(ctx, recoveredQuery, append([]any{accountID}, seenArgs...)...)
		if err != nil {
			return FolderDiscoveryResult{}, fmt.Errorf("find recovered folders: %w", err)
		}
		result.RecoveredIDs, err = scanFolderIDs(recoveredRows)
		if err != nil {
			return FolderDiscoveryResult{}, fmt.Errorf("scan recovered folders: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `UPDATE folders
			SET discovery_state = 'active',
			    missing_since = NULL,
			    last_seen_at = ?,
			    sync_cursor = CASE WHEN discovery_state = 'removed' THEN NULL ELSE sync_cursor END,
			    uid_validity = CASE WHEN discovery_state = 'removed' THEN NULL ELSE uid_validity END,
			    uid_next = CASE WHEN discovery_state = 'removed' THEN NULL ELSE uid_next END,
			    highest_seen_uid = CASE WHEN discovery_state = 'removed' THEN 0 ELSE highest_seen_uid END,
			    highest_modseq = CASE WHEN discovery_state = 'removed' THEN NULL ELSE highest_modseq END,
			    last_full_sync_at = CASE WHEN discovery_state = 'removed' THEN NULL ELSE last_full_sync_at END,
			    last_incremental_sync_at = CASE WHEN discovery_state = 'removed' THEN NULL ELSE last_incremental_sync_at END,
			    sync_error = NULL,
			    updated_at = CURRENT_TIMESTAMP
			WHERE account_id = ? AND `+identityExpr+` IN (`+sqlPlaceholders(len(seen))+`)`, append([]any{completedAt, accountID}, seenArgs...)...); err != nil {
			return FolderDiscoveryResult{}, fmt.Errorf("restore discovered folders: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE folders
		SET discovery_state = 'missing',
		    selectable = 0,
		    missing_since = COALESCE(missing_since, ?),
		    sync_error = 'folder missing from provider discovery',
		    updated_at = CURRENT_TIMESTAMP
		WHERE account_id = ?
		  AND COALESCE(discovery_state, 'active') = 'active'
		  AND `+identityExpr+` != ''`+folderDiscoveryNotSeenClause(identityExpr, seen), append([]any{completedAt, accountID}, seenArgs...)...); err != nil {
		return FolderDiscoveryResult{}, fmt.Errorf("mark missing folders: %w", err)
	}

	cutoff := completedAt.Add(-folderRemovalGracePeriod)
	removedQuery := `SELECT f.id
		FROM folders f
		WHERE f.account_id = ?
		  AND COALESCE(f.discovery_state, 'active') = 'missing'
		  AND f.missing_since IS NOT NULL
		  AND f.missing_since <= ?
		  AND ` + identityExpr + ` != ''` + folderDiscoveryNotSeenClause(identityExpr, seen)
	removedRows, err := tx.QueryContext(ctx, removedQuery, append([]any{accountID, cutoff}, seenArgs...)...)
	if err != nil {
		return FolderDiscoveryResult{}, fmt.Errorf("find confirmed removed folders: %w", err)
	}
	result.RemovedIDs, err = scanFolderIDs(removedRows)
	if err != nil {
		return FolderDiscoveryResult{}, fmt.Errorf("scan confirmed removed folders: %w", err)
	}

	if len(result.RemovedIDs) > 0 {
		if err := quarantineRemovedFolderWork(ctx, tx, result.RemovedIDs, providerKind); err != nil {
			return FolderDiscoveryResult{}, err
		}
		placeholders := sqlPlaceholders(len(result.RemovedIDs))
		args := stringsToAny(result.RemovedIDs)
		if _, err := tx.ExecContext(ctx, `UPDATE folders
			SET discovery_state = 'removed',
			    selectable = 0,
			    sync_cursor = NULL,
			    uid_validity = NULL,
			    uid_next = NULL,
			    highest_seen_uid = 0,
			    highest_modseq = NULL,
			    last_full_sync_at = NULL,
			    last_incremental_sync_at = NULL,
			    total_count = 0,
			    unread_count = 0,
			    sync_error = 'folder removed remotely',
			    updated_at = CURRENT_TIMESTAMP
			WHERE id IN (`+placeholders+`)`, args...); err != nil {
			return FolderDiscoveryResult{}, fmt.Errorf("mark removed folders: %w", err)
		}
	}

	if len(seen) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE folders
			SET last_seen_at = ?
			WHERE account_id = ? AND `+identityExpr+` IN (`+seenClause+`)`, append([]any{completedAt, accountID}, seenArgs...)...); err != nil {
			return FolderDiscoveryResult{}, fmt.Errorf("record discovered folders: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return FolderDiscoveryResult{}, fmt.Errorf("commit folder reconciliation: %w", err)
	}
	return result, nil
}

func uniqueFolderDiscoveryIdentities(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func folderDiscoverySeenClause(seen []string) (string, []any) {
	if len(seen) == 0 {
		return "''", nil
	}
	return sqlPlaceholders(len(seen)), stringsToAny(seen)
}

func folderDiscoveryNotSeenClause(identityExpr string, seen []string) string {
	if len(seen) == 0 {
		return ""
	}
	return ` AND ` + identityExpr + ` NOT IN (` + sqlPlaceholders(len(seen)) + `)`
}

func scanFolderIDs(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func quarantineRemovedFolderWork(ctx context.Context, tx *sql.Tx, folderIDs []string, providerKind string) error {
	if len(folderIDs) == 0 {
		return nil
	}
	placeholders := sqlPlaceholders(len(folderIDs))
	args := stringsToAny(folderIDs)
	reason := fmt.Sprintf("folder removed remotely (%s discovery)", providerKind)
	terminalRetryAt := time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)

	mutationArgs := append([]any{MessageMutationFailed, reason, terminalRetryAt}, args...)
	mutationArgs = append(mutationArgs, args...)
	mutationArgs = append(mutationArgs, MessageMutationPending, MessageMutationProcessing, MessageMutationFailed)
	if _, err := tx.ExecContext(ctx, `UPDATE message_mutations
		SET status = ?, last_error = ?, locked_at = NULL, next_attempt_at = ?, updated_at = CURRENT_TIMESTAMP
		WHERE (folder_id IN (`+placeholders+`) OR destination_folder_id IN (`+placeholders+`))
		  AND status IN (?, ?, ?)`, mutationArgs...); err != nil {
		return fmt.Errorf("fail mutations for removed folders: %w", err)
	}

	labelArgs := append([]any{reason, terminalRetryAt}, args...)
	if _, err := tx.ExecContext(ctx, `UPDATE label_mutation_queue
		SET last_error = ?, next_attempt_at = ?, updated_at = CURRENT_TIMESTAMP
		WHERE folder_id IN (`+placeholders+`)`, labelArgs...); err != nil {
		return fmt.Errorf("pause label mutations for removed folders: %w", err)
	}

	draftArgs := []any{reason, terminalRetryAt}
	draftArgs = append(draftArgs, args...)
	if _, err := tx.ExecContext(ctx, `UPDATE imap_draft_operations
		SET status = 'failed', last_error = ?, locked_at = NULL, next_attempt_at = ?, updated_at = CURRENT_TIMESTAMP
		WHERE status IN ('pending', 'syncing', 'failed')
		  AND EXISTS (
			SELECT 1 FROM imap_draft_states states
			WHERE states.account_id = imap_draft_operations.account_id
			  AND states.draft_key = imap_draft_operations.draft_key
			  AND states.folder_id IN (`+placeholders+`)
		  )`, draftArgs...); err != nil {
		return fmt.Errorf("fail draft operations for removed folders: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE folders
		SET parent_id = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE parent_id IN (`+placeholders+`)`, args...); err != nil {
		return fmt.Errorf("detach children of removed folders: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM folder_thread_state WHERE folder_id IN (`+placeholders+`)`, args...); err != nil {
		return fmt.Errorf("delete removed folder thread state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_folder_state WHERE folder_id IN (`+placeholders+`)`, args...); err != nil {
		return fmt.Errorf("delete removed folder memberships: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages
		WHERE account_id IN (SELECT account_id FROM folders WHERE id IN (`+placeholders+`))
		  AND id NOT IN (SELECT message_id FROM message_folder_state)
		  AND id NOT IN (SELECT message_id FROM message_mutations WHERE status IN ('pending', 'processing', 'failed'))
		  AND id NOT IN (SELECT message_id FROM label_mutation_queue)
		  AND id NOT IN (SELECT local_message_id FROM imap_draft_states WHERE local_message_id IS NOT NULL)`, args...); err != nil {
		return fmt.Errorf("cleanup orphaned messages after folder removal: %w", err)
	}
	return nil
}
