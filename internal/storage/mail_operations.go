package storage

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

var ErrMailOperationNotRetryable = errors.New("mail operation is not retryable in its current state")

var (
	mailOperationSecretPattern = regexp.MustCompile(`(?i)(access[_ -]?token|refresh[_ -]?token|authorization|password|client[_ -]?secret|secret)\s*[:=]\s*["']?[^,\s"'}]+`)
	mailOperationBearerPattern = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
)

const mailOperationProjection = `
SELECT operation_id, operation_type, account_id, account_email, provider,
       message_id, folder_id, folder_name, destination_folder_id, destination_folder_name,
       label_name, operation, draft_key, state, attempts, last_error,
       next_retry_at, created_at, updated_at, can_retry, can_reconcile, can_cancel, ambiguous
FROM (
    SELECT
        'message_mutation:' || mm.id AS operation_id,
        ? AS operation_type,
        mm.account_id,
        COALESCE(a.email_address, '') AS account_email,
        COALESCE(a.provider, '') AS provider,
        mm.message_id,
        COALESCE(mm.folder_id, '') AS folder_id,
        CASE WHEN mm.folder_id = '' THEN '' ELSE COALESCE(NULLIF(sf.remote_id, ''), NULLIF(sf.role, ''), mm.folder_id) END AS folder_name,
        COALESCE(mm.destination_folder_id, '') AS destination_folder_id,
        CASE WHEN mm.destination_folder_id = '' THEN '' ELSE COALESCE(NULLIF(df.remote_id, ''), NULLIF(df.role, ''), mm.destination_folder_id) END AS destination_folder_name,
        '' AS label_name,
        mm.kind AS operation,
        '' AS draft_key,
        mm.status AS state,
        mm.attempt_count AS attempts,
        COALESCE(mm.last_error, '') AS last_error,
        mm.next_attempt_at AS next_retry_at,
        mm.created_at AS created_at,
        mm.updated_at AS updated_at,
        CASE WHEN mm.status = 'failed' THEN 1 ELSE 0 END AS can_retry,
        0 AS can_reconcile,
        0 AS can_cancel,
        0 AS ambiguous
    FROM message_mutations mm
    JOIN accounts a ON a.id = mm.account_id
    LEFT JOIN folders sf ON sf.id = mm.folder_id
    LEFT JOIN folders df ON df.id = mm.destination_folder_id
    WHERE mm.status != 'applied'

    UNION ALL

    SELECT
        'label_mutation:' || lm.id AS operation_id,
        ? AS operation_type,
        lm.account_id,
        COALESCE(a.email_address, '') AS account_email,
        COALESCE(a.provider, '') AS provider,
        lm.message_id,
        COALESCE(lm.folder_id, '') AS folder_id,
        CASE WHEN lm.folder_id = '' THEN '' ELSE COALESCE(NULLIF(lf.remote_id, ''), NULLIF(lf.role, ''), lm.folder_id) END AS folder_name,
        '' AS destination_folder_id,
        '' AS destination_folder_name,
        COALESCE(lm.label_name, '') AS label_name,
        lm.operation,
        '' AS draft_key,
        CASE WHEN lm.attempts > 0 OR COALESCE(lm.last_error, '') != '' THEN 'failed' ELSE 'pending' END AS state,
        lm.attempts,
        COALESCE(lm.last_error, '') AS last_error,
        lm.next_attempt_at,
        lm.created_at,
        lm.updated_at,
        1 AS can_retry,
        0 AS can_reconcile,
        0 AS can_cancel,
        0 AS ambiguous
    FROM label_mutation_queue lm
    JOIN accounts a ON a.id = lm.account_id
    LEFT JOIN folders lf ON lf.id = lm.folder_id

    UNION ALL

    SELECT
        'imap_draft:' || io.id AS operation_id,
        ? AS operation_type,
        io.account_id,
        COALESCE(a.email_address, '') AS account_email,
        COALESCE(a.provider, '') AS provider,
        COALESCE(s.local_message_id, 0) AS message_id,
        COALESCE(s.folder_id, '') AS folder_id,
        CASE WHEN s.folder_id = '' THEN '' ELSE COALESCE(NULLIF(f.remote_id, ''), NULLIF(f.role, ''), s.folder_id) END AS folder_name,
        '' AS destination_folder_id,
        '' AS destination_folder_name,
        '' AS label_name,
        io.kind AS operation,
        io.draft_key,
        io.status AS state,
        io.attempt_count,
        COALESCE(io.last_error, '') AS last_error,
        io.next_attempt_at,
        io.created_at,
        io.updated_at,
        CASE WHEN io.status = 'failed' THEN 1 ELSE 0 END AS can_retry,
        CASE WHEN io.status = 'ambiguous' THEN 1 ELSE 0 END AS can_reconcile,
        0 AS can_cancel,
        CASE WHEN io.status = 'ambiguous' THEN 1 ELSE 0 END AS ambiguous
    FROM imap_draft_operations io
    JOIN accounts a ON a.id = io.account_id
    JOIN imap_draft_states s ON s.account_id = io.account_id AND s.draft_key = io.draft_key
    LEFT JOIN folders f ON f.id = s.folder_id
    WHERE io.status IN ('pending', 'syncing', 'failed', 'ambiguous')

    UNION ALL

    SELECT
        'sent_copy:' || os.id AS operation_id,
        ? AS operation_type,
        os.account_id,
        COALESCE(a.email_address, '') AS account_email,
        COALESCE(a.provider, '') AS provider,
        COALESCE(os.message_id, 0) AS message_id,
        '' AS folder_id,
        '' AS folder_name,
        '' AS destination_folder_id,
        '' AS destination_folder_name,
        '' AS label_name,
        'sent_copy' AS operation,
        COALESCE(os.draft_id, '') AS draft_key,
        'sent_copy_' || os.sent_copy_status AS state,
        os.sent_copy_attempt_count,
        COALESCE(os.sent_copy_last_error, '') AS last_error,
        os.sent_copy_next_attempt_at,
        os.created_at,
        os.updated_at,
        CASE WHEN os.sent_copy_status = 'failed' THEN 1 ELSE 0 END AS can_retry,
        CASE WHEN os.sent_copy_status = 'ambiguous' THEN 1 ELSE 0 END AS can_reconcile,
        0 AS can_cancel,
        CASE WHEN os.sent_copy_status = 'ambiguous' THEN 1 ELSE 0 END AS ambiguous
    FROM outgoing_sends os
    JOIN accounts a ON a.id = os.account_id
    WHERE os.status = 'sent'
      AND os.sent_copy_status IN ('pending', 'copying', 'failed', 'ambiguous')
) operations`

