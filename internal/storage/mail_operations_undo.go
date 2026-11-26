package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// UndoMailOperationForUser drops a parked operation, one that can never
// finish, and puts Gofer's own view of the message back to how it was before
// the change. Nothing is sent to the server: the server never got the change,
// so its copy already looks like this. Only parked message and label changes
// can be undone; anything else is ErrMailOperationNotUndoable. An unknown
// operation, or another user's, is sql.ErrNoRows. guard runs inside the
// transaction before anything changes. The returned summary is the operation
// as it was before the undo.
func (db *DB) UndoMailOperationForUser(ctx context.Context, userID, operationID string, guard func(*sql.Tx) error) (models.MailOperationSummary, error) {
	userID = strings.TrimSpace(userID)
	_, rawID, ok := strings.Cut(strings.TrimSpace(operationID), ":")
	if !ok || strings.TrimSpace(rawID) == "" {
		return models.MailOperationSummary{}, ErrMailOperationNotUndoable
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return models.MailOperationSummary{}, err
	}
	defer tx.Rollback()
	if guard != nil {
		if err := guard(tx); err != nil {
			return models.MailOperationSummary{}, err
		}
	}
	// Read inside the write transaction, so the operation can't change
	// between the check and the undo.
	operation, err := db.mailOperationForUserTx(ctx, tx, userID, operationID)
	if err != nil {
		return models.MailOperationSummary{}, err
	}
	if !operation.CanUndo {
		return models.MailOperationSummary{}, ErrMailOperationNotUndoable
	}
	refresh := map[string]struct{}{}
	switch operation.Type {
	case models.MailOperationMessageMutation:
		err = db.undoMessageMutationTx(ctx, tx, rawID, refresh)
	case models.MailOperationLabelMutation:
		err = db.undoLabelMutationTx(ctx, tx, rawID, refresh)
	default:
		err = ErrMailOperationNotUndoable
	}
	if err != nil {
		return models.MailOperationSummary{}, err
	}
	if err := tx.Commit(); err != nil {
		return models.MailOperationSummary{}, err
	}
	for folderID := range refresh {
		_, _ = db.RefreshFolderUnreadCount(ctx, folderID)
		_ = db.RefreshFolderThreadState(ctx, folderID)
	}
	return operation, nil
}

