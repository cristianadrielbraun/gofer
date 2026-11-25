package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// Hiding a folder or a tag only takes it out of the sidebar. A folder still
// syncs, mail keeps its tags, and search, rules and the Move dialog still
// offer them, so a hidden folder keeps working for mail rules file there.
// Showing is the default, so only hidden ones have a setting, set to
// "false": folder_visible:<id>, tag_visible:<id> for an account's tag, and
// unified_tag_visible:<id> for a tag of all accounts.

// SetFolderVisibility hides one of the owner's folders from the sidebar, or
// shows it again, on any account type. Only folders without a role can be
// hidden, so Inbox, Sent and the other special folders always show. A
// folder that isn't the owner's, isn't one of them, or is gone from the
// server is ErrFolderNotManaged.
func (db *DB) SetFolderVisibility(ctx context.Context, owner, folderID string, visible bool) error {
	return db.setSidebarVisibility(ctx, owner, "folder_visible:"+folderID, visible, func(tx *sql.Tx) error {
		var found int
		err := tx.QueryRowContext(ctx, `
			SELECT 1
			FROM folders f
			JOIN accounts a ON a.id = f.account_id
			WHERE f.id = ?
			  AND a.user_id = ?
			  AND COALESCE(a.is_deleting, 0) = 0
			  AND COALESCE(f.discovery_state, 'active') = 'active'
			  AND COALESCE(f.selectable, 1) = 1
			  AND f.role = 'custom'`, folderID, owner).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFolderNotManaged
		}
		return err
	})
}

// SetTagVisibility hides one of an account's tags from the sidebar, or
// shows it again. Any tag the sidebar can show can be hidden, as any can be
// given a color. One it can't show is ErrFolderNotManaged.
func (db *DB) SetTagVisibility(ctx context.Context, owner, id string, visible bool) error {
	return db.setTagVisibility(ctx, owner, id, false, visible)
}

// SetUnifiedTagVisibility hides a tag from the tags of all accounts, by the
// ID the sidebar gives it there, or shows it again. Each account's own copy
// keeps its place. A name no account's tag has is ErrFolderNotManaged.
func (db *DB) SetUnifiedTagVisibility(ctx context.Context, owner, id string, visible bool) error {
	return db.setTagVisibility(ctx, owner, id, true, visible)
}

func (db *DB) setTagVisibility(ctx context.Context, owner, id string, unified, visible bool) error {
	return db.setSidebarVisibility(ctx, owner, models.TagVisibleKey(id, unified), visible, func(tx *sql.Tx) error {
		_, err := findAppearanceTag(ctx, tx, owner, id, unified)
		return err
	})
}

// setSidebarVisibility stores whether the folder or tag that find looks
// for is hidden, under key.
func (db *DB) setSidebarVisibility(ctx context.Context, owner, key string, visible bool, find func(*sql.Tx) error) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sidebar visibility: %w", err)
	}
	defer tx.Rollback()

	if err := find(tx); errors.Is(err, ErrFolderNotManaged) {
		return err
	} else if err != nil {
		return fmt.Errorf("find what to hide: %w", err)
	}
	if visible {
		_, err = tx.ExecContext(ctx, `DELETE FROM app_settings WHERE user_id = ? AND key = ?`, owner, key)
	} else {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO app_settings (user_id, key, value, updated_at)
			VALUES (?, ?, 'false', CURRENT_TIMESTAMP)
			ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, owner, key)
	}
	if err != nil {
		return fmt.Errorf("save sidebar visibility: %w", err)
	}
	return tx.Commit()
}
