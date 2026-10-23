package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// folder_thread_state summarizes each conversation in each folder for the mail
// list. It is maintained incrementally: triggers record which (folder,
// conversation) pairs a write touched in folder_thread_dirty, and a flush
// recomputes only those pairs. The thread key "*" marks a whole folder whose
// summary has never been built; it is rebuilt in full on first use, and
// changes to it are not recorded until then.
//
// The same statements are in schema.sql for new databases. Trigger inserts
// use an upsert clause because an outer INSERT OR REPLACE or OR ABORT would
// override an OR IGNORE inside the trigger.
const folderThreadTrackingSchema = `
CREATE TABLE IF NOT EXISTS folder_thread_dirty (
    folder_id TEXT NOT NULL,
    thread_key TEXT NOT NULL,
    PRIMARY KEY (folder_id, thread_key)
) WITHOUT ROWID;

-- Covers the filters answered from summaries (unread, starred, attachments,
-- conversations, account), so their counts read only this index.
CREATE INDEX IF NOT EXISTS idx_folder_thread_state_flags
ON folder_thread_state(folder_id, thread_is_read, thread_is_starred, thread_has_attachments, thread_count, account_id);

CREATE TRIGGER IF NOT EXISTS folder_thread_track_state_insert
AFTER INSERT ON message_folder_state
BEGIN
    INSERT INTO folder_thread_dirty(folder_id, thread_key)
    SELECT NEW.folder_id, COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))
    FROM messages m WHERE m.id = NEW.message_id
      AND NOT EXISTS(SELECT 1 FROM folder_thread_dirty w WHERE w.folder_id = NEW.folder_id AND w.thread_key = '*')
    ON CONFLICT DO NOTHING;
END;

CREATE TRIGGER IF NOT EXISTS folder_thread_track_state_update
AFTER UPDATE ON message_folder_state
WHEN OLD.is_read IS NOT NEW.is_read OR OLD.is_starred IS NOT NEW.is_starred
  OR OLD.is_deleted IS NOT NEW.is_deleted OR OLD.folder_id IS NOT NEW.folder_id
  OR OLD.message_id IS NOT NEW.message_id
BEGIN
    INSERT INTO folder_thread_dirty(folder_id, thread_key)
    SELECT OLD.folder_id, COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))
    FROM messages m WHERE m.id = OLD.message_id
      AND NOT EXISTS(SELECT 1 FROM folder_thread_dirty w WHERE w.folder_id = OLD.folder_id AND w.thread_key = '*')
    ON CONFLICT DO NOTHING;
    INSERT INTO folder_thread_dirty(folder_id, thread_key)
    SELECT NEW.folder_id, COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))
    FROM messages m WHERE m.id = NEW.message_id
      AND NOT EXISTS(SELECT 1 FROM folder_thread_dirty w WHERE w.folder_id = NEW.folder_id AND w.thread_key = '*')
    ON CONFLICT DO NOTHING;
END;

CREATE TRIGGER IF NOT EXISTS folder_thread_track_state_delete
AFTER DELETE ON message_folder_state
BEGIN
    INSERT INTO folder_thread_dirty(folder_id, thread_key)
    SELECT OLD.folder_id, COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id))
    FROM messages m WHERE m.id = OLD.message_id
      AND NOT EXISTS(SELECT 1 FROM folder_thread_dirty w WHERE w.folder_id = OLD.folder_id AND w.thread_key = '*')
    ON CONFLICT DO NOTHING;
END;

CREATE TRIGGER IF NOT EXISTS folder_thread_track_message_update
AFTER UPDATE OF thread_id, date_received, has_attachments ON messages
WHEN OLD.thread_id IS NOT NEW.thread_id OR OLD.date_received IS NOT NEW.date_received
  OR OLD.has_attachments IS NOT NEW.has_attachments
BEGIN
    INSERT INTO folder_thread_dirty(folder_id, thread_key)
    SELECT mfs.folder_id, COALESCE(NULLIF(OLD.thread_id, ''), printf('msg:%d', OLD.id))
    FROM message_folder_state mfs WHERE mfs.message_id = OLD.id
      AND NOT EXISTS(SELECT 1 FROM folder_thread_dirty w WHERE w.folder_id = mfs.folder_id AND w.thread_key = '*')
    ON CONFLICT DO NOTHING;
    INSERT INTO folder_thread_dirty(folder_id, thread_key)
    SELECT mfs.folder_id, COALESCE(NULLIF(NEW.thread_id, ''), printf('msg:%d', NEW.id))
    FROM message_folder_state mfs WHERE mfs.message_id = NEW.id
      AND NOT EXISTS(SELECT 1 FROM folder_thread_dirty w WHERE w.folder_id = mfs.folder_id AND w.thread_key = '*')
    ON CONFLICT DO NOTHING;
END;

-- Runs before the cascade removes the message's folder states, while its
-- conversation is still known.
CREATE TRIGGER IF NOT EXISTS folder_thread_track_message_delete
BEFORE DELETE ON messages
BEGIN
    INSERT INTO folder_thread_dirty(folder_id, thread_key)
    SELECT mfs.folder_id, COALESCE(NULLIF(OLD.thread_id, ''), printf('msg:%d', OLD.id))
    FROM message_folder_state mfs WHERE mfs.message_id = OLD.id
      AND NOT EXISTS(SELECT 1 FROM folder_thread_dirty w WHERE w.folder_id = mfs.folder_id AND w.thread_key = '*')
    ON CONFLICT DO NOTHING;
END;
`

