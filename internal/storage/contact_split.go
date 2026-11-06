package storage

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
)

// From schema 138 a contact is one copy in one home. The same person in two
// places is two contacts, unless Gofer Sync links them into one: several
// contacts may share an email address, and Gofer no longer merges them by
// it. Mail activity belongs to the address, so every contact with it shows
// the same activity.

// migrateV137ToV138 lets contacts share an address, and splits each contact
// held in several places without Gofer Sync into one contact per place.
func migrateV137ToV138(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 138 {
		return nil
	}
	exists, err := tableExistsTx(tx, "contact_identities")
	if err != nil {
		return err
	}
	started := time.Now()
	if exists {
		log.Printf("storage: schema 138: letting contacts share an email address")
		// Very old databases tested for upgrades have no users table: the
		// rebuilt table references only the tables there are.
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
			`CREATE TABLE contact_identities_v138 (
				user_id TEXT NOT NULL` + userRef + `,
				profile_id TEXT NOT NULL` + profileRef + `,
				kind TEXT NOT NULL,
				normalized_value TEXT NOT NULL,
				confidence REAL NOT NULL DEFAULT 1.0,
				created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (user_id, kind, normalized_value, profile_id)
			)`,
			`INSERT INTO contact_identities_v138 (user_id, profile_id, kind, normalized_value, confidence, created_at, updated_at)
			 SELECT user_id, profile_id, kind, normalized_value, confidence, created_at, updated_at FROM contact_identities`,
			`DROP TABLE contact_identities`,
			`ALTER TABLE contact_identities_v138 RENAME TO contact_identities`,
			`CREATE INDEX IF NOT EXISTS idx_contact_identities_profile ON contact_identities(user_id, profile_id)`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				return fmt.Errorf("let contacts share an address: %w", err)
			}
		}
	}
	profiles, err := tableExistsTx(tx, "contact_profiles")
	if err != nil {
		return err
	}
	if profiles {
		split, made, err := splitMultiPlaceContactsTx(tx)
		if err != nil {
			return err
		}
		log.Printf("storage: schema 138: %d contact(s) held in several places split into %d more, in %s", split, made, time.Since(started).Round(time.Millisecond))
	}
	if err := markSchemaVersion(tx, 138); err != nil {
		return err
	}
	return tx.Commit()
}

// contactCardPlace is the place a card is in: "local", "book:<id>" or
// "account:<id>".
const contactCardPlaceSQL = `CASE WHEN c.kind = 'local' THEN 'local' WHEN c.address_book_id <> '' THEN 'book:' || c.address_book_id ELSE 'account:' || c.account_id END`

// splitMultiPlaceContactsTx keeps each contact without Gofer Sync in its
// home, and makes every other place it has a copy in a contact of its own:
// the copy's card, and for a copy on an account the details that account
// gave, move to it. A Local copy split off a contact living on an account
// takes a copy of the contact's details, as its own manual ones.
// It gives how many contacts it split, and how many contacts that made.
func splitMultiPlaceContactsTx(tx *sql.Tx) (int, int, error) {
	rows, err := tx.Query(`SELECT p.user_id, p.id, p.home FROM contact_profiles p
		WHERE p.is_deleted = 0 AND p.sync_enabled = 0 AND p.home <> '' AND (
			SELECT COUNT(DISTINCT ` + contactCardPlaceSQL + `) FROM contact_cards c
			WHERE c.user_id = p.user_id AND c.profile_id = p.id AND c.is_deleted = 0) > 1`)
	if err != nil {
		return 0, 0, fmt.Errorf("find contacts in several places: %w", err)
	}
	type split struct{ user, id, home string }
	var splits []split
	for rows.Next() {
		var s split
		if err := rows.Scan(&s.user, &s.id, &s.home); err != nil {
			rows.Close()
			return 0, 0, err
		}
		splits = append(splits, s)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, err
	}
	rows.Close()
	made := 0
	for _, s := range splits {
		n, err := splitContactTx(tx, s.user, s.id, s.home)
		if err != nil {
			return 0, 0, fmt.Errorf("split contact %s: %w", s.id, err)
		}
		made += n
	}
	return len(splits), made, nil
}

