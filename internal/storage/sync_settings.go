package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

var ErrSyncSettingsInvalid = errors.New("invalid sync settings")

// SaveUserSyncSettings validates account/folder ownership and atomically merges
// folder selections with the interval. Unsubmitted accounts retain their modes.
func (db *DB) SaveUserSyncSettings(ctx context.Context, owner string, minutes int, updates map[string][]string) error {
	if minutes < 1 || minutes > 1440 || len(updates) > 256 {
		return ErrSyncSettingsInvalid
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	updates = normalizeIdleFolderEntriesAll(updates)
	for id, entries := range updates {
		var present int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE id=? AND user_id=? AND COALESCE(is_deleting,0)=0`, id, owner).Scan(&present)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAccountRoute
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry == "none" {
				continue
			}
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM folders WHERE account_id=? AND (id=? OR role=?)
				AND COALESCE(selectable,1)=1 AND COALESCE(discovery_state,'active')='active' LIMIT 1`, id, entry, entry).Scan(&present)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrAccountRoute
			}
			if err != nil {
				return err
			}
		}
	}
	if len(updates) > 0 {
		old := ""
		err := tx.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE user_id=? AND key='idle_folders'`, owner).Scan(&old)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		merged := make(map[string][]string)
		if err := json.Unmarshal([]byte(old), &merged); err != nil || merged == nil {
			merged = make(map[string][]string)
			legacy := []string{"inbox", "sent", "drafts"}
			if old != "" {
				legacy = strings.Split(old, ",")
			}
			rows, err := tx.QueryContext(ctx, `SELECT id FROM accounts WHERE user_id=? AND COALESCE(is_deleting,0)=0`, owner)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				merged[id] = append([]string(nil), legacy...)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		for id, entries := range updates {
			merged[id] = entries
		}
		data, err := json.Marshal(normalizeIdleFolderEntriesAll(merged))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO app_settings(key,user_id,value,updated_at)
			VALUES('idle_folders',?,?,CURRENT_TIMESTAMP)`, owner, string(data)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO app_settings(key,user_id,value,updated_at)
		VALUES('sync_interval_minutes',?,?,CURRENT_TIMESTAMP)`, owner, strconv.Itoa(minutes)); err != nil {
		return err
	}
	return tx.Commit()
}
