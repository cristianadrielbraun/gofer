package storage

import (
	"context"
	"database/sql"
	"fmt"
)

const calendarSchemaV94 = `
CREATE TABLE IF NOT EXISTS calendar_sources (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    provider TEXT NOT NULL CHECK (provider IN ('gmail', 'outlook')),
    remote_id TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    timezone TEXT NOT NULL DEFAULT '',
    color TEXT NOT NULL DEFAULT '',
    access_role TEXT NOT NULL DEFAULT '',
    is_primary INTEGER NOT NULL DEFAULT 0 CHECK (is_primary IN (0, 1)),
    is_selected INTEGER NOT NULL DEFAULT 1 CHECK (is_selected IN (0, 1)),
    is_deleted INTEGER NOT NULL DEFAULT 0 CHECK (is_deleted IN (0, 1)),
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (account_id, remote_id)
);

CREATE INDEX IF NOT EXISTS idx_calendar_sources_user
ON calendar_sources(user_id, is_deleted, name COLLATE NOCASE);

CREATE INDEX IF NOT EXISTS idx_calendar_sources_account
ON calendar_sources(account_id, is_deleted, name COLLATE NOCASE);

CREATE TABLE IF NOT EXISTS calendar_events (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source_id TEXT NOT NULL REFERENCES calendar_sources(id) ON DELETE CASCADE,
    remote_id TEXT NOT NULL,
    ical_uid TEXT NOT NULL DEFAULT '',
    series_remote_id TEXT NOT NULL DEFAULT '',
    etag TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'confirmed' CHECK (status IN ('confirmed', 'tentative', 'cancelled')),
    summary TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    location TEXT NOT NULL DEFAULT '',
    organizer_name TEXT NOT NULL DEFAULT '',
    organizer_email TEXT NOT NULL DEFAULT '',
    all_day INTEGER NOT NULL DEFAULT 0 CHECK (all_day IN (0, 1)),
    start_date TEXT,
    end_date TEXT,
    start_at DATETIME,
    end_at DATETIME,
    start_timezone TEXT NOT NULL DEFAULT '',
    end_timezone TEXT NOT NULL DEFAULT '',
    recurrence_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(recurrence_json)),
    attendees_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(attendees_json)),
    online_meeting_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(online_meeting_json)),
    html_link TEXT NOT NULL DEFAULT '',
    provider_created_at DATETIME,
    provider_updated_at DATETIME,
    is_deleted INTEGER NOT NULL DEFAULT 0 CHECK (is_deleted IN (0, 1)),
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (source_id, remote_id),
    CHECK (
        is_deleted = 1
        OR (
            all_day = 1
            AND start_date IS NOT NULL
            AND end_date IS NOT NULL
            AND start_at IS NULL
            AND end_at IS NULL
        )
        OR (
            all_day = 0
            AND start_date IS NULL
            AND end_date IS NULL
            AND start_at IS NOT NULL
            AND end_at IS NOT NULL
        )
    )
);

CREATE INDEX IF NOT EXISTS idx_calendar_events_user_start
ON calendar_events(user_id, is_deleted, start_at, start_date, id);

CREATE INDEX IF NOT EXISTS idx_calendar_events_source_start
ON calendar_events(source_id, is_deleted, start_at, start_date, id);

CREATE INDEX IF NOT EXISTS idx_calendar_events_series
ON calendar_events(source_id, series_remote_id, is_deleted);

CREATE TABLE IF NOT EXISTS calendar_sync_state (
    source_id TEXT PRIMARY KEY REFERENCES calendar_sources(id) ON DELETE CASCADE,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'syncing', 'ok', 'failed')),
    cursor_kind TEXT NOT NULL DEFAULT '',
    cursor_value TEXT NOT NULL DEFAULT '',
    window_start DATETIME,
    window_end DATETIME,
    full_sync_required INTEGER NOT NULL DEFAULT 1 CHECK (full_sync_required IN (0, 1)),
    attempt_count INTEGER NOT NULL DEFAULT 0,
    last_started_at DATETIME,
    last_success_at DATETIME,
    next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_error TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_calendar_sync_state_due
ON calendar_sync_state(state, next_attempt_at, updated_at);
`

// migrateV93ToV94 adds the provider-neutral Calendar cache. Existing
// accounts do not receive calendar sources until a provider authorization and
// initial calendar discovery succeeds.
func migrateV93ToV94(tx *sql.Tx) error {
	if _, err := tx.Exec(calendarSchemaV94); err != nil {
		return fmt.Errorf("create calendar schema: %w", err)
	}
	return markSchemaVersion(tx, 94)
}