func splitContactTx(tx *sql.Tx, user, id, home string) (int, error) {
	rows, err := tx.Query(`SELECT DISTINCT `+contactCardPlaceSQL+`, c.account_id FROM contact_cards c
		WHERE c.user_id = ? AND c.profile_id = ? AND c.is_deleted = 0 ORDER BY 1`, user, id)
	if err != nil {
		return 0, err
	}
	type place struct{ name, account string }
	var places []place
	for rows.Next() {
		var p place
		if err := rows.Scan(&p.name, &p.account); err != nil {
			rows.Close()
			return 0, err
		}
		places = append(places, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	homeAccount := ""
	for _, p := range places {
		if p.name == home {
			homeAccount = p.account
		}
	}
	movedSources := map[string]bool{}
	made := 0
	for _, p := range places {
		if p.name == home {
			continue
		}
		next := uuid.NewString()
		origin := "manual"
		if p.name != "local" && p.account != "" {
			origin = "synced:" + p.account
		}
		if _, err := tx.Exec(`INSERT INTO contact_profiles (id, user_id, display_name, sort_name, primary_email, avatar_url, notes, origin, sync_enabled, is_deleted, created_at, updated_at, home)
			SELECT ?, user_id, display_name, sort_name, primary_email, avatar_url, notes, ?, 0, 0, created_at, CURRENT_TIMESTAMP, ? FROM contact_profiles WHERE user_id = ? AND id = ?`,
			next, origin, p.name, user, id); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE contact_cards SET profile_id = ?, updated_at = CURRENT_TIMESTAMP
			WHERE user_id = ? AND profile_id = ? AND is_deleted = 0 AND (`+strings.ReplaceAll(contactCardPlaceSQL, "c.", "")+`) = ?`, next, user, id, p.name); err != nil {
			return 0, err
		}
		source := "synced:" + p.account
		if p.name == "local" {
			// A Local copy keeps the contact's details as its own.
			if _, err := tx.Exec(`INSERT INTO contact_fields (id, user_id, profile_id, card_id, kind, label, value, normalized_value, is_primary, ordinal, source, confidence)
				SELECT lower(hex(randomblob(16))), user_id, ?, NULL, kind, label, value, normalized_value, is_primary, ordinal, 'manual', confidence
				FROM contact_fields WHERE user_id = ? AND profile_id = ? AND source IN ('manual', 'observed', 'canonical', ?)`,
				next, user, id, "synced:"+homeAccount); err != nil {
				return 0, err
			}
		} else {
			if _, err := tx.Exec(`INSERT INTO contact_fields (id, user_id, profile_id, card_id, kind, label, value, normalized_value, is_primary, ordinal, source, confidence)
				SELECT lower(hex(randomblob(16))), user_id, ?, card_id, kind, label, value, normalized_value, is_primary, ordinal, source, confidence
				FROM contact_fields WHERE user_id = ? AND profile_id = ? AND source = ?`, next, user, id, source); err != nil {
				return 0, err
			}
			if p.account != homeAccount {
				movedSources[source] = true
			}
		}
		if err := rebuildContactIdentitiesTx(tx, user, next); err != nil {
			return 0, err
		}
		made++
	}
	for source := range movedSources {
		if _, err := tx.Exec(`DELETE FROM contact_fields WHERE user_id = ? AND profile_id = ? AND source = ?`, user, id, source); err != nil {
			return 0, err
		}
	}
	return made, rebuildContactIdentitiesTx(tx, user, id)
}

// rebuildContactIdentitiesTx makes a contact's identities its email
// addresses and phone numbers again.
func rebuildContactIdentitiesTx(tx *sql.Tx, user, id string) error {
	if _, err := tx.Exec(`DELETE FROM contact_identities WHERE user_id = ? AND profile_id = ?`, user, id); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT OR IGNORE INTO contact_identities (user_id, profile_id, kind, normalized_value, confidence)
		SELECT user_id, profile_id, kind, normalized_value, MAX(confidence) FROM contact_fields
		WHERE user_id = ? AND profile_id = ? AND kind IN ('email', 'phone') AND normalized_value <> ''
		GROUP BY kind, normalized_value`, user, id)
	return err
}
