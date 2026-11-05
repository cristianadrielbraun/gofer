package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/google/uuid"
)

// A contact in the Trash is a deleted profile with trashed_at set. Moving it
// there deletes its copies on accounts first; it keeps its home, its Local
// copy and its Gofer Sync places, so a restore writes it back where it
// lived, or to another place that becomes its home. Deleting it for good
// clears trashed_at, leaving the deleted profile as deletion always has.

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
		went, err := trashContactTx(ctx, tx, userID, strings.TrimSpace(id))
		if err != nil {
			return 0, err
		}
		if went {
			trashed++
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

// trashContactTx moves one contact to the Trash and stops the pushes still
// waiting for it. It says whether it went.
func trashContactTx(ctx context.Context, tx *sql.Tx, userID, id string) (bool, error) {
	result, err := tx.ExecContext(ctx, `
		UPDATE contact_profiles SET is_deleted = 1, trashed_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND id = ? AND is_deleted = 0`, userID, id)
	if err != nil {
		return false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE contact_sync_operations SET status = 'done', locked_at = NULL, last_error = '', updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND contact_id = ? AND status IN ('pending', 'running')`, userID, id); err != nil {
		return false, err
	}
	// A contact still to resolve is resolved by going to the Trash.
	_, err = tx.ExecContext(ctx, `DELETE FROM contact_remote_deletions WHERE user_id = ? AND profile_id = ?`, userID, id)
	return err == nil, err
}

// A contact deleted on the account it lives on, with Gofer Sync off, goes
// to the Trash like mail deleted there: its home no longer has it. Restoring
// it writes it back there. One with Gofer Sync on goes to be resolved; see
// contact_unresolved.go.

// RemoveDeletedContactSource forgets a copy deleted on its account. A
// contact that lived there with Gofer Sync off goes to the Trash with it,
// and one with Gofer Sync on goes to be resolved.
func (db *DB) RemoveDeletedContactSource(ctx context.Context, userID, provider, accountID, remoteID string) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT profile_id, account_id, address_book_id FROM contact_cards WHERE user_id = ? AND provider = ? AND account_id = ? AND remote_id = ? AND kind = 'provider' AND is_deleted = 0`, userID, provider, accountID, remoteID)
	if err != nil {
		return err
	}
	var cards []removedContactCard
	for rows.Next() {
		var card removedContactCard
		if err := rows.Scan(&card.profile, &card.account, &card.book); err != nil {
			rows.Close()
			return err
		}
		cards = append(cards, card)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(cards) == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM contact_cards WHERE user_id = ? AND provider = ? AND account_id = ? AND remote_id = ? AND kind = 'provider'`, userID, provider, accountID, remoteID); err != nil {
		return err
	}
	trashed, unresolved, err := contactsLostCopiesTx(ctx, tx, userID, cards)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if trashed > 0 {
		_ = db.LogContactActivity(ctx, userID, "contacts_trashed", "", "Contacts moved to Trash", trashed)
	}
	db.logContactsUnresolved(ctx, userID, unresolved)
	return nil
}

// RestoreContacts brings the user's contacts back from the Trash, as they
// were and still linked to their address books. It gives how many came back.
func (db *DB) RestoreContacts(ctx context.Context, userID string, ids []string) (int, error) {
	return db.restoreContacts(ctx, userID, ids, "", nil)
}

// RestoreContactsTo brings the user's contacts back from the Trash to live
// in place instead of where they lived: Gofer Local, an account or an
// address book. The caller checks the place is one contacts can live in.
func (db *DB) RestoreContactsTo(ctx context.Context, userID string, ids []string, place string) (int, error) {
	return db.restoreContacts(ctx, userID, ids, place, nil)
}

// RestoreUserContacts is RestoreContacts in per-user storage, under the
// owner's guard.
func (db *DB) RestoreUserContacts(ctx context.Context, owner string, ids []string, guard func(*sql.Tx) error) (int, error) {
	return db.RestoreUserContactsTo(ctx, owner, ids, "", guard)
}

// RestoreUserContactsTo is RestoreContactsTo in per-user storage, under the
// owner's guard.
func (db *DB) RestoreUserContactsTo(ctx context.Context, owner string, ids []string, place string, guard func(*sql.Tx) error) (int, error) {
	if guard == nil {
		return 0, ErrContactSyncSuperseded
	}
	return db.restoreContacts(ctx, owner, ids, place, guard)
}

func (db *DB) restoreContacts(ctx context.Context, userID string, ids []string, place string, guard func(*sql.Tx) error) (int, error) {
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
			if place != "" {
				if err := moveRestoredContactHomeTx(ctx, tx, userID, strings.TrimSpace(id), place); err != nil {
					return 0, err
				}
			}
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

// moveRestoredContactHomeTx makes place the home of a contact just back
// from the Trash. Its copies on accounts went when it went to the Trash, so
// the old home keeps nothing: its Local copy goes, or it leaves Gofer Sync.
// A contact with Gofer Sync on syncs with its new home, as a saved one does;
// the caller writes it there.
func moveRestoredContactHomeTx(ctx context.Context, tx *sql.Tx, userID, id, place string) error {
	place = strings.TrimSpace(place)
	if place != contactHomeLocal && !contactStoreTargetValid(place) {
		return fmt.Errorf("invalid restore place %q", place)
	}
	var home string
	var syncEnabled int
	if err := tx.QueryRowContext(ctx, `SELECT home, sync_enabled FROM contact_profiles WHERE user_id = ? AND id = ?`, userID, id).Scan(&home, &syncEnabled); err != nil {
		return err
	}
	if home == place {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE contact_profiles SET home = ?, updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND id = ?`, place, userID, id); err != nil {
		return err
	}
	if err := setContactSyncPlaceTx(ctx, tx, userID, id, home, false); err != nil {
		return err
	}
	if place == contactHomeLocal {
		_, err := keepContactHomeCardTx(ctx, tx, userID, id)
		return err
	}
	if syncEnabled == 1 {
		return setContactSyncPlaceTx(ctx, tx, userID, id, place, true)
	}
	return nil
}

// setContactSyncPlaceTx turns a place on or off among a contact's Gofer
// Sync places; for Gofer Local, it keeps or drops the Local copy.
func setContactSyncPlaceTx(ctx context.Context, tx *sql.Tx, userID, id, place string, on bool) error {
	if place == contactHomeLocal {
		_, err := tx.ExecContext(ctx, `UPDATE contact_cards SET is_deleted = ?, updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND profile_id = ? AND kind = 'local'`, boolInt(!on), userID, id)
		return err
	}
	account, book := "", ""
	if value, ok := strings.CutPrefix(place, "book:"); ok {
		book = value
		if err := tx.QueryRowContext(ctx, `SELECT account_id FROM account_contact_address_books WHERE user_id = ? AND id = ?`, userID, book).Scan(&account); err != nil {
			if errors.Is(err, sql.ErrNoRows) && !on {
				return nil
			}
			return err
		}
	} else if value, ok := strings.CutPrefix(place, "account:"); ok {
		account = value
	} else {
		return nil
	}
	if !on {
		_, err := tx.ExecContext(ctx, `UPDATE contact_sync_memberships SET enabled = 0, status = 'stopped', last_error = '', updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND profile_id = ? AND account_id = ? AND address_book_id = ?`, userID, id, account, book)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO contact_sync_memberships (id, user_id, profile_id, account_id, address_book_id, enabled, status, last_error)
		VALUES (?, ?, ?, ?, ?, 1, 'active', '')
		ON CONFLICT(user_id, profile_id, account_id, address_book_id) DO UPDATE SET enabled = 1, status = 'active', last_error = '', updated_at = CURRENT_TIMESTAMP`,
		uuid.NewString(), userID, id, account, book)
	return err
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
