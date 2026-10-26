package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// FolderIDForIdentity returns the stable local ID for a real provider folder.
// The provider identity must be the exact mailbox/label/folder identity, not a
// display name or a sanitized path. Empty identities are invalid because they
// cannot be reconciled safely after a restart.
func FolderIDForIdentity(accountID, providerKind, providerIdentity string) string {
	accountID = strings.TrimSpace(accountID)
	providerKind = strings.TrimSpace(strings.ToLower(providerKind))
	if accountID == "" || providerKind == "" || providerIdentity == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(providerKind + "\x00" + providerIdentity))
	return fmt.Sprintf("%s_f_%s", accountID, hex.EncodeToString(digest[:16]))
}

// NewIMAPFolderID returns the ID for an IMAP folder first seen under name.
// A folder renamed in Gofer keeps its ID, so the ID its new name would give
// may already belong to it; taken holds the IDs in use, and a name whose ID
// is taken gets the next free one.
func NewIMAPFolderID(accountID, name string, taken map[string]bool) string {
	id := FolderIDForIdentity(accountID, "imap", name)
	for n := 1; id != "" && taken[id]; n++ {
		id = FolderIDForIdentity(accountID, "imap", fmt.Sprintf("%s\x00%d", name, n))
	}
	return id
}

// IMAPFolderIDs returns the IDs of an IMAP account's folders by mailbox
// name, so a folder renamed in Gofer keeps its ID.
func (db *DB) IMAPFolderIDs(ctx context.Context, accountID string) (map[string]string, error) {
	rows, err := db.Read().QueryContext(ctx, `
		SELECT remote_id, id FROM folders
		WHERE account_id = ? AND COALESCE(provider_remote_id, '') = '' AND COALESCE(remote_id, '') != ''`,
		strings.TrimSpace(accountID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[string]string{}
	for rows.Next() {
		var remoteID, id string
		if err := rows.Scan(&remoteID, &id); err != nil {
			return nil, err
		}
		ids[remoteID] = id
	}
	return ids, rows.Err()
}
