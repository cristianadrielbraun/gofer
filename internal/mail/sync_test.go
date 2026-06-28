package mail

import (
	"context"
	"sync"
	"testing"
	"time"

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

func TestAccountSyncProgressPayloadCarriesRunAccountIDs(t *testing.T) {
	accountIDs := []string{"acc-a", "acc-b"}
	ctx := withAccountSyncProgressScope(context.Background(), accountSyncProgressScope{
		kind:          "scheduled",
		userID:        "user-1",
		runID:         "run-1",
		accountIDs:    accountIDs,
		accountsTotal: len(accountIDs),
		accountIndex:  1,
		parallelism:   2,
	})

	payload := accountSyncProgressPayload(ctx, "", map[string]any{"status": "syncing"})
	got, ok := payload["account_ids"].([]string)
	if !ok {
		t.Fatalf("account_ids = %#v, want []string", payload["account_ids"])
	}
	if len(got) != 2 || got[0] != "acc-a" || got[1] != "acc-b" {
		t.Fatalf("account_ids = %#v, want full run roster", got)
	}

	accountIDs[0] = "mutated"
	if got[0] != "acc-a" {
		t.Fatalf("account_ids shares backing storage with scope input: %#v", got)
	}
}

func TestRegularManualSyncSkipsAccountCurrentlyRepairing(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('repairing', 'default', 'gmail', 'repairing@example.com'), ('other', 'default', 'gmail', 'other@example.com')`); err != nil {
		t.Fatalf("insert accounts: %v", err)
	}

	orchestrator := &SyncOrchestrator{
		db:         db,
		events:     NewEventBus(),
		running:    make(map[string]*accountSyncRun),
		manualRuns: make(map[string]map[string]*manualSyncRun),
	}
	events := orchestrator.events.Subscribe()
	defer orchestrator.events.Unsubscribe(events)

	repairStarted := make(chan struct{})
	releaseRepair := make(chan struct{})
	var repairOnce sync.Once
	repairRunID, repairStartedOK := orchestrator.syncAccountsWithOperation(ctx, "default", []string{"repairing"}, time.Minute, "repair", func(accountCtx context.Context, accountID string) error {
		repairOnce.Do(func() { close(repairStarted) })
		select {
		case <-accountCtx.Done():
			return accountCtx.Err()
		case <-releaseRepair:
			return nil
		}
	})
	if !repairStartedOK || repairRunID == "" {
		t.Fatalf("repair run started=%v runID=%q, want started", repairStartedOK, repairRunID)
	}

	select {
	case <-repairStarted:
	case <-time.After(time.Second):
		t.Fatal("repair operation did not start")
	}

	var syncedMu sync.Mutex
	var syncedAccounts []string
	syncRunID, syncStartedOK := orchestrator.syncAccountsWithOperation(ctx, "default", []string{"repairing", "other"}, time.Minute, "sync", func(accountCtx context.Context, accountID string) error {
		syncedMu.Lock()
		syncedAccounts = append(syncedAccounts, accountID)
		syncedMu.Unlock()
		return nil
	})
	if !syncStartedOK || syncRunID == "" {
		t.Fatalf("sync run started=%v runID=%q, want started while repair is active", syncStartedOK, syncRunID)
	}

	complete := waitForManualSyncComplete(t, events, syncRunID)
	if got := complete.Payload["skipped"]; got != 1 {
		t.Fatalf("sync skipped = %#v, want 1 repaired account skipped", got)
	}
	if got := complete.Payload["accounts_done"]; got != 2 {
		t.Fatalf("sync accounts_done = %#v, want both accounted for", got)
	}
	syncedMu.Lock()
	gotSynced := append([]string(nil), syncedAccounts...)
	syncedMu.Unlock()
	if len(gotSynced) != 1 || gotSynced[0] != "other" {
		t.Fatalf("synced accounts = %#v, want only other account", gotSynced)
	}

	close(releaseRepair)
	_ = waitForManualSyncComplete(t, events, repairRunID)
}

func TestActiveManualSyncSnapshotReplaysRepairFolderState(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('repairing', 'default', 'gmail', 'repairing@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "repairing_inbox", AccountID: "repairing", RemoteID: "INBOX", ProviderRemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	orchestrator := &SyncOrchestrator{
		db:         db,
		events:     NewEventBus(),
		running:    make(map[string]*accountSyncRun),
		manualRuns: make(map[string]map[string]*manualSyncRun),
	}
	eventCh := orchestrator.events.Subscribe()
	defer orchestrator.events.Unsubscribe(eventCh)

	repairCtx, cancelRepair := context.WithCancel(ctx)
	defer cancelRepair()
	repairStarted := make(chan struct{})
	releaseRepair := make(chan struct{})
	var repairOnce sync.Once
	repairRunID, repairStartedOK := orchestrator.syncAccountsWithOperation(repairCtx, "default", []string{"repairing"}, time.Minute, "repair", func(accountCtx context.Context, accountID string) error {
		repairOnce.Do(func() { close(repairStarted) })
		select {
		case <-accountCtx.Done():
			return accountCtx.Err()
		case <-releaseRepair:
			return nil
		}
	})
	if !repairStartedOK || repairRunID == "" {
		t.Fatalf("repair run started=%v runID=%q, want started", repairStartedOK, repairRunID)
	}

	select {
	case <-repairStarted:
	case <-time.After(time.Second):
		t.Fatal("repair operation did not start")
	}

	events := orchestrator.ActiveManualSyncSnapshot(ctx, "default")
	var sawRun bool
	var sawAccount bool
	var sawFolder bool
	for _, event := range events {
		switch event.Type {
		case EventManualSyncStarted:
			sawRun = event.Payload["run_id"] == repairRunID && event.Payload["mode"] == "repair"
		case EventManualSyncProgress:
			if event.AccountID == "repairing" && event.Payload["run_id"] == repairRunID && event.Payload["mode"] == "repair" && event.Payload["status"] == "syncing" {
				sawAccount = true
			}
		case EventSyncStarted:
			if event.AccountID == "repairing" && event.FolderID == "repairing_inbox" && event.Payload["mode"] == "repair" && event.Payload["provider"] == "gmail_api" && event.Payload["refresh_only"] == true {
				sawFolder = true
			}
		}
	}
	if !sawRun {
		t.Fatalf("snapshot events did not include active repair run: %#v", events)
	}
	if !sawAccount {
		t.Fatalf("snapshot events did not include active repair account progress: %#v", events)
	}
	if !sawFolder {
		t.Fatalf("snapshot events did not include active repair folder state: %#v", events)
	}

	close(releaseRepair)
	_ = waitForManualSyncComplete(t, eventCh, repairRunID)
}

func waitForManualSyncComplete(t *testing.T, events <-chan Event, runID string) Event {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Type != EventManualSyncComplete {
				continue
			}
			if event.Payload["run_id"] == runID {
				return event
			}
		case <-timeout:
			t.Fatalf("timed out waiting for manual sync complete run %q", runID)
		}
	}
}

func TestFolderSyncProgressPayloadCarriesFolderMetadata(t *testing.T) {
	orchestrator := &SyncOrchestrator{}

	payload := orchestrator.folderSyncProgressPayload(context.Background(), "acc", "Inbox", "gmail_api", map[string]any{
		"refresh_only":    true,
		"total_estimated": true,
	})

	if payload["current_folder"] != "Inbox" || payload["folder_name"] != "Inbox" {
		t.Fatalf("folder metadata = %#v, want Inbox labels", payload)
	}
	if payload["provider"] != "gmail_api" {
		t.Fatalf("provider = %#v, want gmail_api", payload["provider"])
	}
	if payload["refresh_only"] != true || payload["total_estimated"] != true {
		t.Fatalf("refresh metadata = %#v, want true flags", payload)
	}
}

func TestPollingFoldersForPeriodicSyncExcludesIdleFolderIDs(t *testing.T) {
	folders := []storage.FolderSyncInfo{
		{ID: "inbox", Role: "inbox"},
		{ID: "sent", Role: "sent"},
		{ID: "gmail_sent", Role: "sent"},
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
	if len(got) != 3 || got[0].ID != "gmail_sent" || got[1].ID != "archive" || got[2].ID != "custom" {
		t.Fatalf("polling folders = %#v, want gmail_sent, archive, and custom", got)
	}
}

func TestPollingFoldersForPeriodicSyncKeepsAllWithoutIdleFolderIDs(t *testing.T) {
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

func TestPollingIMAPFoldersForAutomaticSyncExcludesIdleRemoteNames(t *testing.T) {
	folders := []imap.FolderInfo{
		{Name: "INBOX", Role: "inbox", Selectable: true},
		{Name: "Sent", Role: "sent", Selectable: true},
		{Name: "[Gmail]/Sent Mail", Role: "sent", Selectable: true},
		{Name: "Archive", Role: "archive", Selectable: true},
		{Name: "Projects", Role: "custom", Selectable: true},
	}

	got, excluded := pollingIMAPFoldersForAutomaticSync(folders, map[string]bool{
		"INBOX": true,
		"Sent":  true,
	})

	if excluded != 2 {
		t.Fatalf("excluded = %d, want 2", excluded)
	}
	if len(got) != 3 || got[0].Name != "[Gmail]/Sent Mail" || got[1].Name != "Archive" || got[2].Name != "Projects" {
		t.Fatalf("polling folders = %#v, want [Gmail]/Sent Mail, Archive, and Projects", got)
	}
}
