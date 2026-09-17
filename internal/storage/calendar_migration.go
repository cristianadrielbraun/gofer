package storage

import (
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
