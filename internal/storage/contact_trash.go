package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// A contact in the Trash is a deleted profile with trashed_at set. It keeps
// its cards, so it still exists in the address books it syncs with, and a
// restore brings it back as it was. Deleting it for good removes it from
// those address books and clears trashed_at, leaving the deleted profile
// as deletion always has.

// ContactTrashRetention is how long a contact stays in the Trash before it
// is deleted for good.
const ContactTrashRetention = models.ContactTrashRetentionDays * 24 * time.Hour

// contactTrashDates formats when a contact went to the Trash, as stored,
// and when it is deleted for good from there.
func contactTrashDates(ctx context.Context, raw string, loc *time.Location) (trashed, purge string) {
	t, ok := parseSQLiteDateTime(raw)
	if !ok {
		return raw, ""
	}
	if loc == nil {
		loc = time.Local
	}
	dates := i18n.Dates(ctx)
	return dates.Date(t.In(loc)), dates.Date(t.Add(ContactTrashRetention).In(loc))
}

// migrateV122ToV123 records when a contact went to the Trash.
func migrateV122ToV123(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 123 {
		return nil
	}
	exists, err := tableExistsTx(tx, "contact_profiles")
	if err != nil {
		return err
	}
	if exists {
		present, err := columnExistsTx(tx, "contact_profiles", "trashed_at")
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.Exec(`ALTER TABLE contact_profiles ADD COLUMN trashed_at DATETIME`); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_contact_profiles_trash ON contact_profiles(user_id, trashed_at) WHERE trashed_at IS NOT NULL`); err != nil {
			return err
		}
	}
	if err := markSchemaVersion(tx, 123); err != nil {
		return err
	}
	return tx.Commit()
}

// TrashContacts moves the user's contacts to the Trash. Nothing is sent to
// their address books, and pushes still waiting for them stop. It gives how
// many went; ids that aren't the user's contacts, or are already gone, are
// left out.
func (db *DB) TrashContacts(ctx context.Context, userID string, ids []string) (int, error) {
	return db.moveContactsToTrash(ctx, userID, ids, nil)
}

// TrashUserContacts is TrashContacts in per-user storage, under the owner's
// guard.
func (db *DB) TrashUserContacts(ctx context.Context, owner string, ids []string, guard func(*sql.Tx) error) (int, error) {
	if guard == nil {
		return 0, ErrContactSyncSuperseded
	}
	return db.moveContactsToTrash(ctx, owner, ids, guard)
}

func (db *DB) moveContactsToTrash(ctx context.Context, userID string, ids []string, guard func(*sql.Tx) error) (int, error) {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if guard != nil {
		if err := contactSyncOwnerTx(ctx, tx, userID, guard); err != nil {
			return 0, err
		}
	}
	trashed := 0
	for _, id := range ids {
		id = strings.TrimSpace(id)
		result, err := tx.ExecContext(ctx, `
			UPDATE contact_profiles SET is_deleted = 1, trashed_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
			WHERE user_id = ? AND id = ? AND is_deleted = 0`, userID, id)
		if err != nil {
			return 0, err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			continue
		}
		trashed++
		if _, err := tx.ExecContext(ctx, `
			UPDATE contact_sync_operations SET status = 'done', locked_at = NULL, last_error = '', updated_at = CURRENT_TIMESTAMP
			WHERE user_id = ? AND contact_id = ? AND status IN ('pending', 'running')`, userID, id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if trashed > 0 {
		_ = db.LogContactActivity(ctx, userID, "contacts_trashed", "", "Contacts moved to Trash", trashed)
	}
	return trashed, nil
}

// RestoreContacts brings the user's contacts back from the Trash, as they
// were and still linked to their address books. It gives how many came back.
func (db *DB) RestoreContacts(ctx context.Context, userID string, ids []string) (int, error) {
	return db.restoreContacts(ctx, userID, ids, nil)
}

// RestoreUserContacts is RestoreContacts in per-user storage, under the
// owner's guard.
func (db *DB) RestoreUserContacts(ctx context.Context, owner string, ids []string, guard func(*sql.Tx) error) (int, error) {
	if guard == nil {
		return 0, ErrContactSyncSuperseded
	}
	return db.restoreContacts(ctx, owner, ids, guard)
}

func (db *DB) restoreContacts(ctx context.Context, userID string, ids []string, guard func(*sql.Tx) error) (int, error) {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if guard != nil {
		if err := contactSyncOwnerTx(ctx, tx, userID, guard); err != nil {
			return 0, err
		}
	}
	restored := 0
	for _, id := range ids {
		result, err := tx.ExecContext(ctx, `
			UPDATE contact_profiles SET is_deleted = 0, trashed_at = NULL, updated_at = CURRENT_TIMESTAMP
			WHERE user_id = ? AND id = ? AND is_deleted = 1 AND trashed_at IS NOT NULL`, userID, strings.TrimSpace(id))
		if err != nil {
			return 0, err
		}
		if count, _ := result.RowsAffected(); count == 1 {
			restored++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if restored > 0 {
		_ = db.LogContactActivity(ctx, userID, "contacts_restored", "", "Contacts restored from Trash", restored)
	}
	return restored, nil
}

// ContactTrashOwners gives the users with contacts that went to the Trash
// before cutoff.
func (db *DB) ContactTrashOwners(ctx context.Context, cutoff time.Time) ([]string, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT DISTINCT user_id FROM contact_profiles
		WHERE is_deleted = 1 AND trashed_at IS NOT NULL AND trashed_at < ? ORDER BY user_id`, formatDBTime(cutoff))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, err
		}
		owners = append(owners, owner)
	}
	return owners, rows.Err()
}

// TrashedContactIDs gives the user's contacts that went to the Trash before
// cutoff, oldest first, up to limit: the ones due to be deleted for good.
// A zero cutoff gives every contact in the Trash.
func (db *DB) TrashedContactIDs(ctx context.Context, userID string, cutoff time.Time, limit int) ([]string, error) {
	query := `SELECT id FROM contact_profiles WHERE user_id = ? AND is_deleted = 1 AND trashed_at IS NOT NULL`
	args := []any{userID}
	if !cutoff.IsZero() {
		query += ` AND trashed_at < ?`
		args = append(args, formatDBTime(cutoff))
	}
	query += ` ORDER BY trashed_at, id LIMIT ?`
	args = append(args, limit)
	rows, err := db.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ContactInTrash reports whether a profile of the user is in the Trash.
func (db *DB) ContactInTrash(ctx context.Context, userID, profileID string) (bool, error) {
	return contactInTrash(ctx, db.Read(), userID, profileID)
}

func contactInTrash(ctx context.Context, query contactProfileQuery, userID, profileID string) (bool, error) {
	if strings.TrimSpace(profileID) == "" {
		return false, nil
	}
	var trashed int
	err := query.QueryRowContext(ctx, `SELECT trashed_at IS NOT NULL FROM contact_profiles WHERE user_id = ? AND id = ?`, userID, profileID).Scan(&trashed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return trashed == 1, err
}
