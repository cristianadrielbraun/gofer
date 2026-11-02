package storage

import (
	"context"
	"errors"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

var ErrFolderColor = errors.New("choose a valid folder color")

// SetFolderColor keeps personal appearance outside provider-owned folder data.
// Ownership and active state are checked in the write itself.
func (db *DB) SetFolderColor(ctx context.Context, owner, folderID, color string) error {
	if !models.ValidFolderColor(color) {
		return ErrFolderColor
	}
	result, err := db.Write().ExecContext(ctx, `INSERT INTO app_settings (user_id, key, value, updated_at)
		SELECT a.user_id, 'folder_color:' || f.id, ?, CURRENT_TIMESTAMP
		FROM folders f JOIN accounts a ON a.id = f.account_id
		WHERE f.id = ? AND a.user_id = ? AND COALESCE(a.is_deleting, 0) = 0
		AND COALESCE(f.discovery_state, 'active') = 'active' AND COALESCE(f.selectable, 1) = 1
		ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		strings.ToLower(color), folderID, owner)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err == nil && changed == 0 {
		return ErrFolderNotManaged
	}
	return err
}