const folderThreadRebuildKey = "*"

func migrateV107ToV108(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 108 {
		return nil
	}
	mail := true
	for _, table := range []string{"folders", "messages", "message_folder_state", "folder_thread_state"} {
		exists, err := tableExistsTx(tx, table)
		if err != nil {
			return err
		}
		mail = mail && exists
	}
	// Some historical databases checked by upgrade tests hold only the
	// authentication tables; mail tables there come from later repairs.
	if mail {
		if _, err := tx.Exec(folderThreadTrackingSchema); err != nil {
			return err
		}
		// Summaries were built lazily and only for folders that were opened,
		// so none can be trusted as complete: rebuild each folder once on
		// first use.
		if _, err := tx.Exec(`INSERT OR IGNORE INTO folder_thread_dirty(folder_id, thread_key)
			SELECT id, ? FROM folders`, folderThreadRebuildKey); err != nil {
			return err
		}
	}
	if err := markSchemaVersion(tx, 108); err != nil {
		return err
	}
	return tx.Commit()
}

// folderThreadTriggers are the triggers in folderThreadTrackingSchema.
var folderThreadTriggers = []string{
	"folder_thread_track_state_insert",
	"folder_thread_track_state_update",
	"folder_thread_track_state_delete",
	"folder_thread_track_message_update",
	"folder_thread_track_message_delete",
}

