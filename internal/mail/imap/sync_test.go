package imap

import (
	"testing"

	goimap "github.com/emersion/go-imap/v2"
)

func TestLabelsFromFlagsSkipsReservedJunkFlags(t *testing.T) {
	labels := labelsFromFlags([]goimap.Flag{
		"NonJunk",
		"$NotJunk",
		"Junk",
		"Work",
		"\\Seen",
	})
	if len(labels) != 1 || labels[0].Name != "Work" {
		t.Fatalf("labelsFromFlags() = %#v, want only Work", labels)
	}
}

func TestDetectFolderRoleDoesNotTreatGmailImportantAsStarred(t *testing.T) {
	if got := detectFolderRole("[Gmail]/Important", nil); got != "custom" {
		t.Fatalf("detectFolderRole([Gmail]/Important) = %q, want custom", got)
	}
	if got := detectFolderRole("Important", nil); got != "custom" {
		t.Fatalf("detectFolderRole(Important) = %q, want custom", got)
	}
}
