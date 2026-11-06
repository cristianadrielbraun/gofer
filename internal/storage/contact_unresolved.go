package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// A contact with Gofer Sync on lives equally in each of its places. When one
// of them deletes it, Gofer doesn't guess whether that was meant for every
// place: the contact goes to be resolved. It stays in the list, marked, it
// isn't in the Trash, it can't be edited, and its sync waits, so nothing
// writes it back to that place or changes it elsewhere. The user deletes it everywhere, or keeps it without
// that place. contact_remote_deletions holds the places that deleted it.

// contactRemoteDeletionsTable is the table of places that deleted a synced
// contact, still to resolve.
const contactRemoteDeletionsTable = "contact_remote_deletions"

// contactUnresolvedSQL matches contacts still to resolve; alias is the
// contact_profiles alias it filters.
func contactUnresolvedSQL(alias string) string {
	return `EXISTS (SELECT 1 FROM contact_remote_deletions unresolved WHERE unresolved.user_id = ` + alias + `.user_id AND unresolved.profile_id = ` + alias + `.id)`
}

// contactOperationNotPausedSQL leaves out queued writes of contacts still
// to resolve.
const contactOperationNotPausedSQL = `NOT EXISTS (SELECT 1 FROM contact_remote_deletions unresolved WHERE unresolved.user_id = contact_sync_operations.user_id AND unresolved.profile_id = contact_sync_operations.contact_id)`

func migrateV138ToV139(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 139 {
		return nil
	}
	log.Printf("storage: schema 139: adding contacts to resolve")
	// Very old databases tested for upgrades have no users table: the
	// table references only the tables there are.
	userRef, profileRef := "", ""
	if ok, err := tableExistsTx(tx, "users"); err != nil {
		return err
	} else if ok {
		userRef = " REFERENCES users(id) ON DELETE CASCADE"
	}
	if ok, err := tableExistsTx(tx, "contact_profiles"); err != nil {
		return err
	} else if ok {
		profileRef = " REFERENCES contact_profiles(id) ON DELETE CASCADE"
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS contact_remote_deletions (
			user_id TEXT NOT NULL` + userRef + `,
			profile_id TEXT NOT NULL` + profileRef + `,
			place TEXT NOT NULL,
			account_id TEXT NOT NULL DEFAULT '',
			detected_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (user_id, profile_id, place)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_remote_deletions_account ON contact_remote_deletions(account_id)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("add contacts to resolve: %w", err)
		}
	}
	if err := markSchemaVersion(tx, 139); err != nil {
		return err
	}
	return tx.Commit()
}

// removedContactCard is a copy an account no longer has.
type removedContactCard struct {
	profile, account, book string
}

// contactsLostCopiesTx settles contacts whose copies an account just
// deleted. One with Gofer Sync on goes to be resolved, with the places that
// deleted it. One with Gofer Sync off goes to the Trash when it was deleted
// in its home; elsewhere it only loses that copy. It gives the contacts that
// went to the Trash and the ones that went to be resolved.
func contactsLostCopiesTx(ctx context.Context, tx *sql.Tx, userID string, cards []removedContactCard) (trashed int, unresolved []string, err error) {
	byProfile := map[string][]removedContactCard{}
	var order []string
	for _, card := range cards {
		if card.profile == "" {
			continue
		}
		if _, ok := byProfile[card.profile]; !ok {
			order = append(order, card.profile)
		}
		byProfile[card.profile] = append(byProfile[card.profile], card)
	}
	for _, id := range order {
		var home string
		var syncEnabled, deleted int
		err := tx.QueryRowContext(ctx, `SELECT home, sync_enabled, is_deleted FROM contact_profiles WHERE user_id = ? AND id = ?`, userID, id).Scan(&home, &syncEnabled, &deleted)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return trashed, unresolved, err
		}
		if deleted == 1 {
			continue
		}
		if syncEnabled == 1 {
			added := false
			for _, card := range byProfile[id] {
				result, err := tx.ExecContext(ctx, `INSERT INTO contact_remote_deletions (user_id, profile_id, place, account_id) VALUES (?, ?, ?, ?)
					ON CONFLICT(user_id, profile_id, place) DO NOTHING`, userID, id, contactCardPlaceOf(card.account, card.book), card.account)
				if err != nil {
					return trashed, unresolved, err
				}
				if n, _ := result.RowsAffected(); n > 0 {
					added = true
				}
			}
			if added {
				unresolved = append(unresolved, id)
			}
			continue
		}
		went, err := trashContactDeletedAtHomeTx(ctx, tx, userID, id, home)
		if err != nil {
			return trashed, unresolved, err
		}
		if went {
			trashed++
		}
	}
	return trashed, unresolved, nil
}

