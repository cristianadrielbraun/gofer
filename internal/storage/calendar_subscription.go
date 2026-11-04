package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/google/uuid"
)

// CalendarSourceProviderICS is a calendar subscription: an iCalendar feed
// read from its URL, which Gofer can't change. Subscriptions belong to the
// user's Local folders account, since no account of theirs serves them, and
// their remote id is the feed's URL.
const CalendarSourceProviderICS = "ics"

const CalendarSubscriptionDefaultRefresh = models.CalendarSubscriptionDefaultRefresh

// ErrCalendarSubscriptionExists is a feed the user already subscribes to.
var ErrCalendarSubscriptionExists = errors.New("You already subscribe to this calendar.")

// ValidCalendarSubscriptionRefresh reports whether minutes is one of the
// refresh choices.
func ValidCalendarSubscriptionRefresh(minutes int) bool {
	for _, choice := range models.CalendarSubscriptionRefreshChoices {
		if minutes == choice {
			return true
		}
	}
	return false
}

// CalendarSubscription is what the user chooses for a subscription.
type CalendarSubscription struct {
	URL, Name, Color string
	RefreshMinutes   int
}

// AddCalendarSubscription adds a subscription to the account, the user's
// Local folders, synced and due for its first read at once.
func (db *DB) AddCalendarSubscription(ctx context.Context, userID, accountID string, subscription CalendarSubscription) (CalendarSource, error) {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return CalendarSource{}, err
	}
	defer tx.Rollback()
	if err := calendarSubscriptionAccountTx(ctx, tx, userID, accountID); err != nil {
		return CalendarSource{}, err
	}
	var existing string
	var deleted bool
	err = tx.QueryRowContext(ctx, `SELECT id, is_deleted = 1 OR is_selected = 0 FROM calendar_sources WHERE account_id = ? AND remote_id = ?`, accountID, subscription.URL).Scan(&existing, &deleted)
	switch {
	case err == nil && !deleted:
		return CalendarSource{}, ErrCalendarSubscriptionExists
	case err == nil:
		// A leftover row of the same feed: start it over.
		if _, err := tx.ExecContext(ctx, `DELETE FROM calendar_sources WHERE id = ?`, existing); err != nil {
			return CalendarSource{}, err
		}
	case !errors.Is(err, sql.ErrNoRows):
		return CalendarSource{}, err
	}
	source := CalendarSource{
		ID: uuid.NewString(), UserID: userID, AccountID: accountID, Provider: CalendarSourceProviderICS,
		RemoteID: subscription.URL, Name: subscription.Name, Color: subscription.Color, AccessRole: "reader",
		IsSelected: true, RefreshMinutes: subscription.RefreshMinutes, SyncState: "pending",
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO calendar_sources (id, user_id, account_id, provider, remote_id, name, color, access_role, is_selected, refresh_minutes)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'reader', 1, ?)`,
		source.ID, userID, accountID, CalendarSourceProviderICS, subscription.URL, subscription.Name, subscription.Color, subscription.RefreshMinutes); err != nil {
		return CalendarSource{}, fmt.Errorf("add calendar subscription: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO calendar_sync_state (source_id) VALUES (?)`, source.ID); err != nil {
		return CalendarSource{}, fmt.Errorf("add calendar subscription sync state: %w", err)
	}
	return source, tx.Commit()
}

// UpdateCalendarSubscription changes a subscription. A new URL is another
// feed: its events are dropped and it is read again at once.
func (db *DB) UpdateCalendarSubscription(ctx context.Context, userID, sourceID string, subscription CalendarSubscription) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var accountID, url string
	if err := tx.QueryRowContext(ctx, `SELECT account_id, remote_id FROM calendar_sources
		WHERE id = ? AND user_id = ? AND provider = ? AND is_deleted = 0 AND is_selected = 1`,
		sourceID, userID, CalendarSourceProviderICS).Scan(&accountID, &url); err != nil {
		return err
	}
	if url != subscription.URL {
		var other int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_sources WHERE account_id = ? AND remote_id = ? AND id <> ? AND is_deleted = 0 AND is_selected = 1`,
			accountID, subscription.URL, sourceID).Scan(&other); err != nil {
			return err
		}
		if other > 0 {
			return ErrCalendarSubscriptionExists
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM calendar_sources WHERE account_id = ? AND remote_id = ? AND id <> ?`, accountID, subscription.URL, sourceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM calendar_events WHERE source_id = ?`, sourceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE calendar_sync_state SET state = 'pending', attempt_count = 0, last_error = '',
			last_success_at = NULL, window_start = NULL, window_end = NULL, full_sync_required = 1,
			next_attempt_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE source_id = ?`, sourceID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE calendar_sources SET remote_id = ?, name = ?, color = ?, refresh_minutes = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		subscription.URL, subscription.Name, subscription.Color, subscription.RefreshMinutes, sourceID); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveCalendarSubscription removes a subscription with its events.
