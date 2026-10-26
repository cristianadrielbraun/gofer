package imap

import (
	"context"
	"slices"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
)

func folderNames(t *testing.T, client *Client) []string {
	t.Helper()
	folders, err := client.ListFolders(context.Background())
	if err != nil {
		t.Fatalf("ListFolders() error = %v", err)
	}
	var names []string
	for _, folder := range folders {
		names = append(names, folder.Name)
	}
	slices.Sort(names)
	return names
}

// Folders are made under their parent with the server's delimiter, renamed
// with the folders inside them, and deleted.
func TestManageMailboxes(t *testing.T) {
	ctx := context.Background()
	cfg, _ := startRecordedIMAPServerWithMailboxes(t, goimap.CapSet{goimap.CapIMAP4rev1: {}}, []string{"INBOX"})
	client, err := NewClient(ctx, cfg, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.Close()

	top, err := client.NewMailboxName(ctx, "", "Projects")
	if err != nil || top != "Projects" {
		t.Fatalf("NewMailboxName(top) = %q, %v", top, err)
	}
	if err := client.CreateSubscribedMailbox(ctx, top); err != nil {
		t.Fatalf("CreateSubscribedMailbox(%q) error = %v", top, err)
	}
	child, err := client.NewMailboxName(ctx, top, "Garden")
	if err != nil || child != "Projects/Garden" {
		t.Fatalf("NewMailboxName(child) = %q, %v", child, err)
	}
	if err := client.CreateSubscribedMailbox(ctx, child); err != nil {
		t.Fatalf("CreateSubscribedMailbox(%q) error = %v", child, err)
	}
	if _, err := client.NewMailboxName(ctx, "", "A/B"); err == nil {
		t.Fatal("a name with the delimiter was taken")
	}

	renamed, err := client.RenamedMailboxName(ctx, top, "Plans")
	if err != nil || renamed != "Plans" {
		t.Fatalf("RenamedMailboxName() = %q, %v", renamed, err)
	}
	if err := client.RenameMailbox(ctx, top, renamed); err != nil {
		t.Fatalf("RenameMailbox() error = %v", err)
	}
	if got, want := folderNames(t, client), []string{"INBOX", "Plans", "Plans/Garden"}; !slices.Equal(got, want) {
		t.Fatalf("folders after rename = %v, want %v", got, want)
	}
	if name, err := client.RenamedMailboxName(ctx, "Plans/Garden", "Yard"); err != nil || name != "Plans/Yard" {
		t.Fatalf("RenamedMailboxName(child) = %q, %v", name, err)
	}

	for _, mailbox := range []string{"Plans/Garden", "Plans"} {
		if err := client.DeleteMailbox(ctx, mailbox); err != nil {
			t.Fatalf("DeleteMailbox(%q) error = %v", mailbox, err)
		}
	}
	if got, want := folderNames(t, client), []string{"INBOX"}; !slices.Equal(got, want) {
		t.Fatalf("folders after delete = %v, want %v", got, want)
	}
}
