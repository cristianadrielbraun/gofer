package storage

import (
	"context"
	"database/sql"
)

// migrateV132ToV133 records which calendars were created from Gofer: Google
// lets Gofer rename and delete those without full calendar access.
func migrateV132ToV133(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 133 {
		return nil
	}
	exists, err := tableExistsTx(tx, "calendar_sources")
	if err != nil {
		return err
	}
	if exists {
		present, err := columnExistsTx(tx, "calendar_sources", "created_in_gofer")
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.Exec(`ALTER TABLE calendar_sources ADD COLUMN created_in_gofer INTEGER NOT NULL DEFAULT 0`); err != nil {
				return err
			}
		}
	}
	if err := markSchemaVersion(tx, 133); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkCalendarCreatedInGofer records that the account's calendar remoteID was
// created from Gofer.
func (db *DB) MarkCalendarCreatedInGofer(ctx context.Context, userID, accountID, remoteID string) error {
	_, err := db.Write().ExecContext(ctx, `UPDATE calendar_sources SET created_in_gofer = 1 WHERE user_id = ? AND account_id = ? AND remote_id = ?`, userID, accountID, remoteID)
	return err
}
