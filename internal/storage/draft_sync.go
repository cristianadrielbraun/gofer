package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const (
	IMAPDraftOperationUpsert = "upsert"
	IMAPDraftOperationDelete = "delete"
)

const (
	IMAPDraftStatusPending   = "pending"
	IMAPDraftStatusSyncing   = "syncing"
	IMAPDraftStatusFailed    = "failed"
	IMAPDraftStatusAmbiguous = "ambiguous"
)

type IMAPDraftState struct {
	AccountID        string
	DraftKey         string
	LocalMessageID   int64
	FolderID         string
	FolderRemoteName string
	RemoteUID        uint32
	UIDValidity      uint32
}

type IMAPDraftOperation struct {
	ID            string
	State         IMAPDraftState
	Kind          string
	RevisionToken string
	MIMEData      []byte
	MessageDate   time.Time
	Status        string
	AttemptCount  int
	LastError     string
	NextAttemptAt time.Time
}

type QueueIMAPDraftUpsertInput struct {
	State         IMAPDraftState
	RevisionToken string
	MIMEData      []byte
	MessageDate   time.Time
}

func (db *DB) QueueIMAPDraftUpsert(ctx context.Context, input QueueIMAPDraftUpsertInput) (IMAPDraftOperation, error) {
	if input.State.AccountID == "" || input.State.DraftKey == "" || input.State.FolderRemoteName == "" || input.RevisionToken == "" || len(input.MIMEData) == 0 {
		return IMAPDraftOperation{}, fmt.Errorf("missing IMAP draft upsert payload")
	}
	if input.MessageDate.IsZero() {
		input.MessageDate = time.Now().UTC()
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return IMAPDraftOperation{}, err
	}
	defer tx.Rollback()
	if err := upsertIMAPDraftStateTx(ctx, tx, input.State); err != nil {
		return IMAPDraftOperation{}, err
	}

	id, err := coalescibleIMAPDraftOperationIDTx(ctx, tx, input.State.AccountID, input.State.DraftKey)
	if err != nil {
		return IMAPDraftOperation{}, err
	}
	if id == "" {
		id = uuid.NewString()
		_, err = tx.ExecContext(ctx, `
			INSERT INTO imap_draft_operations (
				id, account_id, draft_key, kind, revision_token, mime_data, message_date,
				status, attempt_count, last_error, locked_at, next_attempt_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, '', NULL, CURRENT_TIMESTAMP)`,
			id, input.State.AccountID, input.State.DraftKey, IMAPDraftOperationUpsert,
			input.RevisionToken, input.MIMEData, input.MessageDate.UTC(), IMAPDraftStatusPending)
	} else {
		_, err = tx.ExecContext(ctx, `
			UPDATE imap_draft_operations
			SET kind = ?, revision_token = ?, mime_data = ?, message_date = ?,
				status = ?, attempt_count = 0, last_error = '', locked_at = NULL,
				next_attempt_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
			WHERE id = ?`, IMAPDraftOperationUpsert, input.RevisionToken, input.MIMEData,
			input.MessageDate.UTC(), IMAPDraftStatusPending, id)
	}
	if err != nil {
		return IMAPDraftOperation{}, fmt.Errorf("queue IMAP draft upsert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return IMAPDraftOperation{}, err
	}
	return db.GetIMAPDraftOperation(ctx, id)
}

func (db *DB) QueueIMAPDraftDelete(ctx context.Context, state IMAPDraftState) (IMAPDraftOperation, error) {
	if state.AccountID == "" || state.DraftKey == "" || state.FolderRemoteName == "" {
		return IMAPDraftOperation{}, fmt.Errorf("missing IMAP draft delete identity")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return IMAPDraftOperation{}, err
	}
	defer tx.Rollback()
	if err := upsertIMAPDraftStateTx(ctx, tx, state); err != nil {
		return IMAPDraftOperation{}, err
	}
	id, err := coalescibleIMAPDraftOperationIDTx(ctx, tx, state.AccountID, state.DraftKey)
	if err != nil {
		return IMAPDraftOperation{}, err
	}
	if id == "" {
		id = uuid.NewString()
		_, err = tx.ExecContext(ctx, `
			INSERT INTO imap_draft_operations (
				id, account_id, draft_key, kind, revision_token, mime_data, message_date,
				status, attempt_count, last_error, locked_at, next_attempt_at
			) VALUES (?, ?, ?, ?, '', NULL, NULL, ?, 0, '', NULL, CURRENT_TIMESTAMP)`,
			id, state.AccountID, state.DraftKey, IMAPDraftOperationDelete, IMAPDraftStatusPending)
	} else {
		_, err = tx.ExecContext(ctx, `
			UPDATE imap_draft_operations
			SET kind = ?, revision_token = '', mime_data = NULL, message_date = NULL,
				status = ?, attempt_count = 0, last_error = '', locked_at = NULL,
				next_attempt_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
			WHERE id = ?`, IMAPDraftOperationDelete, IMAPDraftStatusPending, id)
	}
	if err != nil {
		return IMAPDraftOperation{}, fmt.Errorf("queue IMAP draft delete: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return IMAPDraftOperation{}, err
	}
	return db.GetIMAPDraftOperation(ctx, id)
}

func upsertIMAPDraftStateTx(ctx context.Context, tx *sql.Tx, state IMAPDraftState) error {
	var localMessageID any
	if state.LocalMessageID > 0 {
		localMessageID = state.LocalMessageID
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO imap_draft_states (
			account_id, draft_key, local_message_id, folder_id, folder_remote_name, remote_uid, uid_validity
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, draft_key) DO UPDATE SET
			local_message_id = COALESCE(excluded.local_message_id, imap_draft_states.local_message_id),
			folder_id = CASE WHEN excluded.folder_id != '' THEN excluded.folder_id ELSE imap_draft_states.folder_id END,
			folder_remote_name = excluded.folder_remote_name,
			remote_uid = CASE WHEN excluded.remote_uid > 0 THEN excluded.remote_uid ELSE imap_draft_states.remote_uid END,
			uid_validity = CASE WHEN excluded.uid_validity > 0 THEN excluded.uid_validity ELSE imap_draft_states.uid_validity END,
			updated_at = CURRENT_TIMESTAMP`,
		state.AccountID, state.DraftKey, localMessageID, state.FolderID, state.FolderRemoteName, state.RemoteUID, state.UIDValidity)
	return err
}

func coalescibleIMAPDraftOperationIDTx(ctx context.Context, tx *sql.Tx, accountID, draftKey string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM imap_draft_operations
		WHERE account_id = ? AND draft_key = ? AND status IN (?, ?)
		ORDER BY created_at ASC LIMIT 1`, accountID, draftKey, IMAPDraftStatusPending, IMAPDraftStatusFailed).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (db *DB) GetIMAPDraftOperation(ctx context.Context, id string) (IMAPDraftOperation, error) {
	return scanIMAPDraftOperation(db.Read().QueryRowContext(ctx, imapDraftOperationSelect+` WHERE o.id = ?`, id))
}

func (db *DB) GetIMAPDraftState(ctx context.Context, accountID, draftKey string) (*IMAPDraftState, error) {
	var state IMAPDraftState
	var localMessageID sql.NullInt64
	var remoteUID, uidValidity int64
	err := db.Read().QueryRowContext(ctx, `
		SELECT account_id, draft_key, local_message_id, folder_id, folder_remote_name, remote_uid, uid_validity
		FROM imap_draft_states WHERE account_id = ? AND draft_key = ?`, accountID, draftKey,
	).Scan(&state.AccountID, &state.DraftKey, &localMessageID, &state.FolderID, &state.FolderRemoteName, &remoteUID, &uidValidity)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if localMessageID.Valid {
		state.LocalMessageID = localMessageID.Int64
	}
	if remoteUID > 0 {
		state.RemoteUID = uint32(remoteUID)
	}
	if uidValidity > 0 {
		state.UIDValidity = uint32(uidValidity)
	}
	return &state, nil
}

func (db *DB) ClaimDueIMAPDraftOperations(ctx context.Context, now time.Time, limit int) ([]IMAPDraftOperation, error) {
	if limit <= 0 {
		limit = 10
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, imapDraftOperationSelect+`
		WHERE o.status IN (?, ?, ?)
		  AND o.next_attempt_at <= ?
		  AND NOT EXISTS (
			SELECT 1 FROM imap_draft_operations earlier
			WHERE earlier.account_id = o.account_id AND earlier.draft_key = o.draft_key
			  AND earlier.rowid < o.rowid
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM imap_draft_operations active
			WHERE active.account_id = o.account_id AND active.draft_key = o.draft_key
			  AND active.status = ? AND active.id != o.id
		  )
		ORDER BY o.created_at ASC
		LIMIT ?`, IMAPDraftStatusPending, IMAPDraftStatusFailed, IMAPDraftStatusAmbiguous, now.UTC(), IMAPDraftStatusSyncing, limit)
	if err != nil {
		return nil, err
	}
	var operations []IMAPDraftOperation
	for rows.Next() {
		op, err := scanIMAPDraftOperation(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		operations = append(operations, op)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range operations {
		result, err := tx.ExecContext(ctx, `
			UPDATE imap_draft_operations
			SET status = ?, locked_at = ?, attempt_count = attempt_count + 1, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND status = ?`, IMAPDraftStatusSyncing, now.UTC(), operations[i].ID, operations[i].Status)
		if err != nil {
			return nil, err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return nil, fmt.Errorf("IMAP draft operation %s was claimed concurrently", operations[i].ID)
		}
		operations[i].AttemptCount++
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return operations, nil
}

func (db *DB) FinishIMAPDraftOperationWithError(ctx context.Context, id, status, errText string, nextAttempt time.Time) error {
	if status != IMAPDraftStatusAmbiguous {
		status = IMAPDraftStatusFailed
	}
	_, err := db.Write().ExecContext(ctx, `
		UPDATE imap_draft_operations
		SET status = ?, last_error = ?, locked_at = NULL, next_attempt_at = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ?`, status, errText, nextAttempt.UTC(), id, IMAPDraftStatusSyncing)
	return err
}

func (db *DB) MarkInterruptedIMAPDraftOperationsAmbiguous(ctx context.Context, reason string) (int64, error) {
	result, err := db.Write().ExecContext(ctx, `
		UPDATE imap_draft_operations
		SET status = ?, last_error = ?, locked_at = NULL, next_attempt_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE status = ?`, IMAPDraftStatusAmbiguous, reason, IMAPDraftStatusSyncing)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (db *DB) CompleteIMAPDraftUpsert(ctx context.Context, id string, remoteUID, uidValidity uint32) (IMAPDraftState, error) {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return IMAPDraftState{}, err
	}
	defer tx.Rollback()
	var state IMAPDraftState
	var localMessageID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT s.account_id, s.draft_key, s.local_message_id, s.folder_id, s.folder_remote_name
		FROM imap_draft_operations o
		JOIN imap_draft_states s ON s.account_id = o.account_id AND s.draft_key = o.draft_key
		WHERE o.id = ? AND o.status = ?`, id, IMAPDraftStatusSyncing,
	).Scan(&state.AccountID, &state.DraftKey, &localMessageID, &state.FolderID, &state.FolderRemoteName); err != nil {
		return IMAPDraftState{}, err
	}
	if localMessageID.Valid {
		state.LocalMessageID = localMessageID.Int64
	}
	state.RemoteUID = remoteUID
	state.UIDValidity = uidValidity
	if _, err := tx.ExecContext(ctx, `
		UPDATE imap_draft_states
		SET remote_uid = ?, uid_validity = ?, updated_at = CURRENT_TIMESTAMP
		WHERE account_id = ? AND draft_key = ?`, remoteUID, uidValidity, state.AccountID, state.DraftKey); err != nil {
		return IMAPDraftState{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM imap_draft_operations WHERE id = ?`, id); err != nil {
		return IMAPDraftState{}, err
	}
	if err := tx.Commit(); err != nil {
		return IMAPDraftState{}, err
	}
	return state, nil
}

func (db *DB) CompleteIMAPDraftDelete(ctx context.Context, id string) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var accountID, draftKey string
	if err := tx.QueryRowContext(ctx, `
		SELECT account_id, draft_key FROM imap_draft_operations
		WHERE id = ? AND status = ?`, id, IMAPDraftStatusSyncing).Scan(&accountID, &draftKey); err != nil {
		return err
	}
	var newer int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM imap_draft_operations
		WHERE account_id = ? AND draft_key = ? AND id != ?`, accountID, draftKey, id).Scan(&newer); err != nil {
		return err
	}
	if newer == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM imap_draft_states WHERE account_id = ? AND draft_key = ?`, accountID, draftKey); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			UPDATE imap_draft_states SET remote_uid = 0, uid_validity = 0, updated_at = CURRENT_TIMESTAMP
			WHERE account_id = ? AND draft_key = ?`, accountID, draftKey); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM imap_draft_operations WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func hasActiveIMAPDraftOperationTx(ctx context.Context, tx *sql.Tx, accountID, draftKey string) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM imap_draft_operations
		WHERE account_id = ? AND draft_key = ?`, accountID, draftKey).Scan(&count)
	return count > 0, err
}

const imapDraftOperationSelect = `SELECT
	o.id, o.account_id, o.draft_key, s.local_message_id, s.folder_id, s.folder_remote_name,
	s.remote_uid, s.uid_validity, o.kind, o.revision_token, o.mime_data, o.message_date,
	o.status, o.attempt_count, o.last_error, o.next_attempt_at
	FROM imap_draft_operations o
	JOIN imap_draft_states s ON s.account_id = o.account_id AND s.draft_key = o.draft_key`

func scanIMAPDraftOperation(row rowScanner) (IMAPDraftOperation, error) {
	var op IMAPDraftOperation
	var localMessageID sql.NullInt64
	var remoteUID, uidValidity int64
	var messageDate, nextAttempt sqliteNullTime
	if err := row.Scan(
		&op.ID, &op.State.AccountID, &op.State.DraftKey, &localMessageID, &op.State.FolderID, &op.State.FolderRemoteName,
		&remoteUID, &uidValidity, &op.Kind, &op.RevisionToken, &op.MIMEData, &messageDate,
		&op.Status, &op.AttemptCount, &op.LastError, &nextAttempt,
	); err != nil {
		return IMAPDraftOperation{}, err
	}
	if localMessageID.Valid {
		op.State.LocalMessageID = localMessageID.Int64
	}
	if remoteUID > 0 {
		op.State.RemoteUID = uint32(remoteUID)
	}
	if uidValidity > 0 {
		op.State.UIDValidity = uint32(uidValidity)
	}
	if messageDate.Valid {
		op.MessageDate = messageDate.Time
	}
	if nextAttempt.Valid {
		op.NextAttemptAt = nextAttempt.Time
	}
	return op, nil
}
