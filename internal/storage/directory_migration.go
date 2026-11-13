package storage

import (
	"database/sql"
	"fmt"
)

// directoryTablesSQL holds the people's groups (internal/directory), who is
// in each, and the revision counters that tell what was worked out from
// them that it is out of date.
const directoryTablesSQL = `
CREATE TABLE IF NOT EXISTS directory_groups (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    name_normalized TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'local' CHECK (source IN ('local', 'scim', 'oidc', 'ldap')),
    external_id TEXT,
    created_at DATETIME NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    version INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1)
);
CREATE UNIQUE INDEX IF NOT EXISTS directory_groups_external
    ON directory_groups(source, external_id) WHERE external_id IS NOT NULL;
CREATE TABLE IF NOT EXISTS directory_group_members (
    group_id TEXT NOT NULL REFERENCES directory_groups(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source TEXT NOT NULL DEFAULT 'manual' CHECK (source IN ('manual', 'external', 'rule')),
    added_at DATETIME NOT NULL,
    added_by TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (group_id, user_id)
);
CREATE INDEX IF NOT EXISTS directory_group_members_user ON directory_group_members(user_id);
CREATE TABLE IF NOT EXISTS admin_revisions (
    name TEXT PRIMARY KEY,
    revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0)
);
`

// migrateV149ToV150 adds groups of people and the administration revisions.
func migrateV149ToV150(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 150 {
		return nil
	}
	if _, err := tx.Exec(directoryTablesSQL); err != nil {
		return fmt.Errorf("create directory tables: %w", err)
	}
	if err := markSchemaVersion(tx, 150); err != nil {
		return err
	}
	return tx.Commit()
}
