package storage

import "database/sql"

// message_folder_state keeps a copy of each message's date and attachment
// flag, so a folder can be listed message by message straight from its
// index, as folder_thread_state lists it conversation by conversation.
// Triggers keep the copies equal to messages.
//
// The same statements are in schema.sql for new databases.
const folderMessageListSchema = `
CREATE TRIGGER IF NOT EXISTS message_folder_state_copy_message_insert
AFTER INSERT ON message_folder_state
BEGIN
    UPDATE message_folder_state
    SET (message_date, message_has_attachments) =
        (SELECT m.date_received, COALESCE(m.has_attachments, 0) FROM messages m WHERE m.id = NEW.message_id)
    WHERE message_id = NEW.message_id AND folder_id = NEW.folder_id
      AND EXISTS(SELECT 1 FROM messages m WHERE m.id = NEW.message_id);
END;

CREATE TRIGGER IF NOT EXISTS message_folder_state_copy_message_update
AFTER UPDATE OF message_id ON message_folder_state
WHEN OLD.message_id IS NOT NEW.message_id
BEGIN
    UPDATE message_folder_state
    SET (message_date, message_has_attachments) =
        (SELECT m.date_received, COALESCE(m.has_attachments, 0) FROM messages m WHERE m.id = NEW.message_id)
    WHERE message_id = NEW.message_id AND folder_id = NEW.folder_id
      AND EXISTS(SELECT 1 FROM messages m WHERE m.id = NEW.message_id);
END;

CREATE TRIGGER IF NOT EXISTS message_folder_state_follow_message
AFTER UPDATE OF date_received, has_attachments ON messages
WHEN OLD.date_received IS NOT NEW.date_received OR OLD.has_attachments IS NOT NEW.has_attachments
BEGIN
    UPDATE message_folder_state
    SET message_date = NEW.date_received, message_has_attachments = COALESCE(NEW.has_attachments, 0)
    WHERE message_id = NEW.id;
END;

CREATE INDEX IF NOT EXISTS idx_message_folder_state_list
ON message_folder_state(folder_id, is_deleted, message_date DESC, message_id DESC);

-- Covers the filters a message list answers from its folder states (unread,
-- starred, attachments), so their counts read only this index.
CREATE INDEX IF NOT EXISTS idx_message_folder_state_flags
ON message_folder_state(folder_id, is_deleted, is_read, is_starred, message_has_attachments);
`

// folderMessageListBackfillSQL copies every message's date and attachment
// flag to its folder states.
const folderMessageListBackfillSQL = `UPDATE message_folder_state
	SET (message_date, message_has_attachments) =
	    (SELECT m.date_received, COALESCE(m.has_attachments, 0) FROM messages m WHERE m.id = message_folder_state.message_id)
	WHERE EXISTS(SELECT 1 FROM messages m WHERE m.id = message_folder_state.message_id)`

// migrateV124ToV125 lets folders be listed message by message as quickly as
// conversation by conversation. idx_message_folder_state_flags starts with
// the columns of idx_folder_state_folder_unread, which it replaces.
func migrateV124ToV125(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 125 {
		return nil
	}
	mail := true
	for _, table := range []string{"messages", "message_folder_state"} {
		exists, err := tableExistsTx(tx, table)
		if err != nil {
			return err
		}
		mail = mail && exists
	}
	// Some historical databases checked by upgrade tests hold only the
	// authentication tables.
	if mail {
		for _, column := range []struct{ name, definition string }{
			{"message_date", "DATETIME"},
			{"message_has_attachments", "INTEGER NOT NULL DEFAULT 0"},
		} {
			present, err := columnExistsTx(tx, "message_folder_state", column.name)
			if err != nil {
				return err
			}
			if !present {
				if _, err := tx.Exec(`ALTER TABLE message_folder_state ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(folderMessageListBackfillSQL); err != nil {
			return err
		}
		if _, err := tx.Exec(folderMessageListSchema); err != nil {
			return err
		}
		if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_folder_state_folder_unread`); err != nil {
			return err
		}
	}
	if err := markSchemaVersion(tx, 125); err != nil {
		return err
	}
	return tx.Commit()
}
