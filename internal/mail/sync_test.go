package mail

import (
	"context"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestAccountSyncParallelismTreatsZeroAsUnlimited(t *testing.T) {
	if got := accountSyncParallelism(4, 0); got != 4 {
		t.Fatalf("parallelism = %d, want 4", got)
	}
}

func TestAccountSyncParallelismCapsPositiveLimit(t *testing.T) {
	if got := accountSyncParallelism(4, 3); got != 3 {
		t.Fatalf("parallelism = %d, want 3", got)
	}
}

func TestBackgroundSyncSlotsTreatsZeroAsUnlimited(t *testing.T) {
	if slots := newAccountSyncSlots(0); slots != nil {
		t.Fatalf("slots = %#v, want nil", slots)
	}

	orchestrator := &SyncOrchestrator{backgroundSyncSlots: newAccountSyncSlots(0)}
	release, ok := orchestrator.acquireBackgroundSyncSlot(context.Background())
	if !ok {
		t.Fatal("acquire returned false for unlimited slots")
	}
	release()
}

func TestPollingFoldersForPeriodicSyncExcludesIdleRoles(t *testing.T) {
	folders := []storage.FolderSyncInfo{
		{ID: "inbox", Role: "inbox"},
		{ID: "sent", Role: "sent"},
		{ID: "archive", Role: "archive"},
		{ID: "custom", Role: "custom"},
	}

	got, excluded := pollingFoldersForPeriodicSync(folders, map[string]bool{
		"inbox": true,
		"sent":  true,
	})

	if excluded != 2 {
		t.Fatalf("excluded = %d, want 2", excluded)
	}
	if len(got) != 2 || got[0].ID != "archive" || got[1].ID != "custom" {
		t.Fatalf("polling folders = %#v, want archive and custom", got)
	}
}

func TestPollingFoldersForPeriodicSyncKeepsAllWithoutIdleRoles(t *testing.T) {
	folders := []storage.FolderSyncInfo{
		{ID: "inbox", Role: "inbox"},
		{ID: "archive", Role: "archive"},
	}

	got, excluded := pollingFoldersForPeriodicSync(folders, map[string]bool{})

	if excluded != 0 {
		t.Fatalf("excluded = %d, want 0", excluded)
	}
	if len(got) != len(folders) {
		t.Fatalf("polling folders len = %d, want %d", len(got), len(folders))
	}
}

func TestPollingIMAPFoldersForAutomaticSyncExcludesIdleRoles(t *testing.T) {
	folders := []imap.FolderInfo{
		{Name: "INBOX", Role: "inbox", Selectable: true},
		{Name: "Sent", Role: "sent", Selectable: true},
		{Name: "Archive", Role: "archive", Selectable: true},
		{Name: "Projects", Role: "custom", Selectable: true},
	}

	got, excluded := pollingIMAPFoldersForAutomaticSync(folders, map[string]bool{
		"inbox": true,
		"sent":  true,
	})

	if excluded != 2 {
		t.Fatalf("excluded = %d, want 2", excluded)
	}
	if len(got) != 2 || got[0].Name != "Archive" || got[1].Name != "Projects" {
		t.Fatalf("polling folders = %#v, want Archive and Projects", got)
	}
}