// trashContactDeletedAtHomeTx moves a contact with Gofer Sync off to the
// Trash when its home has no copy of it left. It says whether it went.
func trashContactDeletedAtHomeTx(ctx context.Context, tx *sql.Tx, userID, id, home string) (bool, error) {
	var match, arg string
	if account, ok := strings.CutPrefix(home, "account:"); ok {
		match, arg = `account_id = ?`, account
	} else if book, ok := strings.CutPrefix(home, "book:"); ok {
		match, arg = `address_book_id = ?`, book
	} else {
		return false, nil
	}
	var present int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM contact_cards WHERE user_id = ? AND profile_id = ? AND kind = 'provider' AND is_deleted = 0 AND `+match+`)`, userID, id, arg).Scan(&present); err != nil {
		return false, err
	}
	if present == 1 {
		return false, nil
	}
	return trashContactTx(ctx, tx, userID, id)
}

// contactCardPlaceOf is the place a copy is in: its address book, else its
// account.
func contactCardPlaceOf(account, book string) string {
	if book != "" {
		return "book:" + book
	}
	return "account:" + account
}

// logContactsUnresolved tells the owner's pages about contacts that went to
// be resolved.
func (db *DB) logContactsUnresolved(ctx context.Context, userID string, ids []string) {
	if len(ids) == 0 {
		return
	}
	event := ContactActivityNotification{UserID: userID, EventType: "contacts_unresolved", Message: "Contacts deleted in a place they sync with", Count: len(ids)}
	if len(ids) == 1 {
		event.ContactID = ids[0]
		_ = db.Read().QueryRowContext(ctx, `SELECT COALESCE(NULLIF(display_name, ''), primary_email) FROM contact_profiles WHERE user_id = ? AND id = ?`, userID, ids[0]).Scan(&event.Email)
	}
	_ = db.logContactActivity(ctx, event)
}

// ContactRemoteDeletion is a place that deleted a synced contact.
type ContactRemoteDeletion struct {
	Place, AccountID, DetectedAt string
}

// ContactRemoteDeletions gives the places that deleted the contact, while
// it is still to resolve.
func (db *DB) ContactRemoteDeletions(ctx context.Context, userID, profileID string) ([]ContactRemoteDeletion, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT place, account_id, detected_at FROM contact_remote_deletions WHERE user_id = ? AND profile_id = ? ORDER BY detected_at, place`, userID, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deletions []ContactRemoteDeletion
	for rows.Next() {
		var deletion ContactRemoteDeletion
		if err := rows.Scan(&deletion.Place, &deletion.AccountID, &deletion.DetectedAt); err != nil {
			return nil, err
		}
		deletions = append(deletions, deletion)
	}
	return deletions, rows.Err()
}

