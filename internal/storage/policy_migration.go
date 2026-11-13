package storage

import (
	"database/sql"
	"fmt"
)

// policyTablesSQL holds the policies (internal/policy): each a named set
// of setting values, applied in priority order to the groups and people
// it's assigned to, with the Default policy last and to everyone.
const policyTablesSQL = `
CREATE TABLE IF NOT EXISTS policies (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    name_normalized TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0, 1)),
    priority INTEGER CHECK (priority IS NULL OR priority >= 1),
    created_at DATETIME NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    version INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
    CHECK ((is_default = 1) = (priority IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS policies_one_default ON policies(is_default) WHERE is_default = 1;
CREATE UNIQUE INDEX IF NOT EXISTS policies_priority ON policies(priority) WHERE priority IS NOT NULL;
CREATE TABLE IF NOT EXISTS policy_values (
    policy_id TEXT NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
    setting_key TEXT NOT NULL,
    value_json TEXT NOT NULL,
    locked INTEGER NOT NULL DEFAULT 0 CHECK (locked IN (0, 1)),
    enforced INTEGER NOT NULL DEFAULT 0 CHECK (enforced IN (0, 1)),
    updated_at DATETIME NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (policy_id, setting_key)
);
CREATE TABLE IF NOT EXISTS policy_assignments (
    policy_id TEXT NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
    target_kind TEXT NOT NULL CHECK (target_kind IN ('group', 'user')),
    target_id TEXT NOT NULL,
    created_at DATETIME NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (policy_id, target_kind, target_id)
);
CREATE INDEX IF NOT EXISTS policy_assignments_target ON policy_assignments(target_kind, target_id);
`

// migrateV150ToV151 adds the policies. The Default policy's row is made
// the first time something is saved to it (internal/policy); until then it
// sets nothing, so every setting keeps the value it has today.
func migrateV150ToV151(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 151 {
		return nil
	}
	if _, err := tx.Exec(policyTablesSQL); err != nil {
		return fmt.Errorf("create policy tables: %w", err)
	}
	if err := markSchemaVersion(tx, 151); err != nil {
		return err
	}
	return tx.Commit()
}
