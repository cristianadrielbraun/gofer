package storage

import (
	"context"
	"sort"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

type ConfiguredIdleFolder struct {
	AccountID string
	Provider  string
	FolderID  string
}

// ListConfiguredIdleFolders returns the effective IDLE selection for generic
// IMAP accounts. It uses the same defaults and role-to-folder resolution as
// the sync settings page, so admin diagnostics do not invent a second config
// interpretation.
func (db *DB) ListConfiguredIdleFolders(ctx context.Context) ([]ConfiguredIdleFolder, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT id, user_id, provider
		FROM accounts
		WHERE provider = 'imap' AND COALESCE(is_deleting, 0) = 0
		ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configured []ConfiguredIdleFolder
	for rows.Next() {
		var accountID, userID, provider string
		if err := rows.Scan(&accountID, &userID, &provider); err != nil {
			return nil, err
		}
		for folderID := range db.GetIdleFolderIDsForAccount(ctx, userID, accountID) {
			configured = append(configured, ConfiguredIdleFolder{AccountID: accountID, Provider: provider, FolderID: folderID})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(configured, func(i, j int) bool {
		if configured[i].AccountID != configured[j].AccountID {
			return configured[i].AccountID < configured[j].AccountID
		}
		return configured[i].FolderID < configured[j].FolderID
	})
	return configured, nil
}

func (db *DB) ListMailOperationsAdminHealth(ctx context.Context) (models.MailOperationAdminHealth, error) {
	health := models.MailOperationAdminHealth{}

	rows, err := db.Read().QueryContext(ctx, `
		SELECT status, COUNT(*)
		FROM outgoing_sends
		WHERE status IN ('pending', 'sending', 'failed', 'ambiguous')
		GROUP BY status`)
	if err != nil {
		return models.MailOperationAdminHealth{}, err
	}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return models.MailOperationAdminHealth{}, err
		}
		switch state {
		case OutgoingSendPending:
			health.Outgoing.Pending = count
		case OutgoingSendSending:
			health.Outgoing.Sending = count
		case OutgoingSendFailed:
			health.Outgoing.Failed = count
		case OutgoingSendAmbiguous:
			health.Outgoing.Ambiguous = count
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return models.MailOperationAdminHealth{}, err
	}
	if err := rows.Close(); err != nil {
		return models.MailOperationAdminHealth{}, err
	}

	rows, err = db.Read().QueryContext(ctx, `
		SELECT sent_copy_status, COUNT(*)
		FROM outgoing_sends
		WHERE status = 'sent' AND sent_copy_status IN ('pending', 'copying', 'failed', 'ambiguous')
		GROUP BY sent_copy_status`)
	if err != nil {
		return models.MailOperationAdminHealth{}, err
	}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return models.MailOperationAdminHealth{}, err
		}
		switch state {
		case SentCopyPending:
			health.SentCopy.Pending = count
		case SentCopyCopying:
			health.SentCopy.Copying = count
		case SentCopyFailed:
			health.SentCopy.Failed = count
		case SentCopyAmbiguous:
			health.SentCopy.Ambiguous = count
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return models.MailOperationAdminHealth{}, err
	}
	if err := rows.Close(); err != nil {
		return models.MailOperationAdminHealth{}, err
	}

	rows, err = db.Read().QueryContext(ctx, `
		SELECT kind, status, COUNT(*)
		FROM message_mutations
		GROUP BY kind, status
		ORDER BY kind, status`)
	if err != nil {
		return models.MailOperationAdminHealth{}, err
	}
	for rows.Next() {
		var item models.MailOperationAdminKindStateCount
		if err := rows.Scan(&item.Kind, &item.State, &item.Count); err != nil {
			rows.Close()
			return models.MailOperationAdminHealth{}, err
		}
		health.MessageMutations = append(health.MessageMutations, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return models.MailOperationAdminHealth{}, err
	}
	if err := rows.Close(); err != nil {
		return models.MailOperationAdminHealth{}, err
	}

	rows, err = db.Read().QueryContext(ctx, `
		SELECT CASE WHEN attempts > 0 OR COALESCE(last_error, '') != '' THEN 'failed' ELSE 'pending' END, COUNT(*)
		FROM label_mutation_queue
		GROUP BY CASE WHEN attempts > 0 OR COALESCE(last_error, '') != '' THEN 'failed' ELSE 'pending' END
		ORDER BY 1`)
	if err != nil {
		return models.MailOperationAdminHealth{}, err
	}
	for rows.Next() {
		var item models.MailOperationAdminStateCount
		if err := rows.Scan(&item.State, &item.Count); err != nil {
			rows.Close()
			return models.MailOperationAdminHealth{}, err
		}
		health.LabelMutations = append(health.LabelMutations, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return models.MailOperationAdminHealth{}, err
	}
	if err := rows.Close(); err != nil {
		return models.MailOperationAdminHealth{}, err
	}

	rows, err = db.Read().QueryContext(ctx, `
		SELECT status, COUNT(*)
		FROM imap_draft_operations
		GROUP BY status
		ORDER BY status`)
	if err != nil {
		return models.MailOperationAdminHealth{}, err
	}
	for rows.Next() {
		var item models.MailOperationAdminStateCount
		if err := rows.Scan(&item.State, &item.Count); err != nil {
			rows.Close()
			return models.MailOperationAdminHealth{}, err
		}
		health.IMAPDraftOperations = append(health.IMAPDraftOperations, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return models.MailOperationAdminHealth{}, err
	}
	if err := rows.Close(); err != nil {
		return models.MailOperationAdminHealth{}, err
	}

	var oldestPending, nextRetry sqliteNullTime
	if err := db.Read().QueryRowContext(ctx, `
		SELECT MIN(created_at)
		FROM (
			SELECT created_at FROM outgoing_sends WHERE status IN ('pending', 'sending', 'failed', 'ambiguous')
			UNION ALL
			SELECT created_at FROM outgoing_sends WHERE status = 'sent' AND sent_copy_status IN ('pending', 'copying', 'failed', 'ambiguous')
			UNION ALL
			SELECT created_at FROM message_mutations WHERE status IN ('pending', 'processing', 'failed')
			UNION ALL
			SELECT created_at FROM label_mutation_queue
			UNION ALL
			SELECT created_at FROM imap_draft_operations WHERE status IN ('pending', 'syncing', 'failed', 'ambiguous')
		)`).Scan(&oldestPending); err != nil {
		return models.MailOperationAdminHealth{}, err
	}
	if oldestPending.Valid {
		health.OldestPendingAt = oldestPending.Time
	}
	if err := db.Read().QueryRowContext(ctx, `
		SELECT MIN(next_retry_at)
		FROM (
			SELECT next_attempt_at AS next_retry_at FROM outgoing_sends WHERE status IN ('pending', 'failed')
			UNION ALL
			SELECT sent_copy_next_attempt_at FROM outgoing_sends WHERE status = 'sent' AND sent_copy_status IN ('pending', 'failed')
			UNION ALL
			SELECT next_attempt_at FROM message_mutations WHERE status IN ('pending', 'failed')
			UNION ALL
			SELECT next_attempt_at FROM label_mutation_queue
			UNION ALL
			SELECT next_attempt_at FROM imap_draft_operations WHERE status IN ('pending', 'failed')
		)`).Scan(&nextRetry); err != nil {
		return models.MailOperationAdminHealth{}, err
	}
	if nextRetry.Valid {
		health.NextRetryAt = nextRetry.Time
	}

	if err := db.Read().QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN COALESCE(f.sync_error, '') = '' AND COALESCE(f.last_full_sync_at, f.last_incremental_sync_at) IS NOT NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN COALESCE(f.sync_error, '') != '' AND COALESCE(f.last_full_sync_at, f.last_incremental_sync_at) IS NOT NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN COALESCE(f.last_full_sync_at, f.last_incremental_sync_at) IS NULL THEN 1 ELSE 0 END), 0)
		FROM folders f
		JOIN accounts a ON a.id = f.account_id
		WHERE COALESCE(a.is_deleting, 0) = 0
		  AND COALESCE(f.discovery_state, 'active') = 'active'
		  AND COALESCE(f.selectable, 1) = 1`).Scan(
		&health.FolderSync.Complete, &health.FolderSync.Partial, &health.FolderSync.Failed,
	); err != nil {
		return models.MailOperationAdminHealth{}, err
	}

	return health, nil
}
