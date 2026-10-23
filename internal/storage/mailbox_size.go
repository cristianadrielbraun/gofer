package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// GetMailboxSize adds up the sizes the mail server reported for the owner's
// account, with its folders as the sidebar shows them. It returns
// sql.ErrNoRows for an account the owner doesn't have.
func (db *DB) GetMailboxSize(ctx context.Context, userID, accountID string) (models.MailboxSize, error) {
	var size models.MailboxSize
	err := db.Read().QueryRowContext(ctx,
		`SELECT id, email_address, display_name FROM accounts
		 WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0`, accountID, userID,
	).Scan(&size.Account.ID, &size.Account.Email, &size.Account.Name)
	if err != nil {
		return size, err
	}
	if size.Account.Folders, err = db.getFolders(ctx, accountID); err != nil {
		return size, err
	}

	// Sizes are 0 while unknown and -1 when the server can't report them.
	err = db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN m.size_bytes > 0 THEN m.size_bytes END), 0),
		        COALESCE(SUM(m.size_bytes <= 0), 0)
		 FROM messages m
		 WHERE m.account_id = ?
		   AND EXISTS (SELECT 1 FROM message_folder_state s WHERE s.message_id = m.id AND s.is_deleted = 0)`, accountID,
	).Scan(&size.Total.Messages, &size.Total.Bytes, &size.Total.Unmeasured)
	if err != nil {
		return size, fmt.Errorf("mailbox size: %w", err)
	}

	rows, err := db.Read().QueryContext(ctx,
		`SELECT s.folder_id, COUNT(*), COALESCE(SUM(CASE WHEN m.size_bytes > 0 THEN m.size_bytes END), 0),
		        COALESCE(SUM(m.size_bytes <= 0), 0)
		 FROM folders f
		 JOIN message_folder_state s ON s.folder_id = f.id
		 JOIN messages m ON m.id = s.message_id
		 WHERE f.account_id = ? AND s.is_deleted = 0
		 GROUP BY s.folder_id`, accountID)
	if err != nil {
		return size, fmt.Errorf("folder sizes: %w", err)
	}
	defer rows.Close()
	size.Folders = make(map[string]models.MailboxSizeCount)
	for rows.Next() {
		var folderID string
		var count models.MailboxSizeCount
		if err := rows.Scan(&folderID, &count.Messages, &count.Bytes, &count.Unmeasured); err != nil {
			return size, fmt.Errorf("scan folder size: %w", err)
		}
		size.Folders[folderID] = count
	}
	return size, rows.Err()
}

// AddLocalMailboxSizes fills in what Gofer keeps on this device for the
// account, given the bytes of each message's files by message ID.
func (db *DB) AddLocalMailboxSizes(ctx context.Context, size *models.MailboxSize, files map[int64]int64) error {
	size.HasLocal = true
	size.Total.LocalBytes = 0
	size.LocalOutsideFolders = 0
	if len(files) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(files))
	for id, bytes := range files {
		ids = append(ids, id)
		size.Total.LocalBytes += bytes
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	rows, err := db.Read().QueryContext(ctx,
		`SELECT s.message_id, s.folder_id
		 FROM message_folder_state s
		 JOIN messages m ON m.id = s.message_id
		 WHERE m.account_id = ? AND s.is_deleted = 0
		   AND s.message_id IN (SELECT value FROM json_each(?))`, size.Account.ID, string(encoded))
	if err != nil {
		return fmt.Errorf("local folder sizes: %w", err)
	}
	defer rows.Close()
	filed := make(map[int64]bool, len(files))
	for rows.Next() {
		var messageID int64
		var folderID string
		if err := rows.Scan(&messageID, &folderID); err != nil {
			return fmt.Errorf("scan local folder size: %w", err)
		}
		filed[messageID] = true
		count := size.Folders[folderID]
		count.LocalBytes += files[messageID]
		size.Folders[folderID] = count
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id, bytes := range files {
		if !filed[id] {
			size.LocalOutsideFolders += bytes
		}
	}
	return nil
}
