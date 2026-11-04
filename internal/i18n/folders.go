package i18n

import (
	"context"
	"strings"
)

// Sync names special folders in English ("Inbox", "Sent", …) by their role.
// These are the roles and the names it gives them.
var folderRoleNames = map[string]string{
	"inbox": "Inbox", "sent": "Sent", "drafts": "Drafts", "trash": "Trash",
	"junk": "Spam", "spam": "Spam", "archive": "Archive", "starred": "Starred",
	"scheduled": "Scheduled",
}

// FolderName shows a folder's name in the context's language. A special
// folder that still has the English name sync gave it is translated; any
// other name, such as one the user or the server chose, is kept as it is.
func FolderName(ctx context.Context, role, name string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	if english, ok := folderRoleNames[role]; ok && name == english {
		if role == "junk" {
			role = "spam"
		}
		return T(ctx, "folder."+role)
	}
	return name
}

// LocalFoldersName is the English name Gofer gives each user's Local folders
// account, the one with no mail server behind it.
const LocalFoldersName = "Local folders"

// AccountName shows an account's name in the context's language. Local
// folders still called by the name Gofer gave it is translated; every other
// name is kept as it is.
func AccountName(ctx context.Context, provider, name string) string {
	if provider == "local" && name == LocalFoldersName {
		return T(ctx, "account.local_folders")
	}
	return name
}
