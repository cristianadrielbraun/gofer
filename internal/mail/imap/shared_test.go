package imap

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestNamespaceSetFromDataTakesOtherAndSharedPrefixes(t *testing.T) {
	set := namespaceSetFromData(&imap.NamespaceData{
		Personal: []imap.NamespaceDescriptor{{Prefix: "", Delim: '/'}},
		Other:    []imap.NamespaceDescriptor{{Prefix: "Shared Folders/", Delim: '/'}},
		Shared:   []imap.NamespaceDescriptor{{Prefix: "Public", Delim: '/'}},
	})
	if len(set.Personal) != 1 || set.Personal[0] != "" {
		t.Fatalf("personal = %#v", set.Personal)
	}
	if len(set.Shared) != 2 || set.Shared[0] != (sharedNamespace{"Shared Folders", true}) || set.Shared[1] != (sharedNamespace{"Public", false}) {
		t.Fatalf("shared = %#v", set.Shared)
	}
}

func TestSharedPlacementOf(t *testing.T) {
	prefixes := []sharedNamespace{{Prefix: "Shared Folders"}}
	cases := []struct {
		name     string
		ok       bool
		mailbox  string
		relative string
	}{
		{"Shared Folders", true, "", ""},
		{"Shared Folders/support@example.com", true, "support@example.com", ""},
		{"Shared Folders/Support@Example.com/Sent Items", true, "support@example.com", "Sent Items"},
		{"Shared Folders/support@example.com/Inbox/Orders", true, "support@example.com", "Inbox/Orders"},
		// Not an address: left to the ordinary folder handling.
		{"Shared Folders/Team Notes", false, "", ""},
		{"Shared Folders/john", false, "", ""},
		// An address-like personal folder is not a shared mailbox.
		{"Projects/support@example.com/Inbox", false, "", ""},
		{"support@example.com", false, "", ""},
		// The prefix must end at a delimiter.
		{"Shared Folders2/support@example.com", false, "", ""},
	}
	for _, c := range cases {
		got, ok := sharedPlacementOf(c.name, '/', prefixes)
		if ok != c.ok || got.Mailbox != c.mailbox || got.Relative != c.relative {
			t.Errorf("%q: got %#v, %v; want mailbox %q relative %q, %v", c.name, got, ok, c.mailbox, c.relative, c.ok)
		}
	}
	if _, ok := sharedPlacementOf("Shared Folders/support@example.com", '/', nil); ok {
		t.Error("without a shared namespace nothing is a shared mailbox")
	}
}

// RFC 2342: in the "other users" namespace the level below the prefix is the
// owner's name, an address or not, as Dovecot's "shared/" or Cyrus's "Other
// Users." give it; in a "shared" namespace only an address names a mailbox.
func TestSharedPlacementOfOtherUsersNames(t *testing.T) {
	namespaces := []sharedNamespace{{Prefix: "Other Users", OtherUsers: true}, {Prefix: "Public", OtherUsers: false}}
	cases := []struct {
		name, delim string
		ok          bool
		mailbox     string
		relative    string
	}{
		{"Other Users.support", ".", true, "support", ""},
		{"Other Users.Support.Sent", ".", true, "support", "Sent"},
		{"Other Users.support@example.com.INBOX", ".", false, "", ""},
		{"Public.Announcements", ".", false, "", ""},
		{"Public.team@example.com.Sent", ".", false, "", ""},
		{"Public/team@example.com/Sent", "/", true, "team@example.com", "Sent"},
	}
	for _, c := range cases {
		got, ok := sharedPlacementOf(c.name, rune(c.delim[0]), namespaces)
		if ok != c.ok || got.Mailbox != c.mailbox || got.Relative != c.relative {
			t.Errorf("%q: got %#v, %v; want %q %q, %v", c.name, got, ok, c.mailbox, c.relative, c.ok)
		}
	}
	if !IsAddress("team@example.com") || IsAddress("support") {
		t.Error("IsAddress")
	}
}

