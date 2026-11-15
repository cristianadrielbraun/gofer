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
