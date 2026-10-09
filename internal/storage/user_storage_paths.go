package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
)

// The shared layout stored message and upload paths relative to the working
// directory Gofer was started from. Relative paths only resolve, and retention
// only recognizes them as referenced, from that directory.
const relativeStoredPath = `(%[1]s<>'' AND substr(%[1]s,1,1) NOT IN ('/','\'))`

var storedPathColumns = []string{"body_text_path", "body_html_path", "body_html_original_path", "raw_path"}

// absolutizeStoredPaths rewrites the relative file paths a user database kept
// from the shared layout into absolute paths in the layout's blob directory,
// resolved against the working directory recorded at migration. Paths written
// since are already absolute, so after the first start this finds nothing.
func absolutizeStoredPaths(ctx context.Context, path string, layout UserStorageLayout) (err error) {
	pending, err := hasRelativeStoredPaths(ctx, path)
	if err != nil || !pending {
		return err
	}
	resolve := func(stored string) (string, bool, error) {
		if stored == "" || filepath.IsAbs(stored) {
			return stored, false, nil
		}
		relative, err := filepath.Rel(layout.BlobDirectory, filepath.Join(layout.WorkingDirectory, stored))
		if err != nil || !filepath.IsLocal(relative) {
			return "", false, errors.New("a retained file path is outside the blob directory")
		}
		return filepath.Join(layout.BlobDirectory, relative), true, nil
	}
	db, err := openDB(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := absolutizeMessagePaths(ctx, tx, resolve); err != nil {
		return err
	}
	if err := absolutizeRows(ctx, tx, `SELECT storage_path,id FROM attachments WHERE `+fmt.Sprintf(relativeStoredPath, "storage_path"),
		`UPDATE attachments SET storage_path=? WHERE id=?`, 1, resolve); err != nil {
		return err
	}
	// Send snapshots reference compose uploads. json_set replaces only the
	// path members; snapshots that are not valid JSON objects stay untouched.
	if err := absolutizeRows(ctx, tx, `SELECT json_extract(a.value,'$.path'),s.id,'$.attachments['||a.key||'].path'
 FROM outgoing_sends s,json_each(CASE WHEN json_valid(s.message_json) AND json_type(s.message_json,'$.attachments')='array' THEN s.message_json ELSE '{"attachments":[]}' END,'$.attachments') a
 WHERE a.type='object' AND json_type(a.value,'$.path')='text' AND `+fmt.Sprintf(relativeStoredPath, "json_extract(a.value,'$.path')"),
		`UPDATE outgoing_sends SET message_json=json_set(message_json,?3,?1) WHERE id=?2`, 2, resolve); err != nil {
		return err
	}
	if pending, err := hasRelativeStoredPathsTx(ctx, tx); err != nil {
		return err
	} else if pending && filepath.Separator == '/' {
		return errors.New("retained file paths remain relative")
	}
	return tx.Commit()
}

func absolutizeMessagePaths(ctx context.Context, tx *sql.Tx, resolve func(string) (string, bool, error)) error {
	condition := ""
	for i, column := range storedPathColumns {
		if i > 0 {
			condition += " OR "
		}
		condition += fmt.Sprintf(relativeStoredPath, column)
	}
	type row struct {
		id    int64
		paths [4]sql.NullString
	}
	var last int64
	for {
		rows, err := tx.QueryContext(ctx, `SELECT id,body_text_path,body_html_path,body_html_original_path,raw_path FROM messages WHERE id>? AND (`+condition+`) ORDER BY id LIMIT 1000`, last)
		if err != nil {
			return err
		}
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.paths[0], &r.paths[1], &r.paths[2], &r.paths[3]); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, r)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, r := range batch {
			values := make([]any, 0, len(r.paths)+1)
			changed := false
			for _, stored := range r.paths {
				if !stored.Valid {
					values = append(values, nil)
					continue
				}
				resolved, rewritten, err := resolve(stored.String)
				if err != nil {
					return err
				}
				changed = changed || rewritten
				values = append(values, resolved)
			}
			if !changed {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET body_text_path=?,body_html_path=?,body_html_original_path=?,raw_path=? WHERE id=?`, append(values, r.id)...); err != nil {
				return err
			}
		}
		last = batch[len(batch)-1].id
	}
}

// absolutizeRows reads every candidate (path, keys...) before writing, since
// the transaction's single connection cannot interleave a query and updates.
// The update receives the resolved path followed by the keys.
func absolutizeRows(ctx context.Context, tx *sql.Tx, query, update string, keys int, resolve func(string) (string, bool, error)) error {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	var candidates [][]any
	for rows.Next() {
		var path string
		values := make([]any, keys+1)
		targets := []any{&path}
		for i := 1; i <= keys; i++ {
			targets = append(targets, &values[i])
		}
		if err := rows.Scan(targets...); err != nil {
			rows.Close()
			return err
		}
		values[0] = path
		candidates = append(candidates, values)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, values := range candidates {
		resolved, changed, err := resolve(values[0].(string))
		if err != nil {
			return err
		}
		if !changed {
			continue
		}
		values[0] = resolved
		if _, err := tx.ExecContext(ctx, update, values...); err != nil {
			return err
		}
	}
	return nil
}

func hasRelativeStoredPaths(ctx context.Context, path string) (pending bool, err error) {
	db, err := openReadOnlyDB(path)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	return hasRelativeStoredPathsTx(ctx, tx)
}

func hasRelativeStoredPathsTx(ctx context.Context, tx *sql.Tx) (pending bool, err error) {
	query := `EXISTS(SELECT 1 FROM attachments WHERE ` + fmt.Sprintf(relativeStoredPath, "storage_path") + `)
 OR EXISTS(SELECT 1 FROM outgoing_sends s,json_each(CASE WHEN json_valid(s.message_json) AND json_type(s.message_json,'$.attachments')='array' THEN s.message_json ELSE '{"attachments":[]}' END,'$.attachments') a
 WHERE a.type='object' AND json_type(a.value,'$.path')='text' AND ` + fmt.Sprintf(relativeStoredPath, "json_extract(a.value,'$.path')") + `)`
	for _, column := range storedPathColumns {
		query += ` OR EXISTS(SELECT 1 FROM messages WHERE ` + fmt.Sprintf(relativeStoredPath, column) + `)`
	}
	err = tx.QueryRowContext(ctx, `SELECT `+query).Scan(&pending)
	return pending, err
}
