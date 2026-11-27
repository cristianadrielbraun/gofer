package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// migrateV154ToV155 adds the color picked for a calendar in Gofer, which
// Gofer keeps when the calendar's provider didn't take it. Sync never
// changes it.
func migrateV154ToV155(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 155 {
		return nil
	}
	table, err := tableExistsTx(tx, "calendar_sources")
	if err != nil {
		return err
	}
	if table {
		exists, err := columnExistsTx(tx, "calendar_sources", "chosen_color")
		if err != nil {
			return err
		}
		if !exists {
			if _, err := tx.Exec(`ALTER TABLE calendar_sources ADD COLUMN chosen_color TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("add calendar_sources.chosen_color: %w", err)
			}
		}
	}
	if err := markSchemaVersion(tx, 155); err != nil {
		return err
	}
	return tx.Commit()
}

// CalendarColorChange is a color picked for a calendar in Gofer, as
// #rrggbb. OnProvider is set when the calendar's provider took it too: it
// becomes the calendar's color, and a later sync brings whatever the
// provider has. Otherwise Gofer keeps it as its own, and sync leaves it
// alone. An empty Color drops the one Gofer kept, back to the provider's.
type CalendarColorChange struct {
	Color      string
	OnProvider bool
}

// SetCalendarSourceColor saves a color picked for one of the user's synced
// calendars. A calendar that isn't synced, or is gone, is sql.ErrNoRows.
func (db *DB) SetCalendarSourceColor(ctx context.Context, userID, sourceID string, change CalendarColorChange) error {
	return setCalendarSourceColor(ctx, db.Write(), userID, sourceID, change)
}

// SetUserCalendarSourceColor is SetCalendarSourceColor in the per-user
// store, checking the account is still active like other calendar
// controls.
func (db *DB) SetUserCalendarSourceColor(ctx context.Context, owner, sourceID string, change CalendarColorChange, guard func(*sql.Tx, string) error) error {
	if guard == nil {
		return ErrAccountRoute
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var accountID string
	if err := tx.QueryRowContext(ctx, `SELECT account_id FROM calendar_sources
 WHERE id=? AND user_id=? AND is_selected=1 AND is_deleted=0`, sourceID, owner).Scan(&accountID); err != nil {
		return err
	}
	if err := calendarControlOwnerTx(ctx, tx, owner, accountID, guard); err != nil {
		return err
	}
	if err := setCalendarSourceColor(ctx, tx, owner, sourceID, change); err != nil {
		return err
	}
	return tx.Commit()
}

func setCalendarSourceColor(ctx context.Context, tx calendarControlExecutor, userID, sourceID string, change CalendarColorChange) error {
	color := strings.ToLower(strings.TrimSpace(change.Color))
	if color != "" && !calendarHexColor(color) {
		return fmt.Errorf("calendar color %q is not #rrggbb", change.Color)
	}
	update := `UPDATE calendar_sources SET chosen_color = ?`
	args := []any{color}
	if change.OnProvider && color != "" {
		update = `UPDATE calendar_sources SET color = ?, chosen_color = ''`
	}
	result, err := tx.ExecContext(ctx, update+`, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND user_id = ? AND is_selected = 1 AND is_deleted = 0
		  AND EXISTS (
			SELECT 1 FROM accounts
			WHERE accounts.id = calendar_sources.account_id
			  AND accounts.user_id = calendar_sources.user_id
			  AND COALESCE(accounts.is_deleting, 0) = 0
		  )`, append(args, sourceID, userID)...)
	if err != nil {
		return fmt.Errorf("save calendar color: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func calendarHexColor(value string) bool {
	if len(value) != 7 || value[0] != '#' {
		return false
	}
	for _, r := range value[1:] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// calendarShownColorSQL is the color Gofer shows for source: the one picked
// in Gofer, or else the provider's.
const calendarShownColorSQL = `CASE WHEN source.chosen_color != '' THEN source.chosen_color ELSE source.color END`
