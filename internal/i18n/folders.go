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
