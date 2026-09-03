package storage

import (
	"database/sql"
	"fmt"
)

// migrateV92ToV93 adds the bounded administrator-configurable authentication
// event retention policy. Existing instances receive the 180-day default.
func migrateV92ToV93(tx *sql.Tx) error {
	hasState, err := tableExistsTx(tx, "auth_system_state")
	if err != nil {
		return fmt.Errorf("check authentication system state table: %w", err)
	}
	if hasState {
		columns := []struct {
			name       string
			definition string
		}{
			{
				name:       "auth_event_retention_days",
				definition: "INTEGER NOT NULL DEFAULT 180 CHECK (auth_event_retention_days BETWEEN 1 AND 365)",
			},
			{name: "auth_event_retention_updated_at", definition: "DATETIME"},
			{
				name:       "auth_event_retention_updated_by",
				definition: "TEXT REFERENCES users(id) ON DELETE SET NULL",
			},
		}
		for _, column := range columns {
			exists, err := columnExistsTx(tx, "auth_system_state", column.name)
			if err != nil {
				return fmt.Errorf("check auth event retention column %s: %w", column.name, err)
			}
			if exists {
				continue
			}
			if _, err := tx.Exec(`ALTER TABLE auth_system_state ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
				return fmt.Errorf("add auth event retention column %s: %w", column.name, err)
			}
		}
	}

	hasEvents, err := tableExistsTx(tx, "auth_events")
	if err != nil {
		return fmt.Errorf("check authentication events table: %w", err)
	}
	if hasEvents {
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_auth_events_occurred
			ON auth_events(occurred_at DESC, id DESC)`); err != nil {
			return fmt.Errorf("create authentication event retention index: %w", err)
		}
	}
	return markSchemaVersion(tx, 93)
}