func (db *DB) RemoveCalendarSubscription(ctx context.Context, userID, sourceID string) error {
	result, err := db.Write().ExecContext(ctx, `DELETE FROM calendar_sources WHERE id = ? AND user_id = ? AND provider = ?`, sourceID, userID, CalendarSourceProviderICS)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func calendarSubscriptionAccountTx(ctx context.Context, tx *sql.Tx, userID, accountID string) error {
	var provider string
	err := tx.QueryRowContext(ctx, `SELECT provider FROM accounts WHERE id = ? AND user_id IS ? AND COALESCE(is_deleting, 0) = 0`, accountID, nullableUser(userID)).Scan(&provider)
	if err != nil {
		return err
	}
	if provider != localProvider {
		return errors.New("calendar subscriptions belong to Local folders")
	}
	return nil
}

// migrateV135ToV136 lets calendars be subscriptions to iCalendar feeds, read
// again every refresh_minutes. The provider CHECK can only change by
// rebuilding calendar_sources; the tables that refer to it keep their rows,
// since foreign keys are off while it is replaced.
func migrateV135ToV136(db *sql.DB) (returnErr error) {
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
	if version >= 136 {
		return nil
	}
	var definition string
	err = conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = 'calendar_sources'`).Scan(&definition)
	if err == sql.ErrNoRows || (err == nil && strings.Contains(definition, "'ics'")) {
		// Some historical databases checked by upgrade tests have no
		// calendars; a table made from the current schema already allows ics.
		_, err := conn.ExecContext(ctx, `INSERT OR REPLACE INTO schema_version (version) VALUES (136)`)
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
		return fmt.Errorf("begin calendar subscription migration: %w", err)
	}
	defer tx.Rollback()
	var rows int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_sources`).Scan(&rows); err != nil {
		return err
	}
	statements := []string{
		`CREATE TABLE calendar_sources_v136 (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			provider TEXT NOT NULL CHECK (provider IN ('gmail', 'outlook', 'caldav', 'jmap', 'ics')),
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
			created_in_gofer INTEGER NOT NULL DEFAULT 0,
			refresh_minutes INTEGER NOT NULL DEFAULT 0,
			UNIQUE (account_id, remote_id)
		)`,
		`INSERT INTO calendar_sources_v136 (
			id, user_id, account_id, provider, remote_id, name, description,
			timezone, color, access_role, is_primary, is_selected, is_hidden, is_deleted,
			created_at, updated_at, created_in_gofer
		) SELECT id, user_id, account_id, provider, remote_id, name, description,
			timezone, color, access_role, is_primary, is_selected, is_hidden, is_deleted,
			created_at, updated_at, created_in_gofer FROM calendar_sources`,
		`DROP TABLE calendar_sources`,
		`ALTER TABLE calendar_sources_v136 RENAME TO calendar_sources`,
		`CREATE INDEX idx_calendar_sources_user ON calendar_sources(user_id, is_deleted, name COLLATE NOCASE)`,
		`CREATE INDEX idx_calendar_sources_account ON calendar_sources(account_id, is_deleted, name COLLATE NOCASE)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (136)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply calendar subscription schema statement: %w", err)
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
		return fmt.Errorf("commit calendar subscription migration: %w", err)
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
		return fmt.Errorf("foreign key violation after calendar subscription migration: %s row %v references %s (constraint %d)", table, rowID, parent, fkID)
	}
	return check.Err()
}