// ContactUnresolved says whether the contact waits to be resolved.
func (db *DB) ContactUnresolved(ctx context.Context, userID, profileID string) (bool, error) {
	var unresolved int
	err := db.Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM contact_remote_deletions WHERE user_id = ? AND profile_id = ?)`, userID, profileID).Scan(&unresolved)
	return unresolved == 1, err
}

// CountUnresolvedContacts counts the contacts still to resolve.
func (db *DB) CountUnresolvedContacts(ctx context.Context, userID string) (int, error) {
	var count int
	err := db.Read().QueryRowContext(ctx, `SELECT COUNT(DISTINCT d.profile_id) FROM contact_remote_deletions d
		JOIN contact_profiles p ON p.user_id = d.user_id AND p.id = d.profile_id
		WHERE d.user_id = ? AND p.is_deleted = 0`, userID).Scan(&count)
	return count, err
}

// KeepContactWithoutDeletedPlaces resolves a contact by keeping it without
// the places that deleted it: they leave its Gofer Sync, and its sync goes
// on with the rest. When its home was one of them, it moves to newHome, one
// of its ContactNewHomes choices, or when newHome is empty to the default
// one; a newHome no longer among them gives ErrContactNewHomeGone. newHome
// is ignored when the home stays. It says whether the contact was still to
// resolve.
func (db *DB) KeepContactWithoutDeletedPlaces(ctx context.Context, userID, profileID, newHome string) (bool, error) {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	kept, err := keepContactWithoutDeletedPlacesTx(ctx, tx, userID, profileID, newHome)
	if err != nil || !kept {
		return false, err
	}
	return true, tx.Commit()
}

func keepContactWithoutDeletedPlacesTx(ctx context.Context, tx *sql.Tx, userID, profileID, newHome string) (bool, error) {
	var home string
	if err := tx.QueryRowContext(ctx, `SELECT home FROM contact_profiles WHERE user_id = ? AND id = ? AND is_deleted = 0`, userID, profileID).Scan(&home); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT place FROM contact_remote_deletions WHERE user_id = ? AND profile_id = ?`, userID, profileID)
	if err != nil {
		return false, err
	}
	deleted := map[string]bool{}
	for rows.Next() {
		var place string
		if err := rows.Scan(&place); err != nil {
			rows.Close()
			return false, err
		}
		deleted[place] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if len(deleted) == 0 {
		return false, nil
	}
	for place := range deleted {
		if book, ok := strings.CutPrefix(place, "book:"); ok {
			_, err = tx.ExecContext(ctx, `UPDATE contact_sync_memberships SET enabled = 0, status = 'stopped', last_error = '', updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND profile_id = ? AND address_book_id = ?`, userID, profileID, book)
		} else if account, ok := strings.CutPrefix(place, "account:"); ok {
			_, err = tx.ExecContext(ctx, `UPDATE contact_sync_memberships SET enabled = 0, status = 'stopped', last_error = '', updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND profile_id = ? AND account_id = ? AND address_book_id = ''`, userID, profileID, account)
		}
		if err != nil {
			return false, err
		}
	}
	if deleted[home] {
		choices, next, err := contactNewHomeChoices(ctx, tx, userID, profileID, deleted)
		if err != nil {
			return false, err
		}
		if newHome = strings.TrimSpace(newHome); newHome != "" {
			if !slices.Contains(choices, newHome) {
				return false, ErrContactNewHomeGone
			}
			next = newHome
		}
		if _, err := tx.ExecContext(ctx, `UPDATE contact_profiles SET home = ?, updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND id = ?`, next, userID, profileID); err != nil {
			return false, err
		}
		if next == "local" {
			if _, err := keepContactHomeCardTx(ctx, tx, userID, profileID); err != nil {
				return false, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM contact_remote_deletions WHERE user_id = ? AND profile_id = ?`, userID, profileID); err != nil {
		return false, err
	}
	return true, nil
}

// ErrContactNewHomeGone is a new home chosen for a contact kept without
// its home that is no longer among the places it could live in.
var ErrContactNewHomeGone = errors.New("the chosen main location is no longer available")

type contactPlaceQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// contactNewHomeChoices gives where a contact can live once its home
// deleted it: Gofer Local, and each place with Gofer Sync on that still has
// its copy. The default is Gofer Local when it has a Local copy, else the
// first of those places, else Gofer Local.
func contactNewHomeChoices(ctx context.Context, q contactPlaceQuerier, userID, profileID string, deleted map[string]bool) ([]string, string, error) {
	var local int
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM contact_cards WHERE user_id = ? AND profile_id = ? AND kind = 'local' AND is_deleted = 0)`, userID, profileID).Scan(&local); err != nil {
		return nil, "", err
	}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT c.account_id, c.address_book_id FROM contact_cards c
		JOIN contact_sync_memberships m ON m.user_id = c.user_id AND m.profile_id = c.profile_id AND m.enabled = 1
		  AND m.account_id = c.account_id AND m.address_book_id = c.address_book_id
		WHERE c.user_id = ? AND c.profile_id = ? AND c.kind = 'provider' AND c.is_deleted = 0 AND c.account_id <> ''`, userID, profileID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var places []string
	for rows.Next() {
		var account, book string
		if err := rows.Scan(&account, &book); err != nil {
			return nil, "", err
		}
		if place := contactCardPlaceOf(account, book); !deleted[place] && !slices.Contains(places, place) {
			places = append(places, place)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	sort.Strings(places)
	choices := append([]string{contactHomeLocal}, places...)
	if local == 0 && len(places) > 0 {
		return choices, places[0], nil
	}
	return choices, contactHomeLocal, nil
}

// ContactNewHome is where a contact to resolve can live once it is kept
// without its home, and where it goes when none is chosen.
type ContactNewHome struct {
	Choices []string
	Default string
}

// ContactNewHomes gives, for each listed contact whose home is among the
// places that deleted it, where it can live once kept without them. The
// others keep their home and are left out.
func (db *DB) ContactNewHomes(ctx context.Context, userID string, contacts []models.Contact) (map[string]ContactNewHome, error) {
	homes := make(map[string]ContactNewHome)
	for _, contact := range contacts {
		if !slices.Contains(contact.DeletedFrom, contact.StoredIn) {
			continue
		}
		deleted := make(map[string]bool, len(contact.DeletedFrom))
		for _, place := range contact.DeletedFrom {
			deleted[place] = true
		}
		choices, def, err := contactNewHomeChoices(ctx, db.Read(), userID, contact.ID, deleted)
		if err != nil {
			return nil, err
		}
		homes[contact.ID] = ContactNewHome{Choices: choices, Default: def}
	}
	return homes, nil
}

// ListUnresolvedContacts lists the contacts still to resolve, each with the
// places that deleted it, the first deleted first.
func (db *DB) ListUnresolvedContacts(ctx context.Context, userID string, limit int) ([]models.Contact, error) {
	contacts, err := db.ListContacts(ctx, userID, models.ContactFilters{Unresolved: true, SortBy: "name", SortOrder: "asc"}, limit, 0)
	if err != nil {
		return nil, err
	}
	for i := range contacts {
		deletions, err := db.ContactRemoteDeletions(ctx, userID, contacts[i].ID)
		if err != nil {
			return nil, err
		}
		for _, deletion := range deletions {
			contacts[i].DeletedFrom = append(contacts[i].DeletedFrom, deletion.Place)
		}
	}
	return contacts, nil
}
