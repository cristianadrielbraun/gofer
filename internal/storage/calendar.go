package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// CalendarSource is the local representation of one provider calendar. A
// source is kept even when it is not selected so a later discovery can
// preserve the user's choice.
type CalendarSource struct {
	ID          string
	UserID      string
	AccountID   string
	Provider    string
	RemoteID    string
	Name        string
	Description string
	TimeZone    string
	Color       string
	AccessRole  string
	IsPrimary   bool
	IsSelected  bool
	IsDeleted   bool
}

// ReplaceCalendarSources reconciles one provider account's discovered
// calendars. Existing selection state is preserved; new sources use the
// caller's default selection. Sources no longer returned by the provider are
// soft-deleted and deselected.
func (db *DB) ReplaceCalendarSources(ctx context.Context, userID, accountID, provider string, sources []CalendarSource) error {
	userID = strings.TrimSpace(userID)
	accountID = strings.TrimSpace(accountID)
	provider = strings.TrimSpace(provider)
	if userID == "" || accountID == "" || provider == "" {
		return fmt.Errorf("calendar source reconciliation requires user, account, and provider")
	}

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin calendar source reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var accountExists int
	if err := tx.QueryRowContext(ctx, `
		SELECT 1
		FROM accounts
		WHERE id = ? AND user_id = ? AND provider = ? AND COALESCE(is_deleting, 0) = 0`,
		accountID, userID, provider).Scan(&accountExists); err != nil {
		if err == sql.ErrNoRows {
			return sql.ErrNoRows
		}
		return fmt.Errorf("verify calendar account: %w", err)
	}

	remoteIDs := make([]string, 0, len(sources))
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		remoteID := strings.TrimSpace(source.RemoteID)
		if remoteID == "" {
			continue
		}
		if _, exists := seen[remoteID]; exists {
			return fmt.Errorf("duplicate calendar remote id %q", remoteID)
		}
		seen[remoteID] = struct{}{}
		remoteIDs = append(remoteIDs, remoteID)

		sourceID := strings.TrimSpace(source.ID)
		selected := source.IsSelected
		var existingSelected int
		err := tx.QueryRowContext(ctx, `
			SELECT id, is_selected
			FROM calendar_sources
			WHERE account_id = ? AND remote_id = ?`, accountID, remoteID).Scan(&sourceID, &existingSelected)
		if err == nil {
			selected = existingSelected == 1
		} else if err != sql.ErrNoRows {
			return fmt.Errorf("load calendar source %q: %w", remoteID, err)
		}
		if sourceID == "" {
			sourceID = uuid.NewString()
		}

		if _, err = tx.ExecContext(ctx, `
			INSERT INTO calendar_sources (
				id, user_id, account_id, provider, remote_id, name, description,
				timezone, color, access_role, is_primary, is_selected, is_deleted
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
			ON CONFLICT(account_id, remote_id) DO UPDATE SET
				user_id = excluded.user_id,
				provider = excluded.provider,
				name = excluded.name,
				description = excluded.description,
				timezone = excluded.timezone,
				color = excluded.color,
				access_role = excluded.access_role,
				is_primary = excluded.is_primary,
				is_selected = excluded.is_selected,
				is_deleted = 0,
				updated_at = CURRENT_TIMESTAMP`,
			sourceID, userID, accountID, provider, remoteID,
			strings.TrimSpace(source.Name), strings.TrimSpace(source.Description),
			strings.TrimSpace(source.TimeZone), strings.TrimSpace(source.Color), strings.TrimSpace(source.AccessRole),
			calendarBoolInt(source.IsPrimary), calendarBoolInt(selected)); err != nil {
			return fmt.Errorf("upsert calendar source %q: %w", remoteID, err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO calendar_sync_state (source_id)
			VALUES (?)
			ON CONFLICT(source_id) DO NOTHING`, sourceID); err != nil {
			return fmt.Errorf("create calendar sync state %q: %w", sourceID, err)
		}
	}

	markMissingQuery := `
		UPDATE calendar_sources
		SET is_deleted = 1, is_selected = 0, updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND account_id = ? AND provider = ?`
	markMissingArgs := []any{userID, accountID, provider}
	if len(remoteIDs) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(remoteIDs)), ",")
		markMissingQuery += ` AND remote_id NOT IN (` + placeholders + `)`
		for _, remoteID := range remoteIDs {
			markMissingArgs = append(markMissingArgs, remoteID)
		}
	}
	if _, err := tx.ExecContext(ctx, markMissingQuery, markMissingArgs...); err != nil {
		return fmt.Errorf("mark missing calendar sources: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit calendar source reconciliation: %w", err)
	}
	return nil
}

func (db *DB) ListCalendarSourcesForAccount(ctx context.Context, userID, accountID string) ([]CalendarSource, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT id, user_id, account_id, provider, remote_id, name, description,
		       timezone, color, access_role, is_primary, is_selected, is_deleted
		FROM calendar_sources
		WHERE user_id = ? AND account_id = ? AND is_deleted = 0
		ORDER BY is_primary DESC, name COLLATE NOCASE, remote_id`, userID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sources []CalendarSource
	for rows.Next() {
		var source CalendarSource
		var isPrimary, isSelected, isDeleted int
		if err := rows.Scan(
			&source.ID, &source.UserID, &source.AccountID, &source.Provider,
			&source.RemoteID, &source.Name, &source.Description, &source.TimeZone,
			&source.Color, &source.AccessRole, &isPrimary, &isSelected, &isDeleted,
		); err != nil {
			return nil, err
		}
		source.IsPrimary = isPrimary == 1
		source.IsSelected = isSelected == 1
		source.IsDeleted = isDeleted == 1
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

func calendarBoolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
