package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
)

// A contact always lives somewhere: its home, contact_profiles.home, is
// "local" for Gofer Local, or "account:<id>" or "book:<id>" for an account
// or one of its address books. Gofer Sync may copy it to more places; the
// home is where it is stored whether or not Gofer Sync is on. A contact with
// Gofer Local as its home always has a Local card. Observed contacts, which
// Gofer collected from mail, always live in Gofer Local.

const contactHomeLocal = "local"

// contactHomeValid says whether home names a place a contact can live in.
func contactHomeValid(home string) bool {
	return home == contactHomeLocal || contactStoreTargetValid(home)
}

// migrateV136ToV137 gives every contact a home, see repairContactHomesTx.
func migrateV136ToV137(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 137 {
		return nil
	}
	exists, err := tableExistsTx(tx, "contact_profiles")
	if err != nil {
		return err
	}
	if exists {
		started := time.Now()
		log.Printf("storage: schema 137: giving every contact a home")
		present, err := columnExistsTx(tx, "contact_profiles", "home")
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.Exec(`ALTER TABLE contact_profiles ADD COLUMN home TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
		repaired, err := repairContactHomesTx(tx)
		if err != nil {
			return err
		}
		log.Printf("storage: schema 137: %d contact(s) given a home, %d Local copy(ies) restored or added, in %s", repaired.homes, repaired.localCopies, time.Since(started).Round(time.Millisecond))
	}
	if err := markSchemaVersion(tx, 137); err != nil {
		return err
	}
	return tx.Commit()
}

// repairContactHomes gives a home to the contacts that have none. It runs on
// every open, after the migrations: contacts copied in from another storage
// mode arrive without one.
func repairContactHomes(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	exists, err := tableExistsTx(tx, "contact_profiles")
	if err != nil || !exists {
		return err
	}
	present, err := columnExistsTx(tx, "contact_profiles", "home")
	if err != nil || !present {
		return err
	}
	started := time.Now()
	repaired, err := repairContactHomesTx(tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if repaired.homes > 0 || repaired.localCopies > 0 {
		log.Printf("storage: %d contact(s) given a home, %d Local copy(ies) restored or added, in %s", repaired.homes, repaired.localCopies, time.Since(started).Round(time.Millisecond))
	}
	return nil
}

// contactHomeRepair counts what repairContactHomesTx did.
type contactHomeRepair struct {
	homes, localCopies int64
}

// repairContactHomesTx gives each contact without a home the one it has:
// Gofer Local for observed contacts and for any with a Local card; else the
// place of a copy on an account, the one the contact came from first; else,
// for a contact stored nowhere, Gofer Local. Every contact living in Gofer
// Local then gets its Local card back, or a new one.
func repairContactHomesTx(tx *sql.Tx) (contactHomeRepair, error) {
	var repaired contactHomeRepair
	var missing int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM contact_profiles WHERE home = '' OR (home = 'local' AND NOT EXISTS (
		SELECT 1 FROM contact_cards c WHERE c.user_id = contact_profiles.user_id AND c.profile_id = contact_profiles.id AND c.kind = 'local' AND c.is_deleted = 0)))`).Scan(&missing); err != nil {
		return repaired, fmt.Errorf("find contacts without a home: %w", err)
	}
	if missing == 0 {
		return repaired, nil
	}
	statements := []string{
		`UPDATE contact_profiles SET home = 'local'
		 WHERE home = '' AND (origin = 'observed' OR EXISTS (
			SELECT 1 FROM contact_cards c WHERE c.user_id = contact_profiles.user_id AND c.profile_id = contact_profiles.id AND c.kind = 'local' AND c.is_deleted = 0))`,
		`UPDATE contact_profiles SET home = (
			SELECT CASE WHEN c.address_book_id <> '' THEN 'book:' || c.address_book_id ELSE 'account:' || c.account_id END
			FROM contact_cards c
			WHERE c.user_id = contact_profiles.user_id AND c.profile_id = contact_profiles.id AND c.kind = 'provider' AND c.is_deleted = 0 AND c.account_id <> ''
			ORDER BY CASE WHEN contact_profiles.origin = 'synced:' || c.account_id THEN 0 ELSE 1 END, c.id
			LIMIT 1)
		 WHERE home = '' AND EXISTS (
			SELECT 1 FROM contact_cards c WHERE c.user_id = contact_profiles.user_id AND c.profile_id = contact_profiles.id AND c.kind = 'provider' AND c.is_deleted = 0 AND c.account_id <> '')`,
		`UPDATE contact_profiles SET home = 'local' WHERE home = ''`,
		`UPDATE contact_cards SET is_deleted = 0, updated_at = CURRENT_TIMESTAMP
		 WHERE kind = 'local' AND is_deleted = 1 AND EXISTS (
			SELECT 1 FROM contact_profiles p WHERE p.user_id = contact_cards.user_id AND p.id = contact_cards.profile_id AND p.home = 'local')
		 AND NOT EXISTS (
			SELECT 1 FROM contact_cards live WHERE live.user_id = contact_cards.user_id AND live.profile_id = contact_cards.profile_id AND live.kind = 'local' AND live.is_deleted = 0)
		 AND contact_cards.id = (
			SELECT first.id FROM contact_cards first WHERE first.user_id = contact_cards.user_id AND first.profile_id = contact_cards.profile_id AND first.kind = 'local' ORDER BY first.id LIMIT 1)`,
	}
	for i, statement := range statements {
		result, err := tx.Exec(statement)
		if err != nil {
			return repaired, fmt.Errorf("repair contact homes: %w", err)
		}
		changed, _ := result.RowsAffected()
		// The first three give homes; the last brings Local cards back.
		if i < 3 {
			repaired.homes += changed
		} else {
			repaired.localCopies += changed
		}
	}
	rows, err := tx.Query(`SELECT p.user_id, p.id FROM contact_profiles p WHERE p.home = 'local' AND NOT EXISTS (
		SELECT 1 FROM contact_cards c WHERE c.user_id = p.user_id AND c.profile_id = p.id AND c.kind = 'local' AND c.is_deleted = 0)`)
	if err != nil {
		return repaired, fmt.Errorf("find contacts without a Local card: %w", err)
	}
	type profileRef struct{ user, id string }
	var refs []profileRef
	for rows.Next() {
		var ref profileRef
		if err := rows.Scan(&ref.user, &ref.id); err != nil {
			rows.Close()
			return repaired, err
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return repaired, err
	}
	rows.Close()
	for _, ref := range refs {
		if _, err := tx.Exec(`INSERT INTO contact_cards (id, user_id, profile_id, kind) VALUES (?, ?, ?, 'local')`, uuid.NewString(), ref.user, ref.id); err != nil {
			return repaired, fmt.Errorf("add a Local card: %w", err)
		}
		repaired.localCopies++
	}
	return repaired, nil
}

// contactHomeForSave is the home a saved contact gets: the one it has, for
// an existing contact; else the requested one, Gofer Local when it names no
// valid place, and always Gofer Local for an observed contact.
func contactHomeForSave(requested, existing, origin string) string {
	if origin == "observed" {
		return contactHomeLocal
	}
	if existing = strings.TrimSpace(existing); existing != "" {
		return existing
	}
	if requested = strings.TrimSpace(requested); contactHomeValid(requested) {
		return requested
	}
	return contactHomeLocal
}

// keepContactHomeCardTx keeps the copy a contact's home stands for: a
// contact living in Gofer Local has its Local card, whatever a save asked.
// It gives the contact's home.
func keepContactHomeCardTx(ctx context.Context, tx *sql.Tx, userID, profileID string) (string, error) {
	var home string
	if err := tx.QueryRowContext(ctx, `SELECT home FROM contact_profiles WHERE user_id = ? AND id = ?`, userID, profileID).Scan(&home); err != nil {
		return "", err
	}
	if home != contactHomeLocal {
		return home, nil
	}
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM contact_cards WHERE user_id = ? AND profile_id = ? AND kind = 'local' AND is_deleted = 0)`, userID, profileID).Scan(&live); err != nil {
		return "", err
	}
	if live == 1 {
		return home, nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE contact_cards SET is_deleted = 0, updated_at = CURRENT_TIMESTAMP
		WHERE id = (SELECT id FROM contact_cards WHERE user_id = ? AND profile_id = ? AND kind = 'local' ORDER BY id LIMIT 1)`, userID, profileID)
	if err != nil {
		return "", err
	}
	if restored, err := result.RowsAffected(); err != nil {
		return "", err
	} else if restored == 1 {
		return home, nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO contact_cards (id, user_id, profile_id, kind) VALUES (?, ?, ?, 'local')`, uuid.NewString(), userID, profileID)
	return home, err
}

// contactHomeForAccountTx narrows an account home to the address book of a
// copy that arrives in it: the contact lives in that book.
func contactHomeForAccountTx(ctx context.Context, tx *sql.Tx, userID, profileID, accountID, bookID string) error {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(bookID) == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE contact_profiles SET home = 'book:' || ? WHERE user_id = ? AND id = ? AND home = 'account:' || ?`, bookID, userID, profileID, accountID)
	return err
}

// TakeOverContact makes to the contact from was, before from is deleted in
// a move: to keeps from's origin and creation date, and the mail Gofer saw
// for from now counts for to, so deleting from never stops Gofer collecting
// that address again.
func (db *DB) TakeOverContact(ctx context.Context, owner, from, to string) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE contact_profiles SET
		origin = (SELECT origin FROM contact_profiles WHERE user_id = ? AND id = ?),
		created_at = (SELECT created_at FROM contact_profiles WHERE user_id = ? AND id = ?),
		updated_at = CURRENT_TIMESTAMP
		WHERE user_id = ? AND id = ? AND EXISTS (SELECT 1 FROM contact_profiles WHERE user_id = ? AND id = ?)`,
		owner, from, owner, from, owner, to, owner, from)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `UPDATE contact_observations SET profile_id = ?, updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND profile_id = ?`, to, owner, from); err != nil {
		return err
	}
	return tx.Commit()
}
