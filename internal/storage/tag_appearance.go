package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

const tagVisibilitySQL = `COALESCE(a.is_deleting, 0) = 0
		  AND COALESCE(l.is_system, 0) = 0
		  AND trim(l.name) != ''
		  AND NOT (
		    l.provider_type = 'imap_keyword'
		    AND (
		      lower(trim(l.name)) IN ('junk', 'notjunk', 'nonjunk', 'non-junk', '$junk', '$notjunk', '$nonjunk')
		      OR lower(trim(l.provider_id)) IN ('junk', 'notjunk', 'nonjunk', 'non-junk', '$junk', '$notjunk', '$nonjunk')
		    )
		  )`

type tagAppearanceQueryer interface {
	rowQueryer
	rowsQueryer
}

func findAppearanceTag(ctx context.Context, q tagAppearanceQueryer, owner, id string, unified bool) (models.Label, error) {
	const query = `SELECT l.id, l.account_id, l.name, l.color, l.provider_id, l.provider_type
		FROM labels l JOIN accounts a ON a.id=l.account_id WHERE a.user_id=? AND ` + tagVisibilitySQL
	var label models.Label
	if !unified {
		err := q.QueryRowContext(ctx, query+` AND l.id=?`, owner, id).Scan(&label.ID, &label.AccountID, &label.Name, &label.Color, &label.ProviderID, &label.ProviderType)
		if errors.Is(err, sql.ErrNoRows) {
			return label, ErrFolderNotManaged
		}
		return label, err
	}
	if !models.IsUnifiedTagID(id) {
		return label, ErrFolderNotManaged
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY l.name COLLATE NOCASE`, owner)
	if err != nil {
		return label, err
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&label.ID, &label.AccountID, &label.Name, &label.Color, &label.ProviderID, &label.ProviderType); err != nil {
			return models.Label{}, err
		}
		if models.UnifiedTagID(label.Name) == id {
			return models.UnifiedTagAppearance(label, nil), nil
		}
	}
	if err := rows.Err(); err != nil {
		return models.Label{}, err
	}
	return models.Label{}, ErrFolderNotManaged
}

// TagAppearanceForUser supports every visible tag, including local tags without provider IDs.
func (db *DB) TagAppearanceForUser(ctx context.Context, owner, id string, unified bool) (models.Label, error) {
	label, err := findAppearanceTag(ctx, db.Read(), owner, id, unified)
	if err != nil {
		return label, err
	}
	rows, err := db.Read().QueryContext(ctx, `SELECT key,value FROM app_settings WHERE user_id=? AND key IN (?,?)`, owner, models.TagColorKey(id, unified), models.TagStyleKey(id, unified))
	if err != nil {
		return label, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return label, err
		}
		if key == models.TagColorKey(id, unified) && models.ValidFolderColor(value) {
			label.AppearanceColor = value
		}
		if key == models.TagStyleKey(id, unified) && models.ValidFolderIconStyle(value) {
			label.IconStyle = value
		}
	}
	return label, rows.Err()
}

// SetTagAppearance checks ownership again in the transaction. Provider colors,
// names and mutation queues are untouched; a color-only update preserves style.
func (db *DB) SetTagAppearance(ctx context.Context, owner, id, color string, style *string, unified bool) (models.Label, error) {
	if !models.ValidFolderColor(color) {
		return models.Label{}, ErrFolderColor
	}
	if style != nil && !models.ValidFolderIconStyle(*style) {
		return models.Label{}, ErrFolderIconStyle
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return models.Label{}, err
	}
	defer tx.Rollback()
	label, err := findAppearanceTag(ctx, tx, owner, id, unified)
	if err != nil {
		return label, err
	}
	updates := map[string]string{models.TagColorKey(id, unified): strings.ToLower(color)}
	if style != nil {
		updates[models.TagStyleKey(id, unified)] = *style
	}
	for key, value := range updates {
		if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings (user_id,key,value,updated_at) VALUES (?,?,?,CURRENT_TIMESTAMP)
			ON CONFLICT(user_id,key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, owner, key, value); err != nil {
			return models.Label{}, err
		}
	}
	return label, tx.Commit()
}

// addUnifiedTagAppearances ignores stale full UI snapshots, as virtual views do.
func (db *DB) addUnifiedTagAppearances(ctx context.Context, owner string, settings map[string]string) {
	for key := range settings {
		if strings.HasPrefix(key, "unified_tag_color:") || strings.HasPrefix(key, "unified_tag_style:") || strings.HasPrefix(key, "unified_tag_order:") {
			delete(settings, key)
		}
	}
	rows, err := db.Read().QueryContext(ctx, `SELECT key,value FROM app_settings WHERE user_id=? AND (key LIKE 'unified_tag_color:%' OR key LIKE 'unified_tag_style:%' OR key LIKE 'unified_tag_order:%')`, owner)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return
		}
		if id, ok := strings.CutPrefix(key, "unified_tag_color:"); ok && models.IsUnifiedTagID(id) && models.ValidFolderColor(value) {
			settings[key] = value
		}
		if id, ok := strings.CutPrefix(key, "unified_tag_style:"); ok && models.IsUnifiedTagID(id) && models.ValidFolderIconStyle(value) {
			settings[key] = value
		}
		if id, ok := strings.CutPrefix(key, "unified_tag_order:"); ok && models.IsUnifiedTagID(id) && validOrderPlace(value) {
			settings[key] = value
		}
	}
}
