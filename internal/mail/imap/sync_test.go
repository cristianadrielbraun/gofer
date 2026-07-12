package imap

import (
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/cristianadrielbraun/gofer/internal/storage"
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

func TestUIDStateNeedsReset(t *testing.T) {
	tests := []struct {
		name       string
		expected   uint32
		current    uint32
		highestUID uint32
		want       bool
	}{
		{name: "same generation", expected: 100, current: 100, highestUID: 5000},
		{name: "new generation", expected: 100, current: 200, highestUID: 5000, want: true},
		{name: "no current validity", expected: 100, highestUID: 5000},
		{name: "new folder without cursor", current: 200},
		{name: "cursor without known generation", current: 200, highestUID: 5000, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := uidStateNeedsReset(tt.expected, tt.current, tt.highestUID); got != tt.want {
				t.Fatalf("uidStateNeedsReset(%d, %d, %d) = %v, want %v", tt.expected, tt.current, tt.highestUID, got, tt.want)
			}
		})
	}
}

func TestFinalizeSyncMessageUsesIMAPDatePrecedence(t *testing.T) {
	headerDate := time.Date(2020, 1, 2, 3, 4, 5, 0, time.FixedZone("header", 2*60*60))
	internalDate := time.Date(2021, 2, 3, 4, 5, 6, 0, time.UTC)
	fallbackDate := time.Date(2022, 3, 4, 5, 6, 7, 0, time.UTC)
	tests := []struct {
		name         string
		dates        messageDateCandidates
		want         time.Time
		wantFallback bool
	}{
		{name: "header wins", dates: messageDateCandidates{envelopeDate: headerDate, internalDate: internalDate}, want: headerDate.UTC()},
		{name: "internal date fills missing header", dates: messageDateCandidates{internalDate: internalDate}, want: internalDate},
		{name: "fallback only when both are missing", dates: messageDateCandidates{}, want: fallbackDate, wantFallback: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := storage.SyncMessage{RemoteUID: 42}
			finalizeSyncMessage(&msg, "folder", tt.dates, fallbackDate)
			if !msg.DateSent.Equal(tt.want) || msg.DateSentFallback != tt.wantFallback {
				t.Fatalf("finalized date=%v fallback=%v, want date=%v fallback=%v", msg.DateSent, msg.DateSentFallback, tt.want, tt.wantFallback)
			}
			if msg.MessageID != "<folder-42@sync.gofer>" || msg.Subject != "(no subject)" || msg.Snippet != "(no subject)" {
				t.Fatalf("finalized metadata = %#v, want synthetic defaults", msg)
			}
		})
	}
}
