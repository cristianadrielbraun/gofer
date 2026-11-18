// Package sharedmailbox holds the rules for the mailboxes an IMAP account
// can open besides its own, such as a group's mailbox on Stalwart: how the
// sender identity Gofer makes for one is named, and what the user chose.
package sharedmailbox

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Prefix modes for identity names.
const (
	// PrefixDomain starts the name with the organization the mailbox's
	// domain names: "Example Support" for support@example.com.
	PrefixDomain = "domain"
	// PrefixNone names the identity after the mailbox alone: "Support".
	PrefixNone = "none"
	// PrefixCustom starts the name with text the user chose.
	PrefixCustom = "custom"
	// PrefixServer takes the name the server gives the mailbox, such as a
	// JMAP server's sender name for it, and starts it with the domain's
	// name, as PrefixDomain, when the server gives none.
	PrefixServer = "server"
)

// IdentityName is the name Gofer gives the sender identity of the shared
// mailbox at address: its local part in words, after the prefix that mode
// and custom give. support@example.com is "Example Support" by default.
func IdentityName(address, mode, custom string) string {
	local, domain, ok := strings.Cut(strings.TrimSpace(address), "@")
	if !ok || local == "" {
		// A mailbox named by its owner's user name alone.
		return words(strings.TrimSpace(address))
	}
	name := words(local)
	var prefix string
	switch mode {
	case PrefixNone:
	case PrefixCustom:
		prefix = strings.TrimSpace(custom)
	default:
		prefix = organization(domain)
	}
	if prefix == "" {
		return name
	}
	return prefix + " " + name
}

// organization is the name a domain stands for: its first label, skipping a
// mail host's own label, so example.co.uk and mail.example.com are both
// "Example". A label of three letters or fewer reads as initials: "abc" is
// "ABC".
func organization(domain string) string {
	labels := strings.Split(strings.ToLower(strings.Trim(domain, ".")), ".")
	if len(labels) == 0 || labels[0] == "" {
		return ""
	}
	label := labels[0]
	// A mail host's own label says nothing of the organization.
	if len(labels) > 2 && (label == "mail" || label == "smtp" || label == "imap" || label == "mx") {
		label = labels[1]
	}
	if utf8.RuneCountInString(label) <= 3 && !strings.ContainsAny(label, "-_") {
		return strings.ToUpper(label)
	}
	return words(label)
}

// words turns a local part into words: "sales-team" is "Sales Team" and
// "ai" is "AI".
func words(local string) string {
	fields := strings.FieldsFunc(local, func(r rune) bool { return r == '.' || r == '-' || r == '_' || r == '+' })
	for i, field := range fields {
		if utf8.RuneCountInString(field) <= 2 {
			fields[i] = strings.ToUpper(field)
			continue
		}
		r, size := utf8.DecodeRuneInString(field)
		fields[i] = string(unicode.ToUpper(r)) + strings.ToLower(field[size:])
	}
	if len(fields) == 0 {
		return local
	}
	return strings.Join(fields, " ")
}
