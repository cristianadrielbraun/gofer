package imap

import (
	"context"
	"fmt"
	"net/mail"
	"strings"

	"github.com/emersion/go-imap/v2"
)

// Shared roles. A folder inside another person's or a group's mailbox keeps
// the role it has there in SharedRole, and Role "custom", so that only the
// account's own folders ever stand for its Inbox, Sent, Drafts or Trash.
const (
	// SharedRoleNamespace is the folder that holds the shared mailboxes,
	// the prefix of a shared namespace.
	SharedRoleNamespace = "namespace"
	// SharedRoleRoot is a shared mailbox's own top folder, named after its
	// owner.
	SharedRoleRoot = "root"
)

// namespaceSet is what the server reported in NAMESPACE (RFC 2342).
type namespaceSet struct {
	// Personal is nil when the server could not report its namespaces.
	Personal []string
	// Shared holds the "other users" and "shared" namespaces.
	Shared []sharedNamespace
	// Known says the server answered NAMESPACE.
	Known bool
}

// sharedNamespace is a namespace other people's mailboxes are in.
type sharedNamespace struct {
	// Prefix is the namespace's prefix without its trailing delimiter.
	Prefix string
	// OtherUsers is set for the "other users" namespace, where the level
	// below the prefix is a mailbox owner's name (RFC 2342 section 5); in a
	// "shared" namespace it is only taken for a mailbox when it is an
	// address, since ordinary public folders live there too.
	OtherUsers bool
}

func namespaceSetFromData(data *imap.NamespaceData) namespaceSet {
	set := namespaceSet{Personal: make([]string, 0, len(data.Personal)), Known: true}
	for _, ns := range data.Personal {
		set.Personal = append(set.Personal, ns.Prefix)
	}
	add := func(group []imap.NamespaceDescriptor, otherUsers bool) {
		for _, ns := range group {
			prefix := ns.Prefix
			if ns.Delim != 0 {
				prefix = strings.TrimSuffix(prefix, string(ns.Delim))
			}
			if prefix != "" {
				set.Shared = append(set.Shared, sharedNamespace{Prefix: prefix, OtherUsers: otherUsers})
			}
		}
	}
	add(data.Other, true)
	add(data.Shared, false)
	return set
}

// sharedPlacement says where a folder sits in a shared namespace.
type sharedPlacement struct {
	// Mailbox names the shared mailbox: its address in lower case, or, in
	// the "other users" namespace of a server that names people without a
	// domain, its owner's name. "" for the namespace folder itself.
	Mailbox string
	// Relative is the folder's name inside the shared mailbox, "" for the
	// mailbox's own top folder.
	Relative string
}

// sharedPlacementOf finds the shared mailbox a folder belongs to. A folder is
// in one when its name starts with a shared namespace prefix the server
// reported and has a level below it naming the owner: the server's own
// namespaces decide, not the look of the name, so a personal folder named
// after an address is never taken for a shared mailbox.
func sharedPlacementOf(name string, delim rune, namespaces []sharedNamespace) (sharedPlacement, bool) {
	if delim == 0 || name == "" {
		return sharedPlacement{}, false
	}
	sep := string(delim)
	for _, ns := range namespaces {
		prefix := ns.Prefix
		if prefix == "" {
			continue
		}
		if strings.EqualFold(name, prefix) {
			return sharedPlacement{}, true
		}
		if len(name) <= len(prefix)+len(sep) || !strings.EqualFold(name[:len(prefix)], prefix) || name[len(prefix):len(prefix)+len(sep)] != sep {
			continue
		}
		rest := name[len(prefix)+len(sep):]
		owner, relative, _ := strings.Cut(rest, sep)
		if address, ok := mailboxAddress(owner); ok {
			return sharedPlacement{Mailbox: address, Relative: relative}, true
		}
		if ns.OtherUsers && plainOwnerName(owner) {
			return sharedPlacement{Mailbox: strings.ToLower(strings.TrimSpace(owner)), Relative: relative}, true
		}
		return sharedPlacement{}, false
	}
	return sharedPlacement{}, false
}

// plainOwnerName reports whether owner can be a person's or group's name on
// the server: one word, without "@" (an address is checked on its own).
func plainOwnerName(owner string) bool {
	owner = strings.TrimSpace(owner)
	return owner != "" && !strings.ContainsAny(owner, " @<>\"\t")
}

// IsAddress reports whether a shared mailbox is named by an email address,
// and so can be sent from.
func IsAddress(mailbox string) bool {
	_, ok := mailboxAddress(mailbox)
	return ok
}

// mailboxAddress returns owner as a lower-case address when it is exactly one
// plain email address.
func mailboxAddress(owner string) (string, bool) {
	owner = strings.TrimSpace(owner)
	if owner == "" || strings.ContainsAny(owner, " <>\"") {
		return "", false
	}
	parsed, err := mail.ParseAddress(owner)
	if err != nil || !strings.EqualFold(parsed.Address, owner) {
		return "", false
	}
	local, domain, ok := strings.Cut(parsed.Address, "@")
	if !ok || local == "" || !strings.Contains(domain, ".") {
		return "", false
	}
	return strings.ToLower(parsed.Address), true
}

// applySharedPlacement marks the folders that live in shared mailboxes. Their
// Role becomes "custom" and the role they have inside the shared mailbox goes
// to SharedRole.
func applySharedPlacement(folders []FolderInfo, namespaces []sharedNamespace) {
	if len(namespaces) == 0 {
		return
	}
	for i := range folders {
		f := &folders[i]
		placement, ok := sharedPlacementOf(f.Name, f.Delimiter, namespaces)
		if !ok {
			continue
		}
		f.SharedMailbox = placement.Mailbox
		switch {
		case placement.Mailbox == "":
			f.SharedRole = SharedRoleNamespace
		case placement.Relative == "":
			f.SharedRole = SharedRoleRoot
		default:
			// Inside the shared mailbox the name is matched on its own, so
			// "Inbox" there is that mailbox's inbox.
			f.SharedRole = detectFolderRole(placement.Relative, f.Delimiter, f.Attributes, []string{})
		}
		f.Role = "custom"
	}
}

// NamespaceReport is what the server answered to NAMESPACE: the personal
// prefixes and those of the shared namespaces, each without its delimiter
// and followed by "(other users)" or "(shared)". known is false when it
// could not answer.
func (c *Client) NamespaceReport(ctx context.Context) (personal, shared []string, known bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, nil, false, fmt.Errorf("client is closed")
	}
	set := c.namespaces()
	for _, ns := range set.Shared {
		kind := "(shared)"
		if ns.OtherUsers {
			kind = "(other users)"
		}
		shared = append(shared, ns.Prefix+" "+kind)
	}
	return set.Personal, shared, set.Known, nil
}
