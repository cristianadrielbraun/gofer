package imap

import (
	"slices"
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

func TestDetectFolderRoleDoesNotTreatGmailImportantAsStarred(t *testing.T) {
	if got := detectFolderRole("[Gmail]/Important", '/', nil, []string{""}); got != "custom" {
		t.Fatalf("detectFolderRole([Gmail]/Important) = %q, want custom", got)
	}
	if got := detectFolderRole("Important", '/', nil, []string{""}); got != "custom" {
		t.Fatalf("detectFolderRole(Important) = %q, want custom", got)
	}
}

func TestDetectFolderRoleIgnoresNamespacePrefix(t *testing.T) {
	tests := []struct {
		name       string
		folder     string
		delim      rune
		namespaces []string
		want       string
	}{
		{name: "reported prefix", folder: "INBOX.Trash", delim: '.', namespaces: []string{"INBOX."}, want: "trash"},
		{name: "reported prefix in other case", folder: "inbox.Sent", delim: '.', namespaces: []string{"INBOX."}, want: "sent"},
		{name: "assumed prefix without NAMESPACE", folder: "INBOX.Junk", delim: '.', want: "junk"},
		{name: "assumed prefix with slash", folder: "INBOX/Drafts", delim: '/', want: "drafts"},
		{name: "empty reported prefix keeps subfolder", folder: "INBOX.Trash", delim: '.', namespaces: []string{""}, want: "custom"},
		{name: "other reported prefix", folder: "Mail/Trash", delim: '/', namespaces: []string{"Mail/"}, want: "trash"},
		{name: "unprefixed folder", folder: "Trash", delim: '.', namespaces: []string{"INBOX."}, want: "trash"},
		{name: "inbox itself", folder: "INBOX", delim: '.', namespaces: []string{"INBOX."}, want: "inbox"},
		{name: "folder named inbox inside inbox", folder: "INBOX.Inbox", delim: '.', namespaces: []string{"INBOX."}, want: "custom"},
		{name: "custom folder", folder: "INBOX.Receipts", delim: '.', namespaces: []string{"INBOX."}, want: "custom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectFolderRole(tt.folder, tt.delim, nil, tt.namespaces); got != tt.want {
				t.Fatalf("detectFolderRole(%q) = %q, want %q", tt.folder, got, tt.want)
			}
		})
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

// A message Gofer stores in a folder while it is being reconciled, such as a
// new draft revision, gets a UID the server's earlier list cannot include. It
// must not be taken for a message removed from the server.
func TestUIDSnapshotLeavesLaterMessagesAlone(t *testing.T) {
	local := map[uint32]int64{3: 1, 5: 2, 7: 3, 11: 4}
	tests := []struct {
		name     string
		snapshot UIDSnapshot
		seenUID  uint32
		want     []uint32
	}{
		{"uid next", UIDSnapshot{UIDs: []uint32{3, 7}, UIDNext: 11}, 7, []uint32{5}},
		{"without uid next", UIDSnapshot{UIDs: []uint32{3, 7}}, 7, []uint32{5}},
		{"everything removed", UIDSnapshot{UIDNext: 12}, 11, []uint32{3, 5, 7, 11}},
		{"empty without uid next", UIDSnapshot{}, 0, nil},
		// UIDNEXT never goes down, but a server that lowers it after an
		// expunge must not keep messages sync has seen.
		{"understated uid next", UIDSnapshot{UIDs: []uint32{3}, UIDNext: 4}, 11, []uint32{5, 7, 11}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.snapshot.ExpungedUIDs(local, tt.seenUID)
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("ExpungedUIDs() = %v, want %v", got, tt.want)
			}
		})
	}
}
