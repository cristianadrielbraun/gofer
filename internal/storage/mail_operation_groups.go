package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// mailOperationListLimit is how many rows the Mail operations list shows:
// actions, and changes queued before Gofer recorded their action.
const mailOperationListLimit = 200

// mailOperationArgs are the projection's own arguments, which come before
// any a query adds.
func mailOperationArgs(extra ...any) []any {
	return append([]any{
		models.MailOperationMessageMutation, models.MailOperationLabelMutation,
		models.MailOperationIMAPDraft, models.MailOperationSentCopy,
	}, extra...)
}

// mailOperationGroupColumns sums up one action's changes. A folder or label
// is only given when every change that has one shares it, so "Move 3
// messages from Inbox" never names one of several folders.
const mailOperationGroupColumns = `
    action_id,
    MIN(action_kind) AS action_kind,
    MIN(action_source) AS action_source,
    MIN(action_rule_id) AS action_rule_id,
    MIN(action_rule_name) AS action_rule_name,
    MIN(account_id) AS account_id,
    MIN(account_email) AS account_email,
    COUNT(DISTINCT account_id) AS accounts,
    COUNT(DISTINCT NULLIF(message_id, 0)) AS messages,
    COUNT(*) AS jobs,
    SUM(CASE WHEN next_retry_at = ? THEN 0 WHEN can_retry = 1 THEN 1 ELSE 0 END) AS failed,
    SUM(CASE WHEN next_retry_at = ? THEN 1 ELSE 0 END) AS parked,
    SUM(CASE WHEN state = 'blocked' THEN 1 ELSE 0 END) AS blocked,
    SUM(CASE WHEN state IN ('processing', 'syncing') THEN 1 ELSE 0 END) AS active,
    CASE WHEN COUNT(DISTINCT NULLIF(folder_id, '')) = 1 THEN MAX(folder_id) ELSE '' END AS folder_id,
    CASE WHEN COUNT(DISTINCT NULLIF(destination_folder_id, '')) = 1 THEN MAX(destination_folder_id) ELSE '' END AS destination_folder_id,
    CASE WHEN COUNT(DISTINCT NULLIF(label_name, '')) = 1 THEN MAX(label_name) ELSE '' END AS label_name,
    MIN(created_at) AS created_at,
    MAX(updated_at) AS updated_at`

