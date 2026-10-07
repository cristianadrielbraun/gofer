package storage

import (
	"context"
	"database/sql"
)

// Owned controls recheck central lifecycle after waiting for the local writer.
// The guard receives the account read from the owned source, never a browser
// supplied account for a visibility change.
func (db *DB) SetUserCalendarSourceSelection(ctx context.Context, owner, accountID string, ids []string, guard func(*sql.Tx, string) error) error {
	if guard == nil {
		return ErrAccountRoute
	}
	return db.setCalendarSourceSelection(ctx, owner, accountID, ids, func(tx *sql.Tx) error {
		return calendarControlOwnerTx(ctx, tx, owner, accountID, guard)
	})
}

func calendarControlOwnerTx(ctx context.Context, tx *sql.Tx, owner, accountID string, guard func(*sql.Tx, string) error) error {
	if guard == nil || owner == "" || accountID == "" {
		return ErrAccountRoute
	}
	if err := guard(tx, accountID); err != nil {
		return err
	}
	var found int
	return tx.QueryRowContext(ctx, `SELECT 1 FROM accounts a JOIN users u ON u.id=a.user_id
 WHERE a.id=? AND a.user_id=? AND COALESCE(a.is_deleting,0)=0
 AND u.status='active' AND u.deletion_pending=0`, accountID, owner).Scan(&found)
}

func (db *DB) SetUserCalendarSourceVisibility(ctx context.Context, owner, sourceID string, visible bool, guard func(*sql.Tx, string) error) error {
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
	if err := setCalendarSourceVisibility(ctx, tx, owner, sourceID, visible); err != nil {
		return err
	}
	return tx.Commit()
}

// Enabling selects every live source, just as the existing service toggle does.
// It leaves display visibility, cached events and sync progress untouched.
func (db *DB) SetUserCalendarServiceEnabled(ctx context.Context, owner, accountID string, enabled bool, guard func(*sql.Tx, string) error) error {
	if guard == nil {
		return ErrAccountRoute
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := calendarControlOwnerTx(ctx, tx, owner, accountID, guard); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE calendar_sources SET is_selected=?,updated_at=CURRENT_TIMESTAMP
 WHERE account_id=? AND user_id=? AND is_deleted=0`, calendarBoolInt(enabled), accountID, owner)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrCalendarSourcesNotConfigured
	}
	return tx.Commit()
}
