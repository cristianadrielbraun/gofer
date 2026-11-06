package storage

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// Folders and tags can be put in the order the user wants, in the sidebar or
// in an account's Folders dialog. Servers keep no order of their own that
// Gofer could change, so the order is the user's, like folder colors: each
// folder's place among the folders next to it is kept in app_settings as
// folder_order:<id>, and each tag's among its account's tags as
// tag_order:<id>. The unified folders and tags keep theirs as
// virtual_folder_order:<id> and unified_tag_order:<id>, which reach the
// sidebar with the other unified appearance settings. Folders and tags never
// put in order keep the order they had, after the ones that were: the
// server's or the unified folders' own, by name for tags.

// ErrFolderOrder means the order named something that isn't one of the
// account's folders or tags.
var ErrFolderOrder = errors.New("the folders to put in order aren't all this account's")

// maxOrderedItems bounds one order: no account has this many folders next
// to each other, or tags.
const maxOrderedItems = 5000

// SetFolderOrder puts the folders, all of the account and next to each other
// in its tree, in the order given.
func (db *DB) SetFolderOrder(ctx context.Context, owner, accountID string, folderIDs []string) error {
	return db.setOrder(ctx, owner, accountID, folderIDs, "folder_order:", `SELECT COUNT(*) FROM folders f JOIN accounts a ON a.id = f.account_id
		WHERE f.id = ? AND f.account_id = ? AND a.user_id = ? AND COALESCE(a.is_deleting, 0) = 0`)
}

// SetTagOrder puts the account's tags in the order given.
func (db *DB) SetTagOrder(ctx context.Context, owner, accountID string, tagIDs []string) error {
	return db.setOrder(ctx, owner, accountID, tagIDs, "tag_order:", `SELECT COUNT(*) FROM labels l JOIN accounts a ON a.id = l.account_id
		WHERE l.id = ? AND l.account_id = ? AND a.user_id = ? AND COALESCE(a.is_deleting, 0) = 0`)
}

// SetUnifiedFolderOrder puts the unified folders in the order given.
// Scheduled sits apart from them and keeps its place.
func (db *DB) SetUnifiedFolderOrder(ctx context.Context, owner string, ids []string) error {
	return db.saveOrder(ctx, owner, ids, models.VirtualFolderOrderKey, func(_ *sql.Tx, id string) (bool, error) {
		_, ok := models.VirtualFolderAppearance(id, nil)
		return ok && id != "scheduled", nil
	})
}

// SetUnifiedTagOrder puts the unified tags in the order given.
func (db *DB) SetUnifiedTagOrder(ctx context.Context, owner string, ids []string) error {
	return db.saveOrder(ctx, owner, ids, models.UnifiedTagOrderKey, func(_ *sql.Tx, id string) (bool, error) {
		return models.IsUnifiedTagID(id), nil
	})
}

func (db *DB) setOrder(ctx context.Context, owner, accountID string, ids []string, prefix, ownedSQL string) error {
	return db.saveOrder(ctx, owner, ids, func(id string) string { return prefix + id }, func(tx *sql.Tx, id string) (bool, error) {
		var owned int
		err := tx.QueryRowContext(ctx, ownedSQL, id, accountID, owner).Scan(&owned)
		return owned == 1, err
	})
}

// saveOrder keeps each item's place, once each is known to be one the owner
// can put in order.
func (db *DB) saveOrder(ctx context.Context, owner string, ids []string, key func(string) string, known func(*sql.Tx, string) (bool, error)) error {
	if len(ids) == 0 || len(ids) > maxOrderedItems {
		return ErrFolderOrder
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || seen[id] {
			return ErrFolderOrder
		}
		seen[id] = true
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		ok, err := known(tx, id)
		if err != nil {
			return err
		}
		if !ok {
			return ErrFolderOrder
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings (user_id, key, value, updated_at)
			VALUES (?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			owner, key(id), strconv.Itoa(i)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// validOrderPlace reports whether a stored place is one saveOrder wrote.
func validOrderPlace(value string) bool {
	n, err := strconv.Atoi(value)
	return err == nil && n >= 0 && n < maxOrderedItems
}

// sortByUserOrder puts items with a place first, by it, and leaves the rest
// after them as they came.
func sortByUserOrder[T any](items []T, place func(T) sql.NullInt64) {
	slices.SortStableFunc(items, func(a, b T) int {
		pa, pb := place(a), place(b)
		switch {
		case pa.Valid && pb.Valid:
			return cmp.Compare(pa.Int64, pb.Int64)
		case pa.Valid:
			return -1
		case pb.Valid:
			return 1
		}
		return 0
	})
}