// repairFolderThreadTriggers replaces triggers left from a development build
// of schema version 108, which used INSERT OR IGNORE. An upsert on
// message_folder_state overrides that, so reviving a hidden folder entry, as
// moving mail back to a folder it left does, failed on a duplicate. CREATE
// TRIGGER IF NOT EXISTS never replaces them, so they are dropped and created
// again from the current definitions.
func repairFolderThreadTriggers(db *sql.DB) error {
	var stale int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'trigger' AND name LIKE 'folder_thread_track_%'
		  AND sql LIKE '%INSERT OR IGNORE INTO folder_thread_dirty%'`).Scan(&stale); err != nil || stale == 0 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, name := range folderThreadTriggers {
		if _, err := tx.Exec(`DROP TRIGGER IF EXISTS ` + name); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(folderThreadTrackingSchema); err != nil {
		return err
	}
	return tx.Commit()
}

// flushFolderThreadState brings folder_thread_state up to date with every
// recorded change. Folders waiting for their first build are rebuilt in full
// when listed in rebuild; otherwise they wait for first use.
func (db *DB) flushFolderThreadState(ctx context.Context, rebuild ...string) error {
	rebuild = compactStrings(rebuild)
	pending, err := db.folderThreadStatePending(ctx, rebuild)
	if err != nil || !pending {
		return err
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := flushFolderThreadStateTx(ctx, tx, rebuild); err != nil {
		return err
	}
	return tx.Commit()
}

// folderThreadStatePending reports whether a flush has work, so reads of an
// up-to-date folder never take the writer.
func (db *DB) folderThreadStatePending(ctx context.Context, rebuild []string) (bool, error) {
	var pending bool
	if err := db.Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM folder_thread_dirty WHERE thread_key <> '*')`).Scan(&pending); err != nil || pending {
		return pending, err
	}
	for _, folderID := range rebuild {
		if err := db.Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM folder_thread_dirty WHERE folder_id = ? AND thread_key = '*')`,
			folderID).Scan(&pending); err != nil || pending {
			return pending, err
		}
	}
	return false, nil
}

func flushFolderThreadStateTx(ctx context.Context, tx *sql.Tx, rebuild []string) error {
	for _, folderID := range rebuild {
		var unbuilt bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM folder_thread_dirty WHERE folder_id = ? AND thread_key = ?)`,
			folderID, folderThreadRebuildKey).Scan(&unbuilt); err != nil {
			return err
		}
		if unbuilt {
			if err := rebuildFolderThreadStateTx(ctx, tx, folderID); err != nil {
				return err
			}
		}
	}
	// Pairs in folders still waiting for a full build stay recorded; the
	// build covers them.
	const pending = `SELECT d.folder_id, d.thread_key FROM folder_thread_dirty d
		WHERE d.thread_key <> '*'
		  AND NOT EXISTS(SELECT 1 FROM folder_thread_dirty w WHERE w.folder_id = d.folder_id AND w.thread_key = '*')`
	if _, err := tx.ExecContext(ctx, `DELETE FROM folder_thread_state
		WHERE (folder_id, thread_key) IN (`+pending+`)`); err != nil {
		return fmt.Errorf("clear changed conversations: %w", err)
	}
	// CROSS JOIN fixes the join order: from each changed pair to its
	// messages by index, never by scanning the folder.
	if _, err := tx.ExecContext(ctx, `WITH changed AS (`+pending+`),
		members AS (
			SELECT c.folder_id, c.thread_key, m.id, m.account_id, m.date_received, m.has_attachments,
			       mfs.is_read, mfs.is_starred
			FROM changed c
			JOIN folders f ON f.id = c.folder_id
			CROSS JOIN messages m ON m.account_id = f.account_id AND m.thread_id = c.thread_key
			CROSS JOIN message_folder_state mfs ON mfs.message_id = m.id AND mfs.folder_id = c.folder_id AND mfs.is_deleted = 0
			WHERE substr(c.thread_key, 1, 4) <> 'msg:'
			UNION ALL
			SELECT c.folder_id, c.thread_key, m.id, m.account_id, m.date_received, m.has_attachments,
			       mfs.is_read, mfs.is_starred
			FROM changed c
			CROSS JOIN messages m ON m.id = CAST(substr(c.thread_key, 5) AS INTEGER)
			CROSS JOIN message_folder_state mfs ON mfs.message_id = m.id AND mfs.folder_id = c.folder_id AND mfs.is_deleted = 0
			WHERE substr(c.thread_key, 1, 4) = 'msg:' AND COALESCE(m.thread_id, '') = ''
		),
		ranked AS (
			SELECT *,
			       ROW_NUMBER() OVER thread_order AS rn,
			       COUNT(*) OVER conversation AS thread_count,
			       MIN(is_read) OVER conversation AS thread_is_read,
			       MAX(is_starred) OVER conversation AS thread_is_starred,
			       MAX(has_attachments) OVER conversation AS thread_has_attachments
			FROM members
			WINDOW conversation AS (PARTITION BY folder_id, thread_key),
			       thread_order AS (PARTITION BY folder_id, thread_key
			                        ORDER BY COALESCE(date_received, '') || ':' || printf('%020d', id) DESC)
		)
		INSERT OR REPLACE INTO folder_thread_state (
			folder_id, thread_key, head_message_id, account_id, last_message_at,
			thread_count, thread_is_read, thread_is_starred, thread_has_attachments, updated_at
		)
		SELECT folder_id, thread_key, id, account_id, date_received,
		       thread_count, thread_is_read, thread_is_starred, thread_has_attachments, CURRENT_TIMESTAMP
		FROM ranked WHERE rn = 1`); err != nil {
		return fmt.Errorf("summarize changed conversations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM folder_thread_dirty
		WHERE (folder_id, thread_key) IN (`+pending+`)`); err != nil {
		return fmt.Errorf("clear recorded changes: %w", err)
	}
	return nil
}

func compactStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := values[:0:0]
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
