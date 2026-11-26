// Package keywords holds the rules for message keywords, the free-form tags
// that IMAP and JMAP servers store on messages and that Gofer shows as
// labels. Both protocols share one keyword namespace on the server.
package keywords

import (
	"fmt"
	"strings"
)

// Validate checks that keyword can be stored as a label: not a system flag or
// status keyword, and limited to characters every server accepts.
func Validate(keyword string) (string, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return "", fmt.Errorf("label is required")
	}
	if strings.HasPrefix(keyword, "\\") {
		return "", fmt.Errorf("label cannot be a system flag")
	}
	if IsStatus(keyword) {
		return "", fmt.Errorf("label %q is a status keyword", keyword)
	}
	for _, r := range keyword {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' || r == '$' {
			continue
		}
		return "", fmt.Errorf("label %q is not a portable keyword", keyword)
	}
	return keyword, nil
}

// IsStatus reports whether keyword is a system flag or a keyword that marks
// a message's status rather than labelling it. JMAP spells IMAP's \Seen,
// \Flagged, \Draft and \Answered flags as $seen, $flagged, $draft and
// $answered. Registered keywords that servers or clients set to describe a
// message, like $HasAttachment and $HasNoAttachment, which some servers add
// to every new message, count as status too. So do Fastmail's own
// undocumented markers, $IsTrusted and the $X-ME-* keywords. None of them
// show as labels.
func IsStatus(keyword string) bool {
	lower := strings.ToLower(strings.TrimSpace(keyword))
	if IsRFC9979(lower) {
		return true
	}
	switch lower {
	case "\\seen", "\\answered", "\\flagged", "\\deleted", "\\draft", "\\recent",
		"$seen", "$answered", "$flagged", "$draft",
		"$junk", "$notjunk", "$nonjunk", "junk", "notjunk", "nonjunk", "non-junk",
		"$forwarded", "$mdnsent", "$phishing",
		"$submitpending", "$submitted":
		return true
	default:
		return strings.HasPrefix(keyword, "\\") || strings.HasPrefix(lower, "$x-me-")
	}
}

// IsRFC9979 reports whether a keyword is registered by RFC 9979. Keyword
// spellings are matched case-insensitively in IMAP and JMAP.
func IsRFC9979(keyword string) bool {
	switch strings.ToLower(strings.TrimSpace(keyword)) {
	case "$autosent", "$canunsubscribe", "$followed", "$hasattachment",
		"$hasmemo", "$hasnoattachment", "$imported", "$istrusted",
		"$mailflagbit0", "$mailflagbit1", "$mailflagbit2", "$maskedemail",
		"$memo", "$muted", "$new", "$notify", "$unsubscribed":
		return true
	}
	return false
}

// MailboxAttribute identifies a server-supplied RFC 9979 mailbox purpose.
type MailboxAttribute string

const (
	MailboxMemos     MailboxAttribute = "memos"
	MailboxScheduled MailboxAttribute = "scheduled"
	MailboxSnoozed   MailboxAttribute = "snoozed"
)

// RFC9979MailboxAttribute recognizes the registered mailbox attribute or JMAP
// role. RFC 9979 does not imply a leading backslash for these attributes.
func RFC9979MailboxAttribute(value string) MailboxAttribute {
	switch attr := MailboxAttribute(strings.ToLower(value)); attr {
	case MailboxMemos, MailboxScheduled, MailboxSnoozed:
		return attr
	}
	return ""
}
