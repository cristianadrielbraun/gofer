package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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

// SetAccountContainerOrder puts the direct subgroups and accounts inside
// parentGroupID ("" for the top level) in the order of items, where each entry
// is "group:<id>" or "account:<id>" (or a bare account id).
func (db *DB) SetAccountContainerOrder(ctx context.Context, userID, parentGroupID string, items []string) error {
	parentGroupID = strings.TrimSpace(parentGroupID)
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if parentGroupID != "" {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_groups WHERE id = ? AND user_id = ?`, parentGroupID, userID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrAccountOrderChanged
		}
	}

	owned := map[string]bool{}
	gRows, err := tx.QueryContext(ctx, `SELECT id FROM account_groups WHERE user_id = ? AND COALESCE(parent_id, '') = ?`, userID, parentGroupID)
	if err != nil {
		return err
	}
	for gRows.Next() {
		var id string
		if err := gRows.Scan(&id); err != nil {
			gRows.Close()
			return err
		}
		owned["group:"+id] = true
	}
	if err := gRows.Close(); err != nil {
		return err
	}

	aRows, err := tx.QueryContext(ctx, `SELECT id FROM accounts WHERE user_id = ? AND group_id = ? AND COALESCE(is_deleting, 0) = 0`, userID, parentGroupID)
	if err != nil {
		return err
	}
	for aRows.Next() {
		var id string
		if err := aRows.Scan(&id); err != nil {
			aRows.Close()
			return err
		}
		owned["account:"+id] = true
	}
	if err := aRows.Close(); err != nil {
		return err
	}

	if len(items) != len(owned) {
		return ErrAccountOrderChanged
	}
	for i, raw := range items {
		key := strings.TrimSpace(raw)
		if !strings.HasPrefix(key, "group:") && !strings.HasPrefix(key, "account:") {
			key = "account:" + key
		}
		if !owned[key] {
			return ErrAccountOrderChanged
		}
		delete(owned, key)
		if id, ok := strings.CutPrefix(key, "group:"); ok {
			if _, err := tx.ExecContext(ctx, `UPDATE account_groups SET sort_order = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ?`, i, id, userID); err != nil {
				return err
			}
		} else if id, ok := strings.CutPrefix(key, "account:"); ok {
			if _, err := tx.ExecContext(ctx, `UPDATE accounts SET sort_order = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ?`, i, id, userID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
