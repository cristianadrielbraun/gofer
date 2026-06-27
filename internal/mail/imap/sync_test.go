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

func TestLabelsFromFlagsKeepsPredefinedKeywordLabels(t *testing.T) {
	labels := labelsFromFlags([]goimap.Flag{
		"$label1",
		"$Label4",
		"$label5",
		"Client",
	})
	if len(labels) != 4 {
		t.Fatalf("labelsFromFlags() = %#v, want four labels", labels)
	}
	want := map[string]string{
		"$label1": "$label1",
		"$Label4": "$Label4",
		"$label5": "$label5",
		"Client":  "Client",
	}
	for _, label := range labels {
		if got := want[label.Name]; got == "" || got != label.ProviderID {
			t.Fatalf("label = %#v, want provider id %q", label, got)
		}
	}
}

func TestValidateKeywordAllowsProviderKeywordIDs(t *testing.T) {
	tests := []string{
		"$label2",
		"$VendorFlag",
		"Client",
	}
	for _, input := range tests {
		got, err := ValidateKeyword(input)
		if err != nil {
			t.Fatalf("ValidateKeyword(%q) error = %v", input, err)
		}
		if got != input {
			t.Fatalf("ValidateKeyword(%q) = %q, want same keyword", input, got)
		}
	}
	if _, err := ValidateKeyword("$Junk"); err == nil {
		t.Fatal("ValidateKeyword($Junk) error = nil, want status keyword rejection")
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
