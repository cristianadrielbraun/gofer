package imap

import (
	"strings"

	"github.com/emersion/go-imap/v2"
)

func detectFolderRole(name string, attrs []imap.MailboxAttr) string {
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

	return detectRoleByName(upper)
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
	case "STARRED", "[GMAIL]/STARRED", "FLAGGED", "IMPORTANT":
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