func sanitizeMailOperationError(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = mailOperationBearerPattern.ReplaceAllString(value, "Bearer [redacted]")
	value = mailOperationSecretPattern.ReplaceAllString(value, "$1=[redacted]")
	if len(value) > 500 {
		value = value[:500] + "…"
	}
	return value
}

type mailOperationRowScanner interface {
	Scan(dest ...any) error
}

func scanMailOperation(row mailOperationRowScanner) (models.MailOperationSummary, error) {
	var operation models.MailOperationSummary
	var nextRetry, createdAt, updatedAt sqliteNullTime
	var canRetry, canReconcile, canCancel, ambiguous int
	if err := row.Scan(
		&operation.ID, &operation.Type, &operation.AccountID, &operation.AccountEmail, &operation.Provider,
		&operation.MessageID, &operation.FolderID, &operation.FolderName,
		&operation.DestinationFolderID, &operation.DestinationFolderName,
		&operation.LabelName, &operation.Operation, &operation.DraftKey, &operation.State,
		&operation.Attempts, &operation.LastError, &nextRetry, &createdAt, &updatedAt,
		&canRetry, &canReconcile, &canCancel, &ambiguous,
	); err != nil {
		return models.MailOperationSummary{}, err
	}
	operation.LastError = sanitizeMailOperationError(operation.LastError)
	operation.CanRetry = canRetry != 0
	operation.CanReconcile = canReconcile != 0
	operation.CanCancel = canCancel != 0
	operation.Ambiguous = ambiguous != 0
	if nextRetry.Valid {
		operation.NextRetryAt = nextRetry.Time
	}
	if createdAt.Valid {
		operation.CreatedAt = createdAt.Time
	}
	if updatedAt.Valid {
		operation.UpdatedAt = updatedAt.Time
	}
	return operation, nil
}

