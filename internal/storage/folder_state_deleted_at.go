package storage

import "database/sql"

// message_folder_state.deleted_at is when a folder state was marked deleted,
// so marked states can be let go once they are old. Triggers keep it in step
// with is_deleted on every path: marking sets it, unmarking clears it, and
// marking a state that is already marked keeps the first time.
//
// The same statements are in schema.sql for new databases.
const folderStateDeletedAtSchema = `
CREATE TRIGGER IF NOT EXISTS message_folder_state_deleted_at_insert
AFTER INSERT ON message_folder_state
WHEN NEW.is_deleted = 1 AND NEW.deleted_at IS NULL
BEGIN
    UPDATE message_folder_state SET deleted_at = CURRENT_TIMESTAMP
    WHERE message_id = NEW.message_id AND folder_id = NEW.folder_id;
END;

CREATE TRIGGER IF NOT EXISTS message_folder_state_deleted_at_update
AFTER UPDATE OF is_deleted ON message_folder_state
WHEN OLD.is_deleted IS NOT NEW.is_deleted
BEGIN
    UPDATE message_folder_state
    SET deleted_at = CASE WHEN NEW.is_deleted = 1 THEN CURRENT_TIMESTAMP END
    WHERE message_id = NEW.message_id AND folder_id = NEW.folder_id;
END;

CREATE INDEX IF NOT EXISTS idx_message_folder_state_deleted_at
ON message_folder_state(deleted_at) WHERE is_deleted = 1;
`

// migrateV125ToV126 records when folder states were marked deleted. States
// already marked count from the upgrade, so none is let go sooner than one
// marked after it.
func migrateV125ToV126(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 126 {
		return nil
	}
	exists, err := tableExistsTx(tx, "message_folder_state")
	if err != nil {
		return err
	}
	if exists {
		exists, err = columnExistsTx(tx, "message_folder_state", "is_deleted")
		if err != nil {
			return err
		}
	}
	// Some historical databases checked by upgrade tests hold only the
	// authentication tables, or folder states from before is_deleted.
	if exists {
		present, err := columnExistsTx(tx, "message_folder_state", "deleted_at")
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.Exec(`ALTER TABLE message_folder_state ADD COLUMN deleted_at DATETIME`); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE message_folder_state SET deleted_at = CURRENT_TIMESTAMP WHERE is_deleted = 1 AND deleted_at IS NULL`); err != nil {
			return err
		}
		if _, err := tx.Exec(folderStateDeletedAtSchema); err != nil {
			return err
		}
	}
	if err := markSchemaVersion(tx, 126); err != nil {
		return err
	}
	return tx.Commit()
}
