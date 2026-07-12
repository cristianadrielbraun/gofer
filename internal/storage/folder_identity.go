package storage

import (
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