func TestApplySharedPlacementScopesRoles(t *testing.T) {
	folders := []FolderInfo{
		{Name: "INBOX", Delimiter: '/', Role: "inbox"},
		{Name: "Sent Items", Delimiter: '/', Attributes: []imap.MailboxAttr{imap.MailboxAttrSent}, Role: "sent"},
		{Name: "Shared Folders", Delimiter: '/', Attributes: []imap.MailboxAttr{imap.MailboxAttrNoSelect}, Role: "custom"},
		{Name: "Shared Folders/support@example.com", Delimiter: '/', Attributes: []imap.MailboxAttr{imap.MailboxAttrNoSelect}, Role: "custom"},
		{Name: "Shared Folders/support@example.com/Inbox", Delimiter: '/', Role: "custom"},
		{Name: "Shared Folders/support@example.com/Sent Items", Delimiter: '/', Attributes: []imap.MailboxAttr{imap.MailboxAttrSent}, Role: "sent"},
		{Name: "Shared Folders/support@example.com/Deleted Items", Delimiter: '/', Role: "custom"},
		{Name: "Shared Folders/support@example.com/Junk Mail", Delimiter: '/', Attributes: []imap.MailboxAttr{imap.MailboxAttrJunk}, Role: "junk"},
		{Name: "Shared Folders/support@example.com/Orders", Delimiter: '/', Role: "custom"},
	}
	applySharedPlacement(folders, []sharedNamespace{{Prefix: "Shared Folders", OtherUsers: true}})

	want := []struct{ role, mailbox, sharedRole string }{
		{"inbox", "", ""},
		{"sent", "", ""},
		{"custom", "", SharedRoleNamespace},
		{"custom", "support@example.com", SharedRoleRoot},
		{"custom", "support@example.com", "inbox"},
		{"custom", "support@example.com", "sent"},
		{"custom", "support@example.com", "trash"},
		{"custom", "support@example.com", "junk"},
		{"custom", "support@example.com", "custom"},
	}
	for i, w := range want {
		f := folders[i]
		if f.Role != w.role || f.SharedMailbox != w.mailbox || f.SharedRole != w.sharedRole {
			t.Errorf("%q: role %q mailbox %q shared role %q; want %q %q %q", f.Name, f.Role, f.SharedMailbox, f.SharedRole, w.role, w.mailbox, w.sharedRole)
		}
	}
}

// A shared mailbox's top folder that holds mail, as Cyrus's "user.bob" does,
// is the mailbox's inbox; one that holds none, or has an INBOX inside, as
// Dovecot's "shared/bob" may, stays its root.
func TestApplySharedPlacementTakesASelectableTopFolderAsTheInbox(t *testing.T) {
	cyrus := []FolderInfo{
		{Name: "user.bob", Delimiter: '.', Role: "custom", Selectable: true},
		{Name: "user.bob.Sent", Delimiter: '.', Role: "custom", Selectable: true},
	}
	applySharedPlacement(cyrus, []sharedNamespace{{Prefix: "user", OtherUsers: true}})
	if cyrus[0].SharedMailbox != "bob" || cyrus[0].SharedRole != "inbox" || cyrus[1].SharedRole != "sent" {
		t.Fatalf("Cyrus folders = %+v", cyrus)
	}
	dovecot := []FolderInfo{
		{Name: "shared/bob", Delimiter: '/', Role: "custom", Selectable: true},
		{Name: "shared/bob/INBOX", Delimiter: '/', Role: "custom", Selectable: true},
		{Name: "shared/carol", Delimiter: '/', Attributes: []imap.MailboxAttr{imap.MailboxAttrNoSelect}, Role: "custom"},
	}
	applySharedPlacement(dovecot, []sharedNamespace{{Prefix: "shared", OtherUsers: true}})
	if dovecot[0].SharedRole != SharedRoleRoot || dovecot[1].SharedRole != "inbox" || dovecot[2].SharedRole != SharedRoleRoot {
		t.Fatalf("Dovecot folders = %+v", dovecot)
	}
}

// A personal folder whose name differs from a shared namespace prefix only
// in case stays the account's own, and "Deleted Items" and "Junk Mail" are
// Trash and Spam only inside a shared mailbox.
func TestSharedNamesStayInsideSharedMailboxes(t *testing.T) {
	folders := []FolderInfo{
		{Name: "Shared", Delimiter: '/', Role: "custom", Selectable: true},
		{Name: "Shared/notes", Delimiter: '/', Role: "custom", Selectable: true},
		{Name: "shared/bob/Deleted Items", Delimiter: '/', Role: "custom", Selectable: true},
		{Name: "shared/bob/Junk Mail", Delimiter: '/', Role: "custom", Selectable: true},
	}
	applySharedPlacement(folders, []sharedNamespace{{Prefix: "shared", OtherUsers: true}})
	if folders[0].SharedMailbox != "" || folders[0].SharedRole != "" || folders[1].SharedMailbox != "" {
		t.Fatalf("personal folders taken for shared ones: %+v %+v", folders[0], folders[1])
	}
	if folders[2].SharedRole != "trash" || folders[3].SharedRole != "junk" {
		t.Fatalf("shared mailbox folders = %+v %+v", folders[2], folders[3])
	}
	for _, name := range []string{"Deleted Items", "Junk Mail", "INBOX.Deleted Items"} {
		if role := detectFolderRole(name, '.', nil, []string{"INBOX."}); role != "custom" {
			t.Errorf("own folder %q has role %q", name, role)
		}
	}
}
