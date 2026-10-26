package storage

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/cristianadrielbraun/gofer/internal/providers"
)

// ChoosableFolderRoles are the roles people can give an IMAP folder by hand,
// for servers that report none, or the wrong one. The inbox is always INBOX.
var ChoosableFolderRoles = []string{"sent", "drafts", "trash", "junk", "archive"}

// ErrFolderRoleChoice means the roles chosen are not ones that can be chosen,
// name a folder twice, or name the inbox or a folder the account doesn't
// have.
var ErrFolderRoleChoice = errors.New("those folders cannot be given those roles")

// migrateV129ToV130 lets people choose an IMAP account's Sent, Drafts, Trash,
// Spam and Archive folders. Nothing is chosen at first, so every folder keeps
// the role the server reports.
func migrateV129ToV130(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 130 {
		return nil
	}
	exists, err := tableExistsTx(tx, "folders")
	if err != nil {
		return err
	}
	if exists {
		present, err := columnExistsTx(tx, "folders", "role_choice")
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.Exec(`ALTER TABLE folders ADD COLUMN role_choice TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
	}
	if err := markSchemaVersion(tx, 130); err != nil {
		return err
	}
	return tx.Commit()
}

// FolderRoleChoices returns the roles chosen by hand for an account's
// folders, by the folder's remote name.
func (db *DB) FolderRoleChoices(ctx context.Context, accountID string) (map[string]string, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT remote_id, role_choice FROM folders
		WHERE account_id = ? AND role_choice != '' AND COALESCE(remote_id, '') != ''`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	choices := map[string]string{}
	for rows.Next() {
		var remoteID, role string
		if err := rows.Scan(&remoteID, &role); err != nil {
			return nil, err
		}
		choices[remoteID] = role
	}
	return choices, rows.Err()
}

// SetFolderRoleChoices replaces the roles chosen by hand for one of the
// owner's IMAP accounts. choices maps a role from ChoosableFolderRoles to
// the ID of the folder given it; a role left out goes back to the folder the
// server reports. A chosen role moves to its folder at once; the next folder
// sync renames the folders and gives a role left out back to the server's.
func (db *DB) SetFolderRoleChoices(ctx context.Context, userID, accountID string, choices map[string]string) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var provider string
	err = tx.QueryRowContext(ctx, `SELECT provider FROM accounts WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0`, accountID, userID).Scan(&provider)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && provider != providers.ProviderIMAP) {
		return ErrFolderRoleChoice
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE folders SET role_choice = '', updated_at = CURRENT_TIMESTAMP WHERE account_id = ? AND role_choice != ''`, accountID); err != nil {
		return err
	}
	chosen := map[string]bool{}
	for role, folderID := range choices {
		if !slices.Contains(ChoosableFolderRoles, role) || chosen[folderID] {
			return ErrFolderRoleChoice
		}
		chosen[folderID] = true
		result, err := tx.ExecContext(ctx, `
			UPDATE folders SET role_choice = ?, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND account_id = ? AND selectable = 1 AND role != 'inbox' AND COALESCE(remote_id, '') != ''
			  AND COALESCE(discovery_state, 'active') = 'active'`, role, folderID, accountID)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrFolderRoleChoice
		}
		if _, err := tx.ExecContext(ctx, `UPDATE folders SET role = 'custom', updated_at = CURRENT_TIMESTAMP WHERE account_id = ? AND role = ? AND id != ?`, accountID, role, folderID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE folders SET role = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, role, folderID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FolderRoleFolder is a folder of an account as the folder roles dialog
// needs it: its role now, the role chosen for it by hand, and whether it can
// hold mail.
type FolderRoleFolder struct {
	ID         string
	RemoteID   string
	Role       string
	RoleChoice string
	Selectable bool
}

// ListFolderRoleFolders returns an account's listed folders by remote name.
func (db *DB) ListFolderRoleFolders(ctx context.Context, accountID string) ([]FolderRoleFolder, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT id, remote_id, role, role_choice, selectable FROM folders
		WHERE account_id = ? AND COALESCE(remote_id, '') != '' AND COALESCE(discovery_state, 'active') = 'active'
		ORDER BY remote_id COLLATE NOCASE, remote_id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var folders []FolderRoleFolder
	for rows.Next() {
		var f FolderRoleFolder
		if err := rows.Scan(&f.ID, &f.RemoteID, &f.Role, &f.RoleChoice, &f.Selectable); err != nil {
			return nil, err
		}
		folders = append(folders, f)
	}
	return folders, rows.Err()
}

// MissingFolderRoles returns the roles in ChoosableFolderRoles that none of
// an IMAP account's folders has. It returns none for other accounts, and for
// an account whose folders haven't been listed yet, which has no inbox.
func (db *DB) MissingFolderRoles(ctx context.Context, accountID, provider string) ([]string, error) {
	if provider != providers.ProviderIMAP {
		return nil, nil
	}
	rows, err := db.Read().QueryContext(ctx, `
		SELECT DISTINCT role FROM folders
		WHERE account_id = ? AND selectable = 1 AND COALESCE(discovery_state, 'active') = 'active'`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, err
		}
		present[role] = true
	}
	if err := rows.Err(); err != nil || !present["inbox"] {
		return nil, err
	}
	var missing []string
	for _, role := range ChoosableFolderRoles {
		if !present[role] {
			missing = append(missing, role)
		}
	}
	return missing, nil
}
