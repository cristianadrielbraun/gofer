package storage

import (
	"context"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// SetVirtualFolderAppearance stores view preferences separately from folder data
// and the browser's full UI-settings snapshot. Stale snapshots cannot undo them.
func (db *DB) SetVirtualFolderAppearance(ctx context.Context, owner, id, color string, style *string) error {
	if _, ok := models.VirtualFolderAppearance(id, nil); !ok {
		return ErrFolderNotManaged
	}
	if !models.ValidFolderColor(color) {
		return ErrFolderColor
	}
	if style != nil && !models.ValidFolderIconStyle(*style) {
		return ErrFolderIconStyle
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	updates := map[string]string{models.VirtualFolderColorKey(id): strings.ToLower(color)}
	if style != nil {
		updates[models.VirtualFolderStyleKey(id)] = *style
	}
	for key, value := range updates {
		if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings (user_id, key, value, updated_at)
			VALUES (?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(user_id, key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, owner, key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// addVirtualFolderAppearances overlays authoritative values on a UI snapshot.
// The keys are passed to sidebar/settings renderers through their existing map.
func (db *DB) addVirtualFolderAppearances(ctx context.Context, owner string, settings map[string]string) {
	for key := range settings {
		if strings.HasPrefix(key, "virtual_folder_color:") || strings.HasPrefix(key, "virtual_folder_style:") || strings.HasPrefix(key, "virtual_folder_order:") {
			delete(settings, key)
		}
	}
	rows, err := db.Read().QueryContext(ctx, `SELECT key, value FROM app_settings WHERE user_id=?
		AND (key LIKE 'virtual_folder_color:%' OR key LIKE 'virtual_folder_style:%' OR key LIKE 'virtual_folder_order:%')`, owner)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return
		}
		for _, folder := range models.VirtualFolders() {
			if key == models.VirtualFolderColorKey(folder.ID) && models.ValidFolderColor(value) || key == models.VirtualFolderStyleKey(folder.ID) && models.ValidFolderIconStyle(value) || key == models.VirtualFolderOrderKey(folder.ID) && validOrderPlace(value) {
				settings[key] = value
				break
			}
		}
	}
}
