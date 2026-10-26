package storage

import (
	"context"
	"strings"
)

// MessagesShownInFolder returns which of internetIDs, the messages' raw
// Message-IDs, the account already shows in folderID. A message Gofer moved
// there is shown before the server reports it, under a new UID that looks
// like new mail.
func (db *DB) MessagesShownInFolder(ctx context.Context, accountID, folderID string, internetIDs []string) (map[string]bool, error) {
	shown := map[string]bool{}
	const batch = 500
	for start := 0; start < len(internetIDs); start += batch {
		ids := internetIDs[start:min(start+batch, len(internetIDs))]
		args := []any{accountID, folderID}
		for _, id := range ids {
			args = append(args, id)
		}
		rows, err := db.Read().QueryContext(ctx, `
			SELECT m.internet_message_id FROM messages m
			JOIN message_folder_state s ON s.message_id = m.id
			WHERE m.account_id = ? AND s.folder_id = ? AND s.is_deleted = 0
			  AND m.internet_message_id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			shown[id] = true
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return shown, nil
}
