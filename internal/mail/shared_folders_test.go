package mail

import (
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
)

// A Stalwart-like listing: the account's folders, then each shared mailbox
// under "Shared Folders" with the same special folders. The listing leaves out
// the mailbox folders themselves, as a server may.
func TestIMAPFolderInputsKeepSharedMailboxFoldersApart(t *testing.T) {
	folders := []imap.FolderInfo{
		{Name: "INBOX", Delimiter: '/', Role: "inbox", Selectable: true},
		{Name: "Sent Items", Delimiter: '/', Role: "sent", Selectable: true},
		{Name: "Drafts", Delimiter: '/', Role: "drafts", Selectable: true},
		{Name: "Shared Folders/support@example.com/Inbox", Delimiter: '/', Role: "custom", Selectable: true, SharedMailbox: "support@example.com", SharedRole: "inbox"},
		{Name: "Shared Folders/support@example.com/Sent Items", Delimiter: '/', Role: "custom", Selectable: true, SharedMailbox: "support@example.com", SharedRole: "sent"},
		{Name: "Shared Folders/sales@example.com/Sent Items", Delimiter: '/', Role: "custom", Selectable: true, SharedMailbox: "sales@example.com", SharedRole: "sent"},
		{Name: "Shared Folders/sales@example.com/Drafts", Delimiter: '/', Role: "custom", Selectable: true, SharedMailbox: "sales@example.com", SharedRole: "drafts"},
	}
	folders = imapFoldersWithParents(folders)
	inputs, err := imapFolderInputs("acc", folders, map[string]string{"Shared Folders/sales@example.com/Drafts": "drafts"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	byRemote := map[string]int{}
	for i, in := range inputs {
		byRemote[in.RemoteID] = i
	}
	get := func(name string) (in struct{ role, mailbox, sharedRole, parent, name string }) {
		i, ok := byRemote[name]
		if !ok {
			t.Fatalf("no input for %q", name)
		}
		f := inputs[i]
		in.role, in.mailbox, in.sharedRole, in.parent, in.name = f.Role, f.SharedMailbox, f.SharedRole, f.ParentID, f.Name
		return in
	}

	roles := map[string]int{}
	for _, in := range inputs {
		if in.Role != "custom" {
			roles[in.Role]++
		}
	}
	for role, n := range roles {
		if n != 1 {
			t.Errorf("role %q is held by %d folders; only the account's own folder may hold it", role, n)
		}
	}
	if got := get("Sent Items"); got.role != "sent" || got.mailbox != "" {
		t.Errorf("own Sent = %+v", got)
	}

	if got := get("Shared Folders"); got.sharedRole != imap.SharedRoleNamespace {
		t.Errorf("namespace folder = %+v", got)
	}
	root := get("Shared Folders/support@example.com")
	if root.sharedRole != imap.SharedRoleRoot || root.mailbox != "support@example.com" {
		t.Errorf("mailbox folder = %+v", root)
	}
	sent := get("Shared Folders/support@example.com/Sent Items")
	if sent.role != "custom" || sent.sharedRole != "sent" || sent.mailbox != "support@example.com" || sent.name != "Sent Items" {
		t.Errorf("shared Sent = %+v", sent)
	}
	if sent.parent != inputs[byRemote["Shared Folders/support@example.com"]].ID {
		t.Errorf("shared Sent is not under its mailbox: parent %q", sent.parent)
	}
	// A role chosen by hand for a shared folder does not make it the
	// account's Drafts.
	if got := get("Shared Folders/sales@example.com/Drafts"); got.role != "custom" || got.sharedRole != "drafts" {
		t.Errorf("shared Drafts with a chosen role = %+v", got)
	}
}
