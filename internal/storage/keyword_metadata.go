package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/cristianadrielbraun/gofer/internal/mail/keywords"
)

const keywordMetadataTables = `CREATE TABLE IF NOT EXISTS message_provider_keywords (
	message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	remote_message_id TEXT NOT NULL,
	keywords TEXT NOT NULL,
	fetched INTEGER NOT NULL,
	PRIMARY KEY (message_id, remote_message_id)
);
CREATE TABLE IF NOT EXISTS retired_keyword_labels (
	id TEXT PRIMARY KEY,
	account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	provider_id TEXT NOT NULL
);`

// migrateV153ToV154 stores server keyword and mailbox metadata separately from
// labels. The new flag baseline starts unset so older databases fetch full FLAGS.
func migrateV153ToV154(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 154 {
		return nil
	}
	for _, column := range []struct{ table, name, definition string }{
		{"folders", "mailbox_attributes", "TEXT"},
		{"folders", "flags_metadata_synced", "INTEGER NOT NULL DEFAULT 0"},
		{"message_folder_state", "remote_keywords", "TEXT"},
	} {
		table, err := tableExistsTx(tx, column.table)
		if err != nil {
			return err
		}
		if !table {
			continue
		}
		exists, err := columnExistsTx(tx, column.table, column.name)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := tx.Exec(`ALTER TABLE ` + column.table + ` ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
				return fmt.Errorf("add %s.%s: %w", column.table, column.name, err)
			}
		}
	}
	if _, err := tx.Exec(keywordMetadataTables); err != nil {
		return fmt.Errorf("create keyword metadata tables: %w", err)
	}
	if err := preserveStatusKeywordLabelsTx(tx); err != nil {
		return err
	}
	if err := markSchemaVersion(tx, 154); err != nil {
		return err
	}
	return tx.Commit()
}

// encodeMetadata preserves nil as unknown and an empty list as an observed
// empty snapshot. Strings are always JSON-encodable.
func encodeMetadata[T ~string](values []T) sql.NullString {
	if values == nil {
		return sql.NullString{}
	}
	values = slices.Clone(values)
	slices.Sort(values)
	encoded, _ := json.Marshal(slices.Compact(values))
	return sql.NullString{String: string(encoded), Valid: true}
}

// GetMailboxAttributes returns saved RFC 9979 attributes for a folder. A nil
// slice means the server's metadata has yet to be read. An unknown folder
// returns sql.ErrNoRows.
func (db *DB) GetMailboxAttributes(ctx context.Context, folderID string) ([]keywords.MailboxAttribute, error) {
	return readMetadata[keywords.MailboxAttribute](db.Read().QueryRowContext(ctx,
		`SELECT mailbox_attributes FROM folders WHERE id = ?`, folderID))
}

// GetIMAPMessageKeywords returns the FLAGS snapshot for the represented IMAP
// copy in the folder. A nil slice means no snapshot has been saved for its UID.
// An unknown copy returns sql.ErrNoRows.
func (db *DB) GetIMAPMessageKeywords(ctx context.Context, folderID string, uid uint32) ([]string, error) {
	return readMetadata[string](db.Read().QueryRowContext(ctx,
		`SELECT remote_keywords FROM message_folder_state WHERE folder_id = ? AND remote_uid = ?`, folderID, uid))
}

// GetProviderMessageKeywords returns a keyword snapshot for a provider copy.
// A false fetched value denotes legacy label metadata. An empty remote ID
// selects legacy keywords whose copy provenance is unknown. No saved keywords
// for the given identity returns sql.ErrNoRows.
func (db *DB) GetProviderMessageKeywords(ctx context.Context, messageID int64, remoteID string) (values []string, fetched bool, err error) {
	var raw string
	if err := db.Read().QueryRowContext(ctx, `SELECT keywords, fetched FROM message_provider_keywords
		WHERE message_id = ? AND remote_message_id = ?`, messageID, remoteID).Scan(&raw, &fetched); err != nil {
		return nil, false, err
	}
	err = json.Unmarshal([]byte(raw), &values)
	return values, fetched, err
}

func readMetadata[T ~string](row *sql.Row) ([]T, error) {
	var raw sql.NullString
	if err := row.Scan(&raw); err != nil || !raw.Valid {
		return nil, err
	}
	var values []T
	err := json.Unmarshal([]byte(raw.String), &values)
	return values, err
}

func saveProviderKeywords(ctx context.Context, tx *sql.Tx, messageID int64, remoteID string, snapshot []string) error {
	if snapshot == nil || remoteID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO message_provider_keywords (message_id, remote_message_id, keywords, fetched)
		VALUES (?, ?, ?, 1) ON CONFLICT(message_id, remote_message_id) DO UPDATE SET keywords = excluded.keywords, fetched = 1
		WHERE keywords != excluded.keywords OR fetched = 0`, messageID, remoteID, encodeMetadata(snapshot))
	return err
}
