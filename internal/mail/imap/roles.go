package imap

import (
	"strings"

	"github.com/emersion/go-imap/v2"
)

// detectFolderRole names a folder's purpose from its special-use attributes,
// or else from its name. Some servers keep every folder under a personal
// namespace prefix such as "INBOX.", so the name is matched without it.
func detectFolderRole(name string, delim rune, attrs []imap.MailboxAttr, namespaces []string) string {
	for _, attr := range attrs {
		switch attr {
		case imap.MailboxAttrSent:
			return "sent"
		case imap.MailboxAttrDrafts:
			return "drafts"
		case imap.MailboxAttrTrash:
			return "trash"
		case imap.MailboxAttrJunk:
			return "junk"
		case imap.MailboxAttrArchive:
			return "archive"
		case imap.MailboxAttrFlagged:
			return "starred"
		}
	}

	upper := strings.ToUpper(name)
	if upper == "INBOX" {
		return "inbox"
	}

	role := detectRoleByName(strings.ToUpper(trimNamespacePrefix(name, delim, namespaces)))
	if role == "inbox" {
		// Only INBOX itself is the inbox; "INBOX.Inbox" is a folder in it.
		return "custom"
	}
	return role
}

// trimNamespacePrefix removes the personal namespace prefix from a folder
// name. A nil list means the server could not report its namespaces; then an
// "INBOX" prefix is assumed, the usual one on servers that have a prefix.
func trimNamespacePrefix(name string, delim rune, namespaces []string) string {
	if namespaces == nil {
		if delim == 0 {
			return name
		}
		namespaces = []string{"INBOX" + string(delim)}
	}
	for _, prefix := range namespaces {
		if prefix == "" || len(name) <= len(prefix) {
			continue
		}
		// INBOX is case-insensitive, and so is the prefix built on it.
		if strings.EqualFold(name[:len(prefix)], prefix) {
			return name[len(prefix):]
		}
	}
	return name
}

func detectRoleByName(upper string) string {
	switch upper {
	case "INBOX":
		return "inbox"
	case "SENT", "SENT MESSAGES", "SENT ITEMS", "[GMAIL]/SENT MAIL":
		return "sent"
	case "DRAFTS", "[GMAIL]/DRAFTS":
		return "drafts"
	case "TRASH", "DELETED MESSAGES", "[GMAIL]/TRASH":
		return "trash"
	case "SPAM", "JUNK", "[GMAIL]/SPAM", "JUNK EMAIL", "JUNK E-MAIL":
		return "junk"
	case "ARCHIVE", "[GMAIL]/ALL MAIL":
		return "archive"
	case "STARRED", "[GMAIL]/STARRED", "FLAGGED":
		return "starred"
	default:
		return "custom"
	}
}

func RoleIcon(role string) string {
	switch role {
	case "inbox":
		return "inbox"
	case "sent":
		return "send"
	case "drafts":
		return "file"
	case "trash":
		return "trash"
	case "junk":
		return "alert-circle"
	case "archive":
		return "archive"
	case "starred":
		return "star"
	default:
		return "folder"
	}
}
