package storage

import (
	"context"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/google/uuid"
)

func (db *DB) ListContactSyncMemberships(ctx context.Context, userID, profileID string) ([]models.ContactSyncMembership, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT id, user_id, profile_id, account_id, address_book_id, enabled, status, last_error
		FROM contact_sync_memberships
		WHERE user_id = ? AND profile_id = ?
		ORDER BY enabled DESC, account_id, address_book_id`, userID, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var memberships []models.ContactSyncMembership
	for rows.Next() {
		var membership models.ContactSyncMembership
		var enabled int
		if err := rows.Scan(&membership.ID, &membership.UserID, &membership.ProfileID, &membership.AccountID, &membership.AddressBookID, &enabled, &membership.Status, &membership.LastError); err != nil {
			return nil, err
		}
		membership.Enabled = enabled == 1
		memberships = append(memberships, membership)
	}
	return memberships, rows.Err()
}

// ReplaceContactSyncMemberships changes Gofer's replication targets. Local is
// represented by a local card; external targets are memberships. Disabling an
// external target never deletes its existing provider card.
func (db *DB) ReplaceContactSyncMemberships(ctx context.Context, userID, profileID string, targets []string) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	normalizedTargets := normalizeContactSaveTargets(targets)
	localSelected := false
	for _, target := range normalizedTargets {
		if target == "local" {
			localSelected = true
			break
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE contact_cards SET is_deleted = ?, updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND profile_id = ? AND kind = 'local'`, boolInt(!localSelected), userID, profileID); err != nil {
		return err
	}
	if localSelected {
		var localCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_cards WHERE user_id = ? AND profile_id = ? AND kind = 'local'`, userID, profileID).Scan(&localCount); err != nil {
			return err
		}
		if localCount == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO contact_cards (id, user_id, profile_id, kind) VALUES (?, ?, ?, 'local')`, uuid.NewString(), userID, profileID); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE contact_sync_memberships SET enabled = 0, status = 'stopped', last_error = '', updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND profile_id = ?`, userID, profileID); err != nil {
		return err
	}
	for _, target := range normalizedTargets {
		var accountID, bookID string
		switch {
		case strings.HasPrefix(target, "account:"):
			accountID = strings.TrimSpace(strings.TrimPrefix(target, "account:"))
		case strings.HasPrefix(target, "book:"):
			bookID = strings.TrimSpace(strings.TrimPrefix(target, "book:"))
			_ = tx.QueryRowContext(ctx, `SELECT account_id FROM account_contact_address_books WHERE user_id = ? AND id = ?`, userID, bookID).Scan(&accountID)
		default:
			continue
		}
		if accountID == "" && bookID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO contact_sync_memberships (id, user_id, profile_id, account_id, address_book_id, enabled, status, last_error)
			VALUES (?, ?, ?, ?, ?, 1, 'active', '')
			ON CONFLICT(user_id, profile_id, account_id, address_book_id) DO UPDATE SET
				enabled = 1, status = 'active', last_error = '', updated_at = CURRENT_TIMESTAMP`,
			uuid.NewString(), userID, profileID, accountID, bookID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
