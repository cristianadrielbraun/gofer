package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// ErrFolderNotManaged means the folder or label isn't one of the owner's,
// or Gofer no longer shows it.
var ErrFolderNotManaged = errors.New("folder can't be changed")

// ManagedFolder is a folder the owner renames or deletes, with what the
// provider knows it by.
type ManagedFolder struct {
	ID               string
	AccountID        string
	ParentID         string
	RemoteID         string
	ProviderRemoteID string
	Name             string
	Role             string
	// Messages counts the messages in the folder and the folders under it.
	Messages int
	// Descendants are the folders under it, deepest first.
	Descendants []ManagedFolder
	// InTrash says the folder is inside the account's Trash, where deleting
	// it deletes it for good.
	InTrash bool
}

// ManagedFolderForUser returns one of the owner's folders with the folders
// under it. A folder Gofer no longer shows gives ErrFolderNotManaged.
func (db *DB) ManagedFolderForUser(ctx context.Context, userID, folderID string) (ManagedFolder, error) {
	var folder ManagedFolder
	var parentID sql.NullString
	err := db.Read().QueryRowContext(ctx, `
		SELECT f.id, f.account_id, f.parent_id, COALESCE(f.remote_id, ''), COALESCE(f.provider_remote_id, ''), f.name, f.role
		FROM folders f
		JOIN accounts a ON a.id = f.account_id
		WHERE f.id = ? AND a.user_id = ?
		  AND COALESCE(f.discovery_state, 'active') = 'active'`,
		strings.TrimSpace(folderID), strings.TrimSpace(userID),
	).Scan(&folder.ID, &folder.AccountID, &parentID, &folder.RemoteID, &folder.ProviderRemoteID, &folder.Name, &folder.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return ManagedFolder{}, ErrFolderNotManaged
	}
	if err != nil {
		return ManagedFolder{}, err
	}
	folder.ParentID = parentID.String
	if err := db.Read().QueryRowContext(ctx, `
		WITH RECURSIVE up(id, parent_id, role) AS (
			SELECT id, parent_id, role FROM folders WHERE id = ?
			UNION ALL
			SELECT f.id, f.parent_id, f.role FROM folders f JOIN up ON f.id = up.parent_id
		)
		SELECT EXISTS(SELECT 1 FROM up WHERE id != ? AND role = 'trash')`, folder.ParentID, folder.ID).Scan(&folder.InTrash); err != nil {
		return ManagedFolder{}, err
	}
	rows, err := db.Read().QueryContext(ctx, `
		WITH RECURSIVE tree(id, depth) AS (
			SELECT id, 0 FROM folders WHERE id = ?
			UNION ALL
			SELECT f.id, tree.depth + 1 FROM folders f JOIN tree ON f.parent_id = tree.id
			WHERE COALESCE(f.discovery_state, 'active') = 'active'
		)
		SELECT f.id, f.account_id, COALESCE(f.parent_id, ''), COALESCE(f.remote_id, ''), COALESCE(f.provider_remote_id, ''), f.name, f.role,
		       (SELECT COUNT(*) FROM message_folder_state s WHERE s.folder_id = f.id AND s.is_deleted = 0)
		FROM tree JOIN folders f ON f.id = tree.id
		ORDER BY tree.depth DESC, f.id`, folder.ID)
	if err != nil {
		return ManagedFolder{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry ManagedFolder
		var messages int
		if err := rows.Scan(&entry.ID, &entry.AccountID, &entry.ParentID, &entry.RemoteID, &entry.ProviderRemoteID, &entry.Name, &entry.Role, &messages); err != nil {
			return ManagedFolder{}, err
		}
		folder.Messages += messages
		if entry.ID != folder.ID {
			folder.Descendants = append(folder.Descendants, entry)
		}
	}
	return folder, rows.Err()
}

// FolderNameTaken says whether another folder under parentID, or at the top
// when it's "", is already called name, ignoring case.
func (db *DB) FolderNameTaken(ctx context.Context, accountID, parentID, name, exceptID string) (bool, error) {
	var taken bool
	err := db.Read().QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM folders
		WHERE account_id = ? AND COALESCE(parent_id, '') = ? AND id != ?
		  AND lower(name) = lower(?) AND COALESCE(discovery_state, 'active') = 'active')`,
		accountID, parentID, exceptID, name).Scan(&taken)
	return taken, err
}

// AddIMAPFolder saves a folder just made on the server, so it shows before
// the next sync, and returns its ID. parentID is "" for a top folder.
func (db *DB) AddIMAPFolder(ctx context.Context, accountID, parentID, mailbox, name string, sortOrder int) (string, error) {
	ids, err := db.IMAPFolderIDs(ctx, accountID)
	if err != nil {
		return "", err
	}
	if id, ok := ids[mailbox]; ok {
		return id, db.restoreFolder(ctx, id)
	}
	taken := make(map[string]bool, len(ids))
	for _, id := range ids {
		taken[id] = true
	}
	id := NewIMAPFolderID(accountID, mailbox, taken)
	if id == "" {
		return "", fmt.Errorf("folder %q has no ID", mailbox)
	}
	return id, db.UpsertFolders(ctx, []UpsertFolderInput{{
		ID: id, AccountID: accountID, ParentID: parentID, RemoteID: mailbox,
		Name: name, Icon: "folder", Role: "custom", Selectable: true, SortOrder: sortOrder,
	}})
}

// AddProviderFolder saves a folder just made on Outlook or JMAP, by the
// provider's id for it, and returns its ID. path is its name after its
// parents', separated by "/".
func (db *DB) AddProviderFolder(ctx context.Context, accountID, providerKind, parentID, providerID, path, name string, sortOrder int) (string, error) {
	id := FolderIDForIdentity(accountID, providerKind, providerID)
	if id == "" {
		return "", fmt.Errorf("folder %q has no ID", name)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{
		ID: id, AccountID: accountID, ParentID: parentID, RemoteID: path, ProviderRemoteID: providerID,
		Name: name, Icon: "folder", Role: "custom", Selectable: true, SortOrder: sortOrder,
	}}); err != nil {
		return "", err
	}
	return id, db.restoreFolder(ctx, id)
}

// restoreFolder shows again a folder made anew where one had been removed.
// Its old sync state went with it.
func (db *DB) restoreFolder(ctx context.Context, id string) error {
	_, err := db.Write().ExecContext(ctx, `UPDATE folders
		SET discovery_state = 'active', missing_since = NULL, selectable = 1, sync_error = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND COALESCE(discovery_state, 'active') != 'active'`, id)
	return err
}

// NextFolderSortOrder returns a sort order placing a new folder after an
// account's others.
func (db *DB) NextFolderSortOrder(ctx context.Context, accountID string) (int, error) {
	var order int
	err := db.Read().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sort_order), 99) + 1 FROM folders WHERE account_id = ?`, accountID).Scan(&order)
	return order, err
}

