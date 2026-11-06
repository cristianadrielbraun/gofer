package storage

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// ContactBookUsage is how an address book Gofer stops syncing is used: the
// contacts whose main location it is, those of them with no other place to
// live, and the contacts that only keep a copy there.
type ContactBookUsage struct {
	Living     int `json:"living"`
	OnlyThere  int `json:"only_there"`
	CopiesOnly int `json:"copies_only"`
}

// ContactBookRelease says what happens to the contacts living in an address
// book Gofer stops syncing: they stay, in Gofer Local, or Gofer forgets the
// ones that live nowhere else (the provider keeps them all).
type ContactBookRelease string

const (
	ContactBookKeepInLocal ContactBookRelease = "keep"
	ContactBookRemove      ContactBookRelease = "remove"
)

// ContactBookMover is a contact whose main location is an address book Gofer
// stops syncing and that Gofer Sync also keeps in other locations: those
// locations, where it can move to (them, and Gofer Local, first), and where
// Gofer would put it.
type ContactBookMover struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Copies  []string `json:"copies"`
	Places  []string `json:"places"`
	Default string   `json:"default"`
}

// ContactAddressBookMovers lists the contacts that live in an address book
// and in other places too, by name, for the user to choose where each goes
// before Gofer stops syncing the book.
func (db *DB) ContactAddressBookMovers(ctx context.Context, userID, bookID string) ([]ContactBookMover, error) {
	place := "book:" + bookID
	rows, err := db.Read().QueryContext(ctx, `SELECT id, display_name FROM contact_profiles WHERE user_id = ? AND is_deleted = 0 AND home = ? ORDER BY display_name COLLATE NOCASE, id`, userID, place)
	if err != nil {
		return nil, err
	}
	type living struct{ id, name string }
	var all []living
	for rows.Next() {
		var l living
		if err := rows.Scan(&l.id, &l.name); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	movers := []ContactBookMover{}
	for _, l := range all {
		elsewhere, err := contactLivesElsewhere(ctx, db.Read(), userID, l.id, place)
		if err != nil {
			return nil, err
		}
		if !elsewhere {
			continue
		}
		choices, next, err := contactNewHomeChoices(ctx, db.Read(), userID, l.id, map[string]bool{place: true})
		if err != nil {
			return nil, err
		}
		var local int
		if err := db.Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM contact_cards WHERE user_id = ? AND profile_id = ? AND kind = 'local' AND is_deleted = 0)`, userID, l.id).Scan(&local); err != nil {
			return nil, err
		}
		copies := []string{}
		for _, choice := range choices {
			if choice != contactHomeLocal || local == 1 {
				copies = append(copies, choice)
			}
		}
		movers = append(movers, ContactBookMover{ID: l.id, Name: l.name, Copies: copies, Places: choices, Default: next})
	}
	return movers, nil
}

// ContactAddressBookUsage counts how the contacts use an address book.
func (db *DB) ContactAddressBookUsage(ctx context.Context, userID, bookID string) (ContactBookUsage, error) {
	var usage ContactBookUsage
	ids, err := contactBookProfiles(ctx, db.Read(), userID, bookID)
	if err != nil {
		return usage, err
	}
	place := "book:" + bookID
	for _, id := range ids {
		var home string
		if err := db.Read().QueryRowContext(ctx, `SELECT home FROM contact_profiles WHERE user_id = ? AND id = ?`, userID, id).Scan(&home); err != nil {
			return usage, err
		}
		if home != place {
			usage.CopiesOnly++
			continue
		}
		usage.Living++
		if elsewhere, err := contactLivesElsewhere(ctx, db.Read(), userID, id, place); err != nil {
			return usage, err
		} else if !elsewhere {
			usage.OnlyThere++
		}
	}
	return usage, nil
}

// ReleaseContactAddressBook lets go of an address book Gofer no longer syncs.
// Its copies of contacts are dropped from Gofer, never deleted on the
// provider, and nothing syncs to it anymore; the contacts' other places keep
// syncing. Copied, every contact that lived there now lives in Gofer Local.
// Removed, the ones that lived nowhere else leave Gofer, and the others move
// to the place homes gives them, one of their places, or Gofer Local when it
// gives none that still is. It answers how many contacts moved and how many
// Gofer removed.
func (db *DB) ReleaseContactAddressBook(ctx context.Context, userID, bookID string, release ContactBookRelease, homes map[string]string) (moved, removed int, err error) {
	bookID = strings.TrimSpace(bookID)
	if bookID == "" {
		return 0, 0, nil
	}
	if release != ContactBookKeepInLocal && release != ContactBookRemove {
		return 0, 0, fmt.Errorf("unknown address book release %q", release)
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	ids, err := contactBookProfiles(ctx, tx, userID, bookID)
	if err != nil {
		return 0, 0, err
	}
	place := "book:" + bookID
	for _, id := range ids {
		var home string
		if err := tx.QueryRowContext(ctx, `SELECT home FROM contact_profiles WHERE user_id = ? AND id = ?`, userID, id).Scan(&home); err != nil {
			return 0, 0, err
		}
		if home == place {
			choices, _, err := contactNewHomeChoices(ctx, tx, userID, id, map[string]bool{place: true})
			if err != nil {
				return 0, 0, err
			}
			elsewhere, err := contactLivesElsewhere(ctx, tx, userID, id, place)
			if err != nil {
				return 0, 0, err
			}
			next := contactHomeLocal
			if chosen := strings.TrimSpace(homes[id]); release == ContactBookRemove && slices.Contains(choices, chosen) {
				next = chosen
			}
			if !elsewhere && release == ContactBookRemove {
				if _, err := tx.ExecContext(ctx, `UPDATE contact_profiles SET is_deleted = 1, trashed_at = NULL, updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND id = ?`, userID, id); err != nil {
					return 0, 0, err
				}
				removed++
			} else {
				if _, err := tx.ExecContext(ctx, `UPDATE contact_profiles SET home = ?, updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND id = ?`, next, userID, id); err != nil {
					return 0, 0, err
				}
				if next == contactHomeLocal {
					if _, err := keepContactHomeCardTx(ctx, tx, userID, id); err != nil {
						return 0, 0, err
					}
				}
				moved++
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM contact_remote_deletions WHERE user_id = ? AND profile_id = ? AND place = ?`, userID, id, place); err != nil {
			return 0, 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE contact_cards SET is_deleted = 1, updated_at = CURRENT_TIMESTAMP WHERE user_id = ? AND address_book_id = ? AND kind = 'provider'`, userID, bookID); err != nil {
		return 0, 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM contact_sync_memberships WHERE user_id = ? AND address_book_id = ?`, userID, bookID); err != nil {
		return 0, 0, err
	}
	return moved, removed, tx.Commit()
}