// migrateV94ToV95 permits CalDAV-backed calendars and stores each mailbox's
// CalDAV endpoint.
func migrateV94ToV95(db *sql.DB) (returnErr error) {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()

	var currentVersion int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&currentVersion); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if currentVersion >= 95 {
		return nil
	}
	if currentVersion != 94 {
		return fmt.Errorf("expected schema version 94, found %d", currentVersion)
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
		return fmt.Errorf("begin CalDAV schema migration: %w", err)
	}
	defer tx.Rollback()

	statements := []string{
		`CREATE TABLE calendar_sources_v95 (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			provider TEXT NOT NULL CHECK (provider IN ('gmail', 'outlook', 'caldav')),
			remote_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			timezone TEXT NOT NULL DEFAULT '',
			color TEXT NOT NULL DEFAULT '',
			access_role TEXT NOT NULL DEFAULT '',
			is_primary INTEGER NOT NULL DEFAULT 0 CHECK (is_primary IN (0, 1)),
			is_selected INTEGER NOT NULL DEFAULT 1 CHECK (is_selected IN (0, 1)),
			is_deleted INTEGER NOT NULL DEFAULT 0 CHECK (is_deleted IN (0, 1)),
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE (account_id, remote_id)
		)`,
		`INSERT INTO calendar_sources_v95 (
			id, user_id, account_id, provider, remote_id, name, description,
			timezone, color, access_role, is_primary, is_selected, is_deleted,
			created_at, updated_at
		) SELECT id, user_id, account_id, provider, remote_id, name, description,
			timezone, color, access_role, is_primary, is_selected, is_deleted,
			created_at, updated_at FROM calendar_sources`,
		`DROP TABLE calendar_sources`,
		`ALTER TABLE calendar_sources_v95 RENAME TO calendar_sources`,
		`CREATE INDEX idx_calendar_sources_user ON calendar_sources(user_id, is_deleted, name COLLATE NOCASE)`,
		`CREATE INDEX idx_calendar_sources_account ON calendar_sources(account_id, is_deleted, name COLLATE NOCASE)`,
		`CREATE TABLE IF NOT EXISTS account_caldav_configs (
			account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			base_url TEXT NOT NULL CHECK (trim(base_url) <> ''),
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_account_caldav_configs_user ON account_caldav_configs(user_id, account_id)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (95)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply CalDAV schema statement: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit CalDAV schema migration: %w", err)
	}

	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check migrated foreign keys: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var table, parent string
		var rowID sql.NullInt64
		var fkID int
		if err := rows.Scan(&table, &rowID, &parent, &fkID); err != nil {
			return fmt.Errorf("read migrated foreign key violation: %w", err)
		}
		return fmt.Errorf("foreign key violation after CalDAV migration: %s row %v references %s (constraint %d)", table, rowID, parent, fkID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read migrated foreign key check: %w", err)
	}
	return nil
}

// migrateV95ToV96 stores an optional CalDAV-specific username and encrypted
// password while defaulting existing accounts to their mailbox credentials.
func migrateV95ToV96(db *sql.DB) error {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin CalDAV credentials migration: %w", err)
	}
	defer tx.Rollback()

	var currentVersion int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&currentVersion); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if currentVersion >= 96 {
		return nil
	}
	if currentVersion != 95 {
		return fmt.Errorf("expected schema version 95, found %d", currentVersion)
	}
	for _, statement := range []string{
		`ALTER TABLE account_caldav_configs ADD COLUMN username TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE account_caldav_configs ADD COLUMN encrypted_password BLOB`,
		`ALTER TABLE account_caldav_configs ADD COLUMN use_account_credentials INTEGER NOT NULL DEFAULT 1 CHECK (use_account_credentials IN (0, 1))`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("add CalDAV credential setting: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO schema_version (version) VALUES (96)`); err != nil {
		return fmt.Errorf("record CalDAV credentials schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit CalDAV credentials migration: %w", err)
	}
	return nil
}

// migrateV96ToV97 separates Calendar display visibility from sync selection.
// Existing calendars remain visible and all source/cache identities are kept.
func migrateV96ToV97(db *sql.DB) error {
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("begin calendar visibility migration: %w", err)
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 97 {
		return nil
	}
	if version != 96 {
		return fmt.Errorf("expected schema version 96, found %d", version)
	}
	if _, err := tx.Exec(`ALTER TABLE calendar_sources ADD COLUMN is_hidden INTEGER NOT NULL DEFAULT 0 CHECK (is_hidden IN (0, 1))`); err != nil {
		return fmt.Errorf("add calendar visibility: %w", err)
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO schema_version (version) VALUES (97)`); err != nil {
		return err
	}
	return tx.Commit()
}
