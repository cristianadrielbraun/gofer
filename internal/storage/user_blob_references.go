package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// BlobReferences is what keeps files in the blob store: the accounts whose
// directories may be cleaned, the file paths the database points at, and the
// messages each of those accounts still has, by local ID.
type BlobReferences struct {
	Accounts []string
	Paths    map[string]bool
	Messages map[string]map[int64]bool
}

// WithUserBlobReferences holds the single writer connection until local file
// cleanup finishes. Publication cannot replace the snapshot during removal.
// The caller must exclude file readers/stagers first, and must not perform
// network work or use DB.Write inside the callback. Incomplete queries or
// malformed retained delivery payloads fail before invoking cleanup.
func (db *DB) WithUserBlobReferences(ctx context.Context, owner string, cleanup func(BlobReferences) error) error {
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
	refs, err := blobReferencesTx(ctx, tx, `a.user_id=?`, owner)
	if err != nil {
		return err
	}
	return cleanup(refs)
}

// WithBlobReferences is WithUserBlobReferences for an installation with one
// shared database, covering every account in it.
func (db *DB) WithBlobReferences(ctx context.Context, cleanup func(BlobReferences) error) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	refs, err := blobReferencesTx(ctx, tx, `1=1`)
	if err != nil {
		return err
	}
	return cleanup(refs)
}

// blobReferencesTx reads the references of the accounts a matches in scope.
func blobReferencesTx(ctx context.Context, tx *sql.Tx, scope string, args ...any) (refs BlobReferences, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.id FROM accounts a WHERE `+scope+` AND COALESCE(a.is_deleting,0)=0 ORDER BY a.id`, args...)
	if err != nil {
		return refs, err
	}
	refs.Messages = make(map[string]map[int64]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return refs, err
		}
		refs.Accounts = append(refs.Accounts, id)
		refs.Messages[id] = make(map[int64]bool)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return refs, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT m.account_id, m.id FROM messages m JOIN accounts a ON a.id=m.account_id WHERE `+scope, args...)
	if err != nil {
		return refs, err
	}
	for rows.Next() {
		var accountID string
		var id int64
		if err := rows.Scan(&accountID, &id); err != nil {
			rows.Close()
			return refs, err
		}
		if live := refs.Messages[accountID]; live != nil {
			live[id] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return refs, err
	}
	keep := make(map[string]bool)
	refs.Paths = keep
	rows, err = tx.QueryContext(ctx, `SELECT m.body_text_path,m.body_html_path,m.body_html_original_path,m.raw_path FROM messages m JOIN accounts a ON a.id=m.account_id WHERE `+scope, args...)
	if err != nil {
		return refs, err
	}
	for rows.Next() {
		var paths [4]sql.NullString
		if err := rows.Scan(&paths[0], &paths[1], &paths[2], &paths[3]); err != nil {
			rows.Close()
			return refs, err
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
		return refs, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT t.storage_path FROM attachments t JOIN messages m ON m.id=t.message_id JOIN accounts a ON a.id=m.account_id WHERE `+scope, args...)
	if err != nil {
		return refs, err
	}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return refs, err
		}
		if path != "" {
			keep[path] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return refs, err
	}
	// Keep attachment paths in all retained send snapshots, including failed
	// and ambiguous sends and unfinished Sent copies. Canceled sends cannot
	// retry; their historical snapshots no longer own file references.
	rows, err = tx.QueryContext(ctx, `SELECT s.message_json FROM outgoing_sends s JOIN accounts a ON a.id=s.account_id WHERE `+scope+` AND s.message_json<>'' AND s.status<>'canceled' AND NOT (s.status='sent' AND s.sent_copy_status IN ('complete','not_required'))`, args...)
	if err != nil {
		return refs, err
	}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			rows.Close()
			return refs, err
		}
		if !strings.HasPrefix(strings.TrimSpace(data), "{") {
			rows.Close()
			return refs, fmt.Errorf("retained send snapshot is not an object")
		}
		var snapshot struct {
			Attachments []struct {
				Path string `json:"path"`
			} `json:"attachments"`
		}
		if err := json.Unmarshal([]byte(data), &snapshot); err != nil {
			rows.Close()
			return refs, fmt.Errorf("read retained send files: %w", err)
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
		return refs, err
	}
	if err := ctx.Err(); err != nil {
		return refs, err
	}
	return refs, nil
}