// ListMailOperationGroupsForUser sums up the user's queued changes by the
// action that queued them, actions that need attention first, then the most
// recently changed. Changes queued before Gofer recorded their action are left
// out; ListUngroupedMailOperationsForUser lists those.
func (db *DB) ListMailOperationGroupsForUser(ctx context.Context, userID string) ([]models.MailOperationGroup, error) {
	rows, err := db.Read().QueryContext(ctx, `
SELECT g.action_id, g.action_kind, g.action_source, g.action_rule_id, g.action_rule_name,
       g.account_id, g.account_email, g.accounts, g.messages, g.jobs,
       g.failed, g.parked, g.blocked, g.active,
       g.folder_id, COALESCE(sf.name, ''), COALESCE(sf.role, ''),
       g.destination_folder_id, COALESCE(df.name, ''), COALESCE(df.role, ''),
       g.label_name, g.created_at, g.updated_at
FROM (
    SELECT `+mailOperationGroupColumns+`
    FROM (`+db.mailOperationProjection()+`
        WHERE action_id != '' AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)
    ) p
    GROUP BY action_id
) g
LEFT JOIN folders sf ON sf.id = g.folder_id
LEFT JOIN folders df ON df.id = g.destination_folder_id
ORDER BY (g.failed + g.parked + g.blocked > 0) DESC, g.updated_at DESC, g.action_id
LIMIT ?`,
		append([]any{parkedRetryAt, parkedRetryAt}, mailOperationArgs(strings.TrimSpace(userID), mailOperationListLimit)...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]models.MailOperationGroup, 0)
	for rows.Next() {
		var group models.MailOperationGroup
		var createdAt, updatedAt sqliteNullTime
		if err := rows.Scan(
			&group.ActionID, &group.ActionKind, &group.ActionSource, &group.ActionRuleID, &group.ActionRuleName,
			&group.AccountID, &group.AccountEmail, &group.Accounts, &group.Messages, &group.Jobs,
			&group.Failed, &group.Parked, &group.Blocked, &group.Active,
			&group.FolderID, &group.FolderName, &group.FolderRole,
			&group.DestinationFolderID, &group.DestinationName, &group.DestinationRole,
			&group.LabelName, &createdAt, &updatedAt,
		); err != nil {
			return nil, err
		}
		if createdAt.Valid {
			group.CreatedAt = createdAt.Time
		}
		if updatedAt.Valid {
			group.UpdatedAt = updatedAt.Time
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

// ListUngroupedMailOperationsForUser lists the user's operations that have no
// recorded action: drafts, sent copies, and changes queued before Gofer
// recorded actions. It is ordered as ListMailOperationsForUser.
func (db *DB) ListUngroupedMailOperationsForUser(ctx context.Context, userID string) ([]models.MailOperationSummary, error) {
	return db.listMailOperations(ctx, `
WHERE action_id = '' AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)
ORDER BY (can_retry OR can_reconcile OR can_cancel OR state = 'blocked') DESC, updated_at DESC
LIMIT ?`, strings.TrimSpace(userID), mailOperationListLimit)
}

func (db *DB) listMailOperations(ctx context.Context, where string, args ...any) ([]models.MailOperationSummary, error) {
	rows, err := db.Read().QueryContext(ctx, db.mailOperationProjection()+where, mailOperationArgs(args...)...)
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

// extraColumnsScanner scans a projection row followed by more columns.
type extraColumnsScanner struct {
	row   mailOperationRowScanner
	extra []any
}

func (s extraColumnsScanner) Scan(dest ...any) error {
	return s.row.Scan(append(dest, s.extra...)...)
}

// ListMailActionMessagesForUser lists the changes one action queued, with the
// subject and sender of each message, those that need attention first. At
// most limit are listed. An action of another user lists nothing.
func (db *DB) ListMailActionMessagesForUser(ctx context.Context, userID, actionID string, limit int) ([]models.MailOperationGroupMessage, error) {
	actionID = strings.TrimSpace(actionID)
	if actionID == "" {
		return []models.MailOperationGroupMessage{}, nil
	}
	rows, err := db.Read().QueryContext(ctx, `
SELECT p.*, COALESCE(m.subject, ''), COALESCE(m.from_name, ''), COALESCE(m.from_email, ''),
       COALESCE(sf.name, ''), COALESCE(sf.role, ''), COALESCE(df.name, ''), COALESCE(df.role, '')
FROM (`+db.mailOperationProjection()+`
    WHERE action_id = ? AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)
) p
LEFT JOIN messages m ON m.id = p.message_id
LEFT JOIN folders sf ON sf.id = p.folder_id
LEFT JOIN folders df ON df.id = p.destination_folder_id
ORDER BY (p.can_retry = 1 OR p.next_retry_at = ? OR p.state = 'blocked') DESC, p.message_id, p.operation_id
LIMIT ?`, mailOperationArgs(actionID, strings.TrimSpace(userID), parkedRetryAt, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := make([]models.MailOperationGroupMessage, 0)
	for rows.Next() {
		var message models.MailOperationGroupMessage
		operation, err := scanMailOperation(extraColumnsScanner{row: rows, extra: []any{
			&message.Subject, &message.FromName, &message.FromEmail,
			&message.FolderName, &message.FolderRole, &message.DestinationName, &message.DestinationRole,
		}})
		if err != nil {
			return nil, err
		}
		message.Operation = operation
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

// MailActionAccountsForUser lists the user's accounts that still have
// changes queued by the action, so each can be acted on inside its own
// account guard.
func (db *DB) MailActionAccountsForUser(ctx context.Context, userID, actionID string) ([]string, error) {
	rows, err := db.Read().QueryContext(ctx, `
SELECT account_id FROM message_mutations WHERE action_id = ? AND status != ?
  AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)
UNION
SELECT account_id FROM label_mutation_queue WHERE action_id = ?
  AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)
ORDER BY account_id`,
		strings.TrimSpace(actionID), MessageMutationApplied, strings.TrimSpace(userID),
		strings.TrimSpace(actionID), strings.TrimSpace(userID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accounts := []string{}
	for rows.Next() {
		var account string
		if err := rows.Scan(&account); err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

// RetryMailActionForUser schedules every failed change the action queued in
// one of the user's accounts to be tried again now, in one transaction.
// Parked changes are left alone: retrying can't help them. guard runs before
// anything changes and again before committing. It returns how many changes
// will be tried again.
func (db *DB) RetryMailActionForUser(ctx context.Context, userID, actionID, accountID string, guard func(*sql.Tx) error) (int, error) {
	userID, actionID, accountID = strings.TrimSpace(userID), strings.TrimSpace(actionID), strings.TrimSpace(accountID)
	if actionID == "" || accountID == "" {
		return 0, nil
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if guard != nil {
		if err := guard(tx); err != nil {
			return 0, err
		}
	}
	messages, err := tx.ExecContext(ctx, `
		UPDATE message_mutations
		SET status = ?, locked_at = NULL, next_attempt_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE action_id = ? AND account_id = ? AND status = ? AND next_attempt_at != ?
		  AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)`,
		MessageMutationPending, actionID, accountID, MessageMutationFailed, parkedRetryAt, userID)
	if err != nil {
		return 0, fmt.Errorf("retry message changes: %w", err)
	}
	// Label changes are failed once they have been tried, as the list shows
	// them.
	labels, err := tx.ExecContext(ctx, `
		UPDATE label_mutation_queue
		SET next_attempt_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE action_id = ? AND account_id = ? AND (attempts > 0 OR COALESCE(last_error, '') != '')
		  AND next_attempt_at != ?
		  AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)`,
		actionID, accountID, parkedRetryAt, userID)
	if err != nil {
		return 0, fmt.Errorf("retry label changes: %w", err)
	}
	messageCount, err := messages.RowsAffected()
	if err != nil {
		return 0, err
	}
	labelCount, err := labels.RowsAffected()
	if err != nil {
		return 0, err
	}
	if guard != nil {
		if err := guard(tx); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(messageCount + labelCount), nil
}

// UndoMailActionForUser undoes, in Gofer only, every parked change the action
// queued in one of the user's accounts, as UndoMailOperationForUser does one,
// in one transaction. Changes that can still finish are left alone. guard
// runs before anything changes. It returns how many changes were undone.
func (db *DB) UndoMailActionForUser(ctx context.Context, userID, actionID, accountID string, guard func(*sql.Tx) error) (int, error) {
	userID, actionID, accountID = strings.TrimSpace(userID), strings.TrimSpace(actionID), strings.TrimSpace(accountID)
	if actionID == "" || accountID == "" {
		return 0, nil
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if guard != nil {
		if err := guard(tx); err != nil {
			return 0, err
		}
	}
	messageIDs, err := queryStringsTx(ctx, tx, `
		SELECT id FROM message_mutations
		WHERE action_id = ? AND account_id = ? AND status != ? AND next_attempt_at = ?
		  AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)
		ORDER BY id`, actionID, accountID, MessageMutationApplied, parkedRetryAt, userID)
	if err != nil {
		return 0, fmt.Errorf("load parked message changes: %w", err)
	}
	labelIDs, err := queryStringsTx(ctx, tx, `
		SELECT CAST(id AS TEXT) FROM label_mutation_queue
		WHERE action_id = ? AND account_id = ? AND next_attempt_at = ?
		  AND account_id IN (SELECT id FROM accounts WHERE user_id = ?)
		ORDER BY id`, actionID, accountID, parkedRetryAt, userID)
	if err != nil {
		return 0, fmt.Errorf("load parked label changes: %w", err)
	}
	refresh := map[string]struct{}{}
	for _, id := range labelIDs {
		if err := db.undoLabelMutationTx(ctx, tx, id, refresh); err != nil {
			return 0, err
		}
	}
	// Message changes go last: the last one for a message removes a message
	// left in no folder, which the label changes above no longer need.
	for _, id := range messageIDs {
		if err := db.undoMessageMutationTx(ctx, tx, id, refresh); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for folderID := range refresh {
		_, _ = db.RefreshFolderUnreadCount(ctx, folderID)
		_ = db.RefreshFolderThreadState(ctx, folderID)
	}
	return len(messageIDs) + len(labelIDs), nil
}

func queryStringsTx(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// MailOperationsAttentionForUser counts what in the user's Mail operations
// list needs attention, the way the list counts it: an action whose changes
// failed, can't be completed or are blocked counts once, and so does each
// such change with no recorded action. fingerprint changes whenever the count
// or the latest of those changes does, so a notice can tell new problems
// from ones already shown. It reads only; nothing is written.
func (db *DB) MailOperationsAttentionForUser(ctx context.Context, userID string) (count int, fingerprint string, err error) {
	var latest sql.NullString
	err = db.Read().QueryRowContext(ctx, `
SELECT COUNT(DISTINCT CASE WHEN action_id != '' THEN 'a:' || action_id ELSE 'o:' || operation_id END),
       MAX(COALESCE(CAST(updated_at AS TEXT), CAST(created_at AS TEXT), ''))
FROM (`+db.mailOperationProjection()+`
    WHERE account_id IN (SELECT id FROM accounts WHERE user_id = ?)
      AND (can_retry = 1 OR can_reconcile = 1 OR can_cancel = 1 OR state = 'blocked' OR next_retry_at = ?)
) p`, mailOperationArgs(strings.TrimSpace(userID), parkedRetryAt)...).Scan(&count, &latest)
	if err != nil {
		return 0, "", err
	}
	if count == 0 {
		return 0, "", nil
	}
	return count, fmt.Sprintf("%d|%s", count, latest.String), nil
}
