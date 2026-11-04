package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const installationBrandingTableSQL = `CREATE TABLE IF NOT EXISTS installation_branding (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    name TEXT NOT NULL DEFAULT '',
    logo BLOB,
    logo_type TEXT NOT NULL DEFAULT '',
    favicon BLOB,
    favicon_type TEXT NOT NULL DEFAULT '',
    updated_at DATETIME,
    updated_by TEXT NOT NULL DEFAULT ''
)`

type Branding struct {
	Name        string
	Logo        []byte
	LogoType    string
	Favicon     []byte
	FaviconType string
	UpdatedAt   time.Time
	UpdatedBy   string
}

// migrateV136ToV137 adds the installation branding table.
func migrateV136ToV137(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 137 {
		return nil
	}
	if _, err := tx.Exec(installationBrandingTableSQL); err != nil {
		return err
	}
	if err := markSchemaVersion(tx, 137); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) GetBranding(ctx context.Context) (Branding, error) {
	var branding Branding
	var updatedAt sql.NullTime
	err := db.Read().QueryRowContext(ctx, `
		SELECT name, COALESCE(logo, x''), logo_type, COALESCE(favicon, x''), favicon_type, updated_at, updated_by
		FROM installation_branding WHERE id = 1`,
	).Scan(&branding.Name, &branding.Logo, &branding.LogoType, &branding.Favicon, &branding.FaviconType, &updatedAt, &branding.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Branding{}, nil
	}
	if err != nil {
		return Branding{}, err
	}
	if updatedAt.Valid {
		branding.UpdatedAt = updatedAt.Time
	}
	return branding, nil
}

func (db *DB) SaveBranding(ctx context.Context, branding Branding) error {
	var logo, favicon any
	if len(branding.Logo) > 0 {
		logo = branding.Logo
	}
	if len(branding.Favicon) > 0 {
		favicon = branding.Favicon
	}
	if branding.UpdatedAt.IsZero() {
		branding.UpdatedAt = time.Now().UTC()
	}
	_, err := db.Write().ExecContext(ctx, `
		INSERT INTO installation_branding (id, name, logo, logo_type, favicon, favicon_type, updated_at, updated_by)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name, logo = excluded.logo, logo_type = excluded.logo_type,
			favicon = excluded.favicon, favicon_type = excluded.favicon_type,
			updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		branding.Name, logo, branding.LogoType, favicon, branding.FaviconType, branding.UpdatedAt, branding.UpdatedBy)
	return err
}
