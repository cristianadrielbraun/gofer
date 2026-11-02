package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

var ErrFolderColor = errors.New("choose a valid folder color")
var ErrFolderIconStyle = errors.New("choose an outlined or filled icon style")
var ErrFolderIcon = errors.New("choose an available icon for a custom folder")

// SetFolderColor preserves an existing icon preference for color-only callers.
func (db *DB) SetFolderColor(ctx context.Context, owner, folderID, color string) error {
	return db.SetFolderAppearance(ctx, owner, folderID, color, nil, nil)
}

// SetFolderAppearance keeps personal appearance outside provider-owned data.
// A nil icon leaves the current icon preference unchanged; an empty icon resets it.
// A nil style preserves the current style.
// The role and ownership are checked in the same transaction as the appearance preferences.
func (db *DB) SetFolderAppearance(ctx context.Context, owner, folderID, color string, icon, style *string) error {
	if !models.ValidFolderColor(color) {
		return ErrFolderColor
	}
	if style != nil && !models.ValidFolderIconStyle(*style) {
		return ErrFolderIconStyle
	}
	if icon != nil && !models.ValidFolderIcon(*icon) {
		return ErrFolderIcon
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var role string
	err = tx.QueryRowContext(ctx, `SELECT f.role FROM folders f JOIN accounts a ON a.id = f.account_id
		WHERE f.id = ? AND a.user_id = ? AND COALESCE(a.is_deleting, 0) = 0
		AND COALESCE(f.discovery_state, 'active') = 'active' AND COALESCE(f.selectable, 1) = 1`, folderID, owner).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrFolderNotManaged
	}
	if err != nil {
		return err
	}
	if icon != nil && role != "custom" {
		return ErrFolderIcon
	}
	set := func(key, value string) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO app_settings (user_id, key, value, updated_at)
			VALUES (?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, owner, key+folderID, value)
		return err
	}
	if err := set("folder_color:", strings.ToLower(color)); err != nil {
		return err
	}
	if icon != nil {
		if err := set("folder_icon:", *icon); err != nil {
			return err
		}
	}
	if style != nil {
		if err := set("folder_icon_style:", *style); err != nil {
			return err
		}
	}
	return tx.Commit()
}