// undoMessageMutationTx reverses what queueing the change did to Gofer's
// copy of the message, then removes the change. A message left in no folder
// is removed, as folder cleanup would have done without the change.
func (db *DB) undoMessageMutationTx(ctx context.Context, tx *sql.Tx, id string, refresh map[string]struct{}) error {
	var messageID int64
	var folderID, destinationFolderID, providerType, kind string
	var target int
	if err := tx.QueryRowContext(ctx, `
		SELECT message_id, folder_id, destination_folder_id, provider_type, kind, target_value
		FROM message_mutations WHERE id = ?`, id).
		Scan(&messageID, &folderID, &destinationFolderID, &providerType, &kind, &target); err != nil {
		return fmt.Errorf("load message change: %w", err)
	}
	if err := collectMessageFoldersTx(ctx, tx, messageID, refresh); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_mutations WHERE id = ?`, id); err != nil {
		return fmt.Errorf("remove message change: %w", err)
	}
	switch kind {
	case MessageMutationMove:
		// Show the message where it was, if Gofer still has that folder. A
		// move that continued earlier moves started there too, so folders
		// it passed through on the way stay hidden.
		if _, err := tx.ExecContext(ctx, `
			UPDATE message_folder_state SET is_deleted = 0, synced_at = CURRENT_TIMESTAMP
			WHERE message_id = ? AND folder_id = ? AND is_deleted = 1
			  AND EXISTS (SELECT 1 FROM folders WHERE id = ? AND discovery_state != 'removed')`, messageID, folderID, folderID); err != nil {
			return fmt.Errorf("show message in its source folder: %w", err)
		}
		// Gofer put it in the destination before the server did; the server
		// never did.
		if destinationFolderID != "" && destinationFolderID != folderID {
			if _, err := tx.ExecContext(ctx, `DELETE FROM message_folder_copies WHERE message_id = ? AND folder_id = ?`, messageID, destinationFolderID); err != nil {
				return fmt.Errorf("remove destination copies: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM message_folder_state WHERE message_id = ? AND folder_id = ?`, messageID, destinationFolderID); err != nil {
				return fmt.Errorf("remove message from destination folder: %w", err)
			}
		}
	case MessageMutationDelete:
		if providerType == MessageMutationProviderIMAP {
			if _, err := tx.ExecContext(ctx, `
				UPDATE message_folder_state SET is_deleted = 0, synced_at = CURRENT_TIMESTAMP
				WHERE message_id = ? AND folder_id = ?`, messageID, folderID); err != nil {
				return fmt.Errorf("show deleted message again: %w", err)
			}
		} else {
			// The delete hid every folder showing the same provider copy.
			// Folders with a delete of their own still queued stay hidden.
			copyIDs := `SELECT ` + providerCopyIDSQL("source") + ` FROM message_folder_state source WHERE source.message_id = ? AND source.folder_id = ?
				UNION SELECT remote_message_id FROM message_folder_copies WHERE message_id = ? AND folder_id = ?`
			if _, err := tx.ExecContext(ctx, `
				UPDATE message_folder_state SET is_deleted = 0, synced_at = CURRENT_TIMESTAMP
				WHERE message_id = ? AND `+providerCopyIDSQL("message_folder_state")+` IN (`+copyIDs+`)
				  AND folder_id NOT IN (SELECT folder_id FROM message_mutations WHERE message_id = ? AND kind = ?)`,
				messageID, messageID, folderID, messageID, folderID, messageID, MessageMutationDelete); err != nil {
				return fmt.Errorf("show deleted message again: %w", err)
			}
		}
	case MessageMutationRead, MessageMutationStarred:
		// The same change may still be on its way through another folder;
		// then Gofer keeps showing it.
		var others int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_mutations WHERE message_id = ? AND kind = ?`, messageID, kind).Scan(&others); err != nil {
			return err
		}
		if others == 0 {
			column := "is_read"
			if kind == MessageMutationStarred {
				column = "is_starred"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE message_folder_state SET `+column+` = ? WHERE message_id = ?`, boolInt(target == 0), messageID); err != nil {
				return fmt.Errorf("restore %s state: %w", kind, err)
			}
		}
	default:
		return ErrMailOperationNotUndoable
	}
	if err := collectMessageFoldersTx(ctx, tx, messageID, refresh); err != nil {
		return err
	}
	if kind != MessageMutationMove && kind != MessageMutationDelete {
		return nil
	}
	// Like folder cleanup, keep a message other queued work still needs.
	var queued bool
	if err := tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM message_mutations WHERE message_id = ?) OR
		EXISTS(SELECT 1 FROM label_mutation_queue WHERE message_id = ?) OR
		EXISTS(SELECT 1 FROM imap_draft_states WHERE local_message_id = ?)`, messageID, messageID, messageID).Scan(&queued); err != nil {
		return err
	}
	if queued {
		return nil
	}
	return db.deleteFolderlessMessagesTx(ctx, tx, []int64{messageID})
}

// undoLabelMutationTx applies the opposite of a queued label change to
// Gofer's copy of the message, then removes the change.
func (db *DB) undoLabelMutationTx(ctx context.Context, tx *sql.Tx, rawID string, refresh map[string]struct{}) error {
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 {
		return ErrMailOperationNotUndoable
	}
	var accountID, folderID, providerType, operation, name string
	var messageID int64
	if err := tx.QueryRowContext(ctx, `
		SELECT account_id, message_id, folder_id, provider_type, operation, label_name
		FROM label_mutation_queue WHERE id = ?`, id).
		Scan(&accountID, &messageID, &folderID, &providerType, &operation, &name); err != nil {
		return fmt.Errorf("load label change: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM label_mutation_queue WHERE id = ?`, id); err != nil {
		return fmt.Errorf("remove label change: %w", err)
	}
	if operation == LabelMutationAdd {
		if _, err := tx.ExecContext(ctx, `DELETE FROM message_labels WHERE message_id = ? AND label_id IN (SELECT id FROM labels WHERE account_id = ? AND lower(name) = lower(?) AND provider_type IN (?, ?))`,
			messageID, accountID, name, providerType, LabelProviderLocal); err != nil {
			return fmt.Errorf("remove label: %w", err)
		}
	} else {
		// Put back the way queueing an added label shows it.
		if err := db.addMessageLabelsTx(ctx, tx, messageID, accountID, []LabelInput{{AccountID: accountID, ProviderType: LabelProviderLocal, Name: name}}); err != nil {
			return fmt.Errorf("restore label: %w", err)
		}
	}
	if folderID != "" {
		refresh[folderID] = struct{}{}
	}
	return nil
}

// collectMessageFoldersTx adds the folders showing the message to folders.
func collectMessageFoldersTx(ctx context.Context, tx *sql.Tx, messageID int64, folders map[string]struct{}) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT folder_id FROM message_folder_state WHERE message_id = ?`, messageID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var folderID string
		if err := rows.Scan(&folderID); err != nil {
			return err
		}
		folders[folderID] = struct{}{}
	}
	return rows.Err()
}