func (db *DB) ListMailOperationsForUser(ctx context.Context, userID string) ([]models.MailOperationSummary, error) {
	rows, err := db.Read().QueryContext(ctx, mailOperationProjection+`
WHERE account_id IN (SELECT id FROM accounts WHERE user_id = ?)
ORDER BY (can_retry OR can_reconcile OR can_cancel) DESC, updated_at DESC
LIMIT 200`,
		models.MailOperationMessageMutation, models.MailOperationLabelMutation,
		models.MailOperationIMAPDraft, models.MailOperationSentCopy, strings.TrimSpace(userID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	operations := make([]models.MailOperationSummary, 0)
	for rows.Next() {
		operation, err := scanMailOperation(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	return operations, rows.Err()
}

func (db *DB) GetMailOperationForUser(ctx context.Context, userID, operationID string) (models.MailOperationSummary, error) {
	return scanMailOperation(db.Read().QueryRowContext(ctx, mailOperationProjection+`
WHERE account_id IN (SELECT id FROM accounts WHERE user_id = ?) AND operation_id = ?`,
		models.MailOperationMessageMutation, models.MailOperationLabelMutation,
		models.MailOperationIMAPDraft, models.MailOperationSentCopy, strings.TrimSpace(userID), strings.TrimSpace(operationID)))
}

func (db *DB) ListMailOperationsAdminStatus(ctx context.Context) (models.MailOperationsAdminStatus, error) {
	rows, err := db.Read().QueryContext(ctx, mailOperationProjection+`
ORDER BY updated_at DESC`,
		models.MailOperationMessageMutation, models.MailOperationLabelMutation,
		models.MailOperationIMAPDraft, models.MailOperationSentCopy)
	if err != nil {
		return models.MailOperationsAdminStatus{}, err
	}
	defer rows.Close()

	status := models.MailOperationsAdminStatus{}
	type typeKey struct{ operationType, provider string }
	type accountKey string
	type aggregate struct {
		total, actionRequired int
	}
	types := make(map[typeKey]*aggregate)
	accounts := make(map[accountKey]*models.MailOperationAdminAccountCount)
	for rows.Next() {
		operation, err := scanMailOperation(rows)
		if err != nil {
			return models.MailOperationsAdminStatus{}, err
		}
		status.Total++
		actionRequired := operation.CanRetry || operation.CanReconcile || operation.CanCancel
		if actionRequired {
			status.ActionRequired++
		}
		key := typeKey{operation.Type, operation.Provider}
		if types[key] == nil {
			types[key] = &aggregate{}
		}
		types[key].total++
		if actionRequired {
			types[key].actionRequired++
		}
		accountID := accountKey(operation.AccountID)
		if accounts[accountID] == nil {
			accounts[accountID] = &models.MailOperationAdminAccountCount{
				AccountID:    operation.AccountID,
				AccountLabel: maskMailOperationAccount(operation.AccountEmail),
				Provider:     operation.Provider,
			}
		}
		accounts[accountID].Total++
		if actionRequired {
			accounts[accountID].ActionRequired++
		}
	}
	if err := rows.Err(); err != nil {
		return models.MailOperationsAdminStatus{}, err
	}
	for key, aggregate := range types {
		status.ByType = append(status.ByType, models.MailOperationAdminTypeCount{Type: key.operationType, Provider: key.provider, Total: aggregate.total, ActionRequired: aggregate.actionRequired})
	}
	for _, account := range accounts {
		status.ByAccount = append(status.ByAccount, *account)
	}
	sort.Slice(status.ByType, func(i, j int) bool {
		if status.ByType[i].ActionRequired != status.ByType[j].ActionRequired {
			return status.ByType[i].ActionRequired > status.ByType[j].ActionRequired
		}
		return status.ByType[i].Type+status.ByType[i].Provider < status.ByType[j].Type+status.ByType[j].Provider
	})
	sort.Slice(status.ByAccount, func(i, j int) bool {
		if status.ByAccount[i].ActionRequired != status.ByAccount[j].ActionRequired {
			return status.ByAccount[i].ActionRequired > status.ByAccount[j].ActionRequired
		}
		return status.ByAccount[i].AccountLabel < status.ByAccount[j].AccountLabel
	})
	health, err := db.ListMailOperationsAdminHealth(ctx)
	if err != nil {
		return models.MailOperationsAdminStatus{}, err
	}
	status.Health = health
	return status, nil
}

func maskMailOperationAccount(email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		return "Unknown account"
	}
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		if len(email) <= 2 {
			return "•••"
		}
		return email[:1] + "•••"
	}
	return email[:1] + "•••" + email[at:]
}

func (db *DB) RetryMailOperationForUser(ctx context.Context, userID, operationID string) (models.MailOperationSummary, error) {
	kind, rawID, ok := strings.Cut(strings.TrimSpace(operationID), ":")
	if !ok || strings.TrimSpace(rawID) == "" {
		return models.MailOperationSummary{}, ErrMailOperationNotRetryable
	}
	userID = strings.TrimSpace(userID)
	var result sql.Result
	var err error
	switch kind {
	case models.MailOperationMessageMutation:
		result, err = db.Write().ExecContext(ctx, `
			UPDATE message_mutations
			SET status = ?, locked_at = NULL, next_attempt_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND status = ? AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)`,
			MessageMutationPending, rawID, MessageMutationFailed, userID)
	case models.MailOperationLabelMutation:
		id, parseErr := strconv.ParseInt(rawID, 10, 64)
		if parseErr != nil || id <= 0 {
			return models.MailOperationSummary{}, ErrMailOperationNotRetryable
		}
		result, err = db.Write().ExecContext(ctx, `
			UPDATE label_mutation_queue
			SET next_attempt_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)`, id, userID)
	case models.MailOperationIMAPDraft:
		result, err = db.Write().ExecContext(ctx, `
			UPDATE imap_draft_operations
			SET locked_at = NULL, next_attempt_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND status IN (?, ?) AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)`,
			rawID, IMAPDraftStatusFailed, IMAPDraftStatusAmbiguous, userID)
	case models.MailOperationSentCopy:
		result, err = db.Write().ExecContext(ctx, `
			UPDATE outgoing_sends
			SET sent_copy_locked_at = NULL, sent_copy_next_attempt_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND status = ? AND sent_copy_status IN (?, ?) AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)`,
			rawID, OutgoingSendSent, SentCopyFailed, SentCopyAmbiguous, userID)
	default:
		return models.MailOperationSummary{}, ErrMailOperationNotRetryable
	}
	if err != nil {
		return models.MailOperationSummary{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return models.MailOperationSummary{}, err
	}
	if changed != 1 {
		if _, lookupErr := db.GetMailOperationForUser(ctx, userID, operationID); lookupErr != nil {
			return models.MailOperationSummary{}, lookupErr
		}
		return models.MailOperationSummary{}, ErrMailOperationNotRetryable
	}
	return db.GetMailOperationForUser(ctx, userID, operationID)
}
