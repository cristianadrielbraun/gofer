package storage

import (
	"context"
	"database/sql"
	"errors"
)

// ErrAccountOrderChanged means the order given doesn't name exactly the
// owner's accounts, usually because one was added or removed meanwhile.
var ErrAccountOrderChanged = errors.New("accounts changed while they were being reordered")

// migrateV123ToV124 lets people choose the order of their accounts. Every
// account starts at 0, so the order stays as it was, by ID, until changed.
func migrateV123ToV124(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 124 {
		return nil
	}
	exists, err := tableExistsTx(tx, "accounts")
	if err != nil {
		return err
	}
	if exists {
		present, err := columnExistsTx(tx, "accounts", "sort_order")
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.Exec(`ALTER TABLE accounts ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0`); err != nil {
				return err
			}
		}
	}
	if err := markSchemaVersion(tx, 124); err != nil {
		return err
	}
	return tx.Commit()
}

// SetAccountOrder puts the owner's accounts in the order of ids, which must
// name each of their accounts once. Accounts being deleted are left out.
func (db *DB) SetAccountOrder(ctx context.Context, userID string, ids []string) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM accounts WHERE user_id = ? AND COALESCE(is_deleting, 0) = 0`, userID)
	if err != nil {
		return err
	}
	owned := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		owned[id] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(ids) != len(owned) {
		return ErrAccountOrderChanged
	}
	for i, id := range ids {
		if !owned[id] {
			return ErrAccountOrderChanged
		}
		// Each id is used once, so a repeat is caught as a missing account.
		delete(owned, id)
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET sort_order = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ?`, i, id, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
