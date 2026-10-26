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
// name a folder twice, or name a folder the account doesn't have.
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
// server reports. The folders' roles change at the next folder sync.
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
			WHERE id = ? AND account_id = ? AND selectable = 1 AND COALESCE(remote_id, '') != ''
			  AND COALESCE(discovery_state, 'active') = 'active'`, role, folderID, accountID)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrFolderRoleChoice
		}
	}
	return tx.Commit()
}