// RenameFolder records a folder renamed on the server, from oldRemote to
// newRemote: an IMAP mailbox name, or a path for Outlook and JMAP. The
// folders under it, whose remote names start with its own and the
// delimiter, change with it. Every folder keeps its ID.
func (db *DB) RenameFolder(ctx context.Context, accountID, folderID, oldRemote, newRemote, name string, delimiter rune) error {
	return db.moveFolder(ctx, accountID, folderID, nil, oldRemote, newRemote, name, delimiter)
}

// MoveFolder records a folder moved on the server to under parentID, or to
// the top when it's "", as RenameFolder records a rename.
func (db *DB) MoveFolder(ctx context.Context, accountID, folderID, parentID, oldRemote, newRemote, name string, delimiter rune) error {
	return db.moveFolder(ctx, accountID, folderID, &parentID, oldRemote, newRemote, name, delimiter)
}

func (db *DB) moveFolder(ctx context.Context, accountID, folderID string, parentID *string, oldRemote, newRemote, name string, delimiter rune) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE folders SET remote_id = ?, name = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND account_id = ?`, newRemote, name, folderID, accountID); err != nil {
		return fmt.Errorf("rename folder: %w", err)
	}
	if parentID != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE folders SET parent_id = NULLIF(?, '') WHERE id = ? AND account_id = ?`,
			*parentID, folderID, accountID); err != nil {
			return fmt.Errorf("move folder: %w", err)
		}
	}
	if delimiter != 0 {
		oldPrefix := oldRemote + string(delimiter)
		length := utf8.RuneCountInString(oldPrefix)
		if _, err := tx.ExecContext(ctx, `UPDATE folders
			SET remote_id = ? || substr(remote_id, ?), updated_at = CURRENT_TIMESTAMP
			WHERE account_id = ? AND id != ? AND substr(remote_id, 1, ?) = ?`,
			newRemote+string(delimiter), length+1, accountID, folderID, length, oldPrefix); err != nil {
			return fmt.Errorf("rename subfolders: %w", err)
		}
	}
	return tx.Commit()
}

// TrashFolder returns an account's Trash folder, or ErrFolderNotManaged
// when it has none.
func (db *DB) TrashFolder(ctx context.Context, userID, accountID string) (ManagedFolder, error) {
	id, _, err := db.GetFolderIDByRole(ctx, accountID, "trash")
	if err != nil {
		return ManagedFolder{}, err
	}
	if id == "" {
		return ManagedFolder{}, ErrFolderNotManaged
	}
	return db.ManagedFolderForUser(ctx, userID, id)
}

