package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// WithUserBlobReferences holds the single writer connection until local file
// cleanup finishes. Publication cannot replace the snapshot during removal.
// The caller must exclude file readers/stagers first, and must not perform
// network work or use DB.Write inside the callback. Incomplete queries or
// malformed retained delivery payloads fail before invoking cleanup.
func (db *DB) WithUserBlobReferences(ctx context.Context, owner string, cleanup func([]string, map[string]bool) error) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var actual string
	if err := tx.QueryRowContext(ctx, `SELECT user_id FROM gofer_user_store WHERE singleton=1 AND layout_version=1`).Scan(&actual); err != nil {
		return err
	}
	if actual != owner || owner == "" {
		return ErrUserStoreIdentity
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM accounts WHERE user_id=? AND COALESCE(is_deleting,0)=0 ORDER BY id`, owner)
	if err != nil {
		return err
	}
	var accounts []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		accounts = append(accounts, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	keep := make(map[string]bool)
	rows, err = tx.QueryContext(ctx, `SELECT m.body_text_path,m.body_html_path,m.body_html_original_path,m.raw_path FROM messages m JOIN accounts a ON a.id=m.account_id WHERE a.user_id=?`, owner)
	if err != nil {
		return err
	}
	for rows.Next() {
		var paths [4]sql.NullString
		if err := rows.Scan(&paths[0], &paths[1], &paths[2], &paths[3]); err != nil {
			rows.Close()
			return err
		}
		for _, path := range paths {
			if path.String != "" {
				keep[path.String] = true
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT t.storage_path FROM attachments t JOIN messages m ON m.id=t.message_id JOIN accounts a ON a.id=m.account_id WHERE a.user_id=?`, owner)
	if err != nil {
		return err
	}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return err
		}
		if path != "" {
			keep[path] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// Keep attachment paths in all retained send snapshots, including failed
	// and ambiguous sends and unfinished Sent copies. Canceled sends cannot
	// retry; their historical snapshots no longer own file references.
	rows, err = tx.QueryContext(ctx, `SELECT s.message_json FROM outgoing_sends s JOIN accounts a ON a.id=s.account_id WHERE a.user_id=? AND s.message_json<>'' AND s.status<>'canceled' AND NOT (s.status='sent' AND s.sent_copy_status IN ('complete','not_required'))`, owner)
	if err != nil {
		return err
	}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			rows.Close()
			return err
		}
		if !strings.HasPrefix(strings.TrimSpace(data), "{") {
			rows.Close()
			return fmt.Errorf("retained send snapshot is not an object")
		}
		var snapshot struct {
			Attachments []struct {
				Path string `json:"path"`
			} `json:"attachments"`
		}
		if err := json.Unmarshal([]byte(data), &snapshot); err != nil {
			rows.Close()
			return fmt.Errorf("read retained send files: %w", err)
		}
		for _, attachment := range snapshot.Attachments {
			if attachment.Path != "" {
				keep[attachment.Path] = true
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return cleanup(accounts, keep)
}
