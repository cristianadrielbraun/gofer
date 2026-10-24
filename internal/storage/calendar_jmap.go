package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// CalendarSourceProviderJMAP is a calendar read over a JMAP account's own
// connection (JMAP for Calendars) instead of CalDAV.
const CalendarSourceProviderJMAP = "jmap"

// CalendarSourceMailServer reports whether calendars of this provider belong
// to a mail-server account (IMAP or JMAP), which uses one of them at a time
// and signs in with a password rather than OAuth.
func CalendarSourceMailServer(provider string) bool {
	return provider == CalendarSourceProviderCalDAV || provider == CalendarSourceProviderJMAP
}

// migrateV126ToV127 lets calendars be JMAP calendars. The provider CHECK can
// only change by rebuilding calendar_sources; the tables that refer to it
// keep their rows, since foreign keys are off while it is replaced.
func migrateV126ToV127(db *sql.DB) (returnErr error) {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()

	var version int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version >= 127 {
		return nil
	}
	var definition string
	err = conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = 'calendar_sources'`).Scan(&definition)
	if err == sql.ErrNoRows || (err == nil && strings.Contains(definition, "'jmap'")) {
		// Some historical databases checked by upgrade tests have no
		// calendars; a table made from the current schema already allows jmap.
		_, err := conn.ExecContext(ctx, `INSERT OR REPLACE INTO schema_version (version) VALUES (127)`)
		return err
	}
	if err != nil {
		return fmt.Errorf("read calendar_sources: %w", err)
	}

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA legacy_alter_table = ON`); err != nil {
		_, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
		return fmt.Errorf("enable legacy alter table mode: %w", err)
	}
	defer func() {
		_, legacyErr := conn.ExecContext(ctx, `PRAGMA legacy_alter_table = OFF`)
		_, foreignKeysErr := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
		if returnErr == nil {
			if legacyErr != nil {
				returnErr = fmt.Errorf("disable legacy alter table mode: %w", legacyErr)
			} else if foreignKeysErr != nil {
				returnErr = fmt.Errorf("restore foreign keys: %w", foreignKeysErr)
			}
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin JMAP calendar migration: %w", err)
	}
	defer tx.Rollback()
	var rows int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_sources`).Scan(&rows); err != nil {
		return err
	}
	statements := []string{
		`CREATE TABLE calendar_sources_v127 (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			provider TEXT NOT NULL CHECK (provider IN ('gmail', 'outlook', 'caldav', 'jmap')),
			remote_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			timezone TEXT NOT NULL DEFAULT '',
			color TEXT NOT NULL DEFAULT '',
			access_role TEXT NOT NULL DEFAULT '',
			is_primary INTEGER NOT NULL DEFAULT 0 CHECK (is_primary IN (0, 1)),
			is_selected INTEGER NOT NULL DEFAULT 1 CHECK (is_selected IN (0, 1)),
			is_hidden INTEGER NOT NULL DEFAULT 0 CHECK (is_hidden IN (0, 1)),
			is_deleted INTEGER NOT NULL DEFAULT 0 CHECK (is_deleted IN (0, 1)),
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE (account_id, remote_id)
		)`,
		`INSERT INTO calendar_sources_v127 (
			id, user_id, account_id, provider, remote_id, name, description,
			timezone, color, access_role, is_primary, is_selected, is_hidden, is_deleted,
			created_at, updated_at
		) SELECT id, user_id, account_id, provider, remote_id, name, description,
			timezone, color, access_role, is_primary, is_selected, is_hidden, is_deleted,
			created_at, updated_at FROM calendar_sources`,
		`DROP TABLE calendar_sources`,
		`ALTER TABLE calendar_sources_v127 RENAME TO calendar_sources`,
		`CREATE INDEX idx_calendar_sources_user ON calendar_sources(user_id, is_deleted, name COLLATE NOCASE)`,
		`CREATE INDEX idx_calendar_sources_account ON calendar_sources(account_id, is_deleted, name COLLATE NOCASE)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (127)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply JMAP calendar schema statement: %w", err)
		}
	}
	var kept int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_sources`).Scan(&kept); err != nil {
		return err
	}
	if kept != rows {
		return fmt.Errorf("rebuilding calendar_sources kept %d of %d rows", kept, rows)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit JMAP calendar migration: %w", err)
	}

	check, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check migrated foreign keys: %w", err)
	}
	defer check.Close()
	if check.Next() {
		var table, parent string
		var rowID sql.NullInt64
		var fkID int
		if err := check.Scan(&table, &rowID, &parent, &fkID); err != nil {
			return fmt.Errorf("read migrated foreign key violation: %w", err)
		}
		return fmt.Errorf("foreign key violation after JMAP calendar migration: %s row %v references %s (constraint %d)", table, rowID, parent, fkID)
	}
	return check.Err()
}
