package storage

import (
	"database/sql"
	"fmt"
)

// auditTablesSQL holds the administration audit log (internal/audit): one
// row per administrative change, chained by an HMAC, and the checkpoints
// retention pruning leaves so the chain still verifies from what's kept.
const auditTablesSQL = `
CREATE TABLE IF NOT EXISTS audit_events (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    occurred_at DATETIME NOT NULL,
    actor_kind TEXT NOT NULL,
    actor_id TEXT NOT NULL DEFAULT '',
    actor_label TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    token_id TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL,
    target_kind TEXT NOT NULL DEFAULT '',
    target_id TEXT NOT NULL DEFAULT '',
    target_label TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL DEFAULT 'success' CHECK (outcome IN ('success', 'denied')),
    changes_json TEXT NOT NULL DEFAULT '{}',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    client_json TEXT NOT NULL DEFAULT '{}',
    correlation_id TEXT NOT NULL DEFAULT '',
    chain_mac TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_events_time ON audit_events(occurred_at);
CREATE INDEX IF NOT EXISTS audit_events_target ON audit_events(target_kind, target_id, seq);
CREATE INDEX IF NOT EXISTS audit_events_actor ON audit_events(actor_id, seq);
CREATE INDEX IF NOT EXISTS audit_events_action ON audit_events(action, seq);
CREATE TABLE IF NOT EXISTS audit_checkpoints (
    seq INTEGER PRIMARY KEY,
    chain_mac TEXT NOT NULL,
    created_at DATETIME NOT NULL,
    reason TEXT NOT NULL
);
`

// auditRetentionColumn keeps administration events for 730 days unless an
// administrator chooses between 90 and 3,650.
const auditRetentionColumn = "admin_audit_retention_days INTEGER NOT NULL DEFAULT 730 CHECK (admin_audit_retention_days BETWEEN 90 AND 3650)"

// migrateV148ToV149 adds the administration audit log and its retention.
func migrateV148ToV149(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 149 {
		return nil
	}
	if _, err := tx.Exec(auditTablesSQL); err != nil {
		return fmt.Errorf("create audit tables: %w", err)
	}
	hasState, err := tableExistsTx(tx, "auth_system_state")
	if err != nil {
		return fmt.Errorf("check authentication system state table: %w", err)
	}
	if hasState {
		present, err := columnExistsTx(tx, "auth_system_state", "admin_audit_retention_days")
		if err != nil {
			return fmt.Errorf("check audit retention column: %w", err)
		}
		if !present {
			if _, err := tx.Exec(`ALTER TABLE auth_system_state ADD COLUMN ` + auditRetentionColumn); err != nil {
				return fmt.Errorf("add audit retention column: %w", err)
			}
		}
	}
	if err := markSchemaVersion(tx, 149); err != nil {
		return err
	}
	return tx.Commit()
}