// FoldersInTrash returns the folders directly inside the owner's Trash
// folders: the one named, or every account's for the unified Trash.
func (db *DB) FoldersInTrash(ctx context.Context, userID, trashID string) ([]string, error) {
	query := `SELECT f.id FROM folders f
		JOIN folders t ON t.id = f.parent_id
		JOIN accounts a ON a.id = t.account_id
		WHERE a.user_id = ? AND t.role = 'trash'
		  AND COALESCE(f.discovery_state, 'active') = 'active'
		  AND COALESCE(t.discovery_state, 'active') = 'active'`
	args := []any{strings.TrimSpace(userID)}
	if trashID != "trash" {
		query += ` AND t.id = ?`
		args = append(args, strings.TrimSpace(trashID))
	}
	rows, err := db.Read().QueryContext(ctx, query+` ORDER BY f.id`, args...)
	if err != nil {
		return nil, err
	}
	return scanFolderIDs(rows)
}

// RemoveDeletedFolders drops folders deleted on the server at once, with
// their messages, rather than after the wait a folder that vanished gets.
func (db *DB) RemoveDeletedFolders(ctx context.Context, accountID, providerKind string, folderIDs []string) error {
	if len(folderIDs) == 0 {
		return nil
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	args := append([]any{accountID}, stringsToAny(folderIDs)...)
	rows, err := tx.QueryContext(ctx, `SELECT id FROM folders WHERE account_id = ? AND id IN (`+sqlPlaceholders(len(folderIDs))+`)`, args...)
	if err != nil {
		return err
	}
	owned, err := scanFolderIDs(rows)
	if err != nil {
		return err
	}
	if err := removeFoldersTx(ctx, tx, owned, providerKind, "folder deleted"); err != nil {
		return err
	}
	return tx.Commit()
}

// ProviderLabelForUser returns one of the owner's provider labels, such as
// a Gmail label, by its local ID.
func (db *DB) ProviderLabelForUser(ctx context.Context, userID, labelID string) (models.Label, error) {
	var label models.Label
	err := db.Read().QueryRowContext(ctx, `
		SELECT l.id, l.account_id, l.name, l.provider_id, l.provider_type
		FROM labels l JOIN accounts a ON a.id = l.account_id
		WHERE l.id = ? AND a.user_id = ? AND l.is_system = 0 AND l.provider_id != ''`,
		strings.TrimSpace(labelID), strings.TrimSpace(userID),
	).Scan(&label.ID, &label.AccountID, &label.Name, &label.ProviderID, &label.ProviderType)
	if errors.Is(err, sql.ErrNoRows) {
		return models.Label{}, ErrFolderNotManaged
	}
	return label, err
}

// RenameProviderLabels gives provider labels new names, by provider id.
func (db *DB) RenameProviderLabels(ctx context.Context, accountID, providerType string, names map[string]string) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for providerID, name := range names {
		if _, err := tx.ExecContext(ctx, `UPDATE labels SET name = ?, updated_at = CURRENT_TIMESTAMP
			WHERE account_id = ? AND provider_type = ? AND provider_id = ?`, name, accountID, providerType, providerID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteProviderLabels forgets provider labels deleted on the server, and
// drops queued changes that would put them back.
func (db *DB) DeleteProviderLabels(ctx context.Context, accountID, providerType string, providerIDs []string) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, providerID := range providerIDs {
		var name string
		err := tx.QueryRowContext(ctx, `SELECT name FROM labels WHERE account_id = ? AND provider_type = ? AND provider_id = ?`,
			accountID, providerType, providerID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM label_mutation_queue
			WHERE account_id = ? AND provider_type = ? AND lower(label_name) = lower(?)`, accountID, providerType, name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM labels WHERE account_id = ? AND provider_type = ? AND provider_id = ?`,
			accountID, providerType, providerID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ProviderLabels returns an account's labels from one provider.
func (db *DB) ProviderLabels(ctx context.Context, accountID, providerType string) ([]models.Label, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT id, account_id, name, provider_id, provider_type FROM labels
		WHERE account_id = ? AND provider_type = ? AND provider_id != '' AND is_system = 0
		ORDER BY lower(name)`, accountID, providerType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var labels []models.Label
	for rows.Next() {
		var label models.Label
		if err := rows.Scan(&label.ID, &label.AccountID, &label.Name, &label.ProviderID, &label.ProviderType); err != nil {
			return nil, err
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

// AccountFolderTree returns an account's folders as the sidebar shows them,
// with the folders inside each.
func (db *DB) AccountFolderTree(ctx context.Context, accountID string) ([]models.Folder, error) {
	return db.getFolders(ctx, strings.TrimSpace(accountID))
}
