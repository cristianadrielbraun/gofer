package storage

import (
	"context"
	"strings"
)

// folderPlaceOrderSQL orders the folders a message is filed in, most telling
// first: Inbox, Sent and Archive before the user's own folders, and Spam and
// Trash last. alias is the folders table.
func folderPlaceOrderSQL(alias string) string {
	return `CASE ` + alias + `.role
				WHEN 'inbox' THEN 0 WHEN 'sent' THEN 1 WHEN 'archive' THEN 2
				WHEN 'custom' THEN 3 WHEN 'drafts' THEN 4 WHEN 'spam' THEN 5
				WHEN 'junk' THEN 5 WHEN 'trash' THEN 6 ELSE 7 END,
				` + alias + `.sort_order, ` + alias + `.name, ` + alias + `.id`
}

// ThreadItemFolder is a folder a conversation's message is filed in.
type ThreadItemFolder struct {
	Name, Role string
}

// ThreadItemFoldersElsewhere names, for each of the conversation's messages
// that isn't filed where the list is, the folder it is filed in instead, so
// an expanded conversation can say where a reply in Sent lives. here is the
// list's folder, or a unified folder such as "inbox", which any folder of
// that role counts as. Lists that aren't a folder, like Starred, name none.
func (db *DB) ThreadItemFoldersElsewhere(ctx context.Context, userID, here string, messageIDs []string) (map[string]ThreadItemFolder, error) {
	here = strings.TrimSpace(here)
	if here == "" || len(messageIDs) == 0 || here == "starred" || here == "scheduled" {
		return nil, nil
	}
	unified := isUnifiedFolderID(here)
	if !unified {
		var known bool
		if err := db.Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM folders f JOIN accounts a ON a.id = f.account_id
			WHERE f.id = ? AND a.user_id = ?)`, here, userID).Scan(&known); err != nil || !known {
			return nil, err
		}
	}
	args := []any{userID}
	marks := make([]string, 0, len(messageIDs))
	for _, id := range messageIDs {
		marks = append(marks, "?")
		args = append(args, id)
	}
	rows, err := db.Read().QueryContext(ctx, `SELECT s.message_id, f.id, f.name, COALESCE(f.role, '')
		FROM message_folder_state s
		JOIN folders f ON f.id = s.folder_id
		JOIN accounts a ON a.id = f.account_id
		WHERE s.is_deleted = 0 AND a.user_id = ? AND s.message_id IN (`+strings.Join(marks, ", ")+`)
		ORDER BY s.message_id, `+folderPlaceOrderSQL("f"), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	first := make(map[string]ThreadItemFolder)
	inHere := make(map[string]bool)
	for rows.Next() {
		var messageID, folderID string
		var folder ThreadItemFolder
		if err := rows.Scan(&messageID, &folderID, &folder.Name, &folder.Role); err != nil {
			return nil, err
		}
		if folderID == here || unified && unifiedFolderIDFromRole(folder.Role) == here {
			inHere[messageID] = true
		}
		if _, ok := first[messageID]; !ok {
			first[messageID] = folder
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for messageID := range inHere {
		delete(first, messageID)
	}
	return first, nil
}