// contactBookProfiles lists the contacts that live in an address book, keep
// a copy there, or sync to it.
func contactBookProfiles(ctx context.Context, q contactPlaceQuerier, userID, bookID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id FROM contact_profiles WHERE user_id = ?1 AND is_deleted = 0 AND home = 'book:' || ?2
		UNION
		SELECT c.profile_id FROM contact_cards c JOIN contact_profiles p ON p.user_id = c.user_id AND p.id = c.profile_id AND p.is_deleted = 0
		WHERE c.user_id = ?1 AND c.address_book_id = ?2 AND c.kind = 'provider' AND c.is_deleted = 0
		UNION
		SELECT m.profile_id FROM contact_sync_memberships m JOIN contact_profiles p ON p.user_id = m.user_id AND p.id = m.profile_id AND p.is_deleted = 0
		WHERE m.user_id = ?1 AND m.address_book_id = ?2`, userID, bookID)
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

// contactLivesElsewhere says whether a contact has a place other than the
// one given: a Gofer Local copy, or a synced copy in another place.
func contactLivesElsewhere(ctx context.Context, q contactPlaceQuerier, userID, profileID, place string) (bool, error) {
	choices, _, err := contactNewHomeChoices(ctx, q, userID, profileID, map[string]bool{place: true})
	if err != nil {
		return false, err
	}
	if len(choices) > 1 {
		return true, nil
	}
	var local int
	err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM contact_cards WHERE user_id = ? AND profile_id = ? AND kind = 'local' AND is_deleted = 0)`, userID, profileID).Scan(&local)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return local == 1, err
}
