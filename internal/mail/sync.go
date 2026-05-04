package mail

import (
	"context"
	"log"
	"sync"

	"gofer.email/internal/config"
	"gofer.email/internal/mail/imap"
	"gofer.email/internal/storage"
	"gofer.email/internal/store"
)

type SyncOrchestrator struct {
	db           *storage.DB
	accountStore *config.AccountStore
	blobStore    *store.BlobStore
	mu           sync.Mutex
	running      map[string]bool
}

func NewSyncOrchestrator(db *storage.DB, accountStore *config.AccountStore, blobStore *store.BlobStore) *SyncOrchestrator {
	return &SyncOrchestrator{
		db:           db,
		accountStore: accountStore,
		blobStore:    blobStore,
		running:      make(map[string]bool),
	}
}

func (o *SyncOrchestrator) BlobStore() *store.BlobStore {
	return o.blobStore
}

func (o *SyncOrchestrator) SyncAccount(ctx context.Context, accountID string) {
	o.mu.Lock()
	if o.running[accountID] {
		o.mu.Unlock()
		return
	}
	o.running[accountID] = true
	o.mu.Unlock()

	go func() {
		defer func() {
			o.mu.Lock()
			delete(o.running, accountID)
			o.mu.Unlock()
		}()

		if err := o.syncAccount(ctx, accountID); err != nil {
			log.Printf("sync account %s: %v", accountID, err)
		}
	}()
}

func (o *SyncOrchestrator) syncAccount(ctx context.Context, accountID string) error {
	cfg, err := o.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		return err
	}

	password, err := o.accountStore.DecryptPassword(ctx, accountID)
	if err != nil {
		return err
	}

	client, err := imap.NewClient(ctx, cfg, password)
	if err != nil {
		return err
	}
	defer client.Close()

	folders, err := client.ListFolders(ctx)
	if err != nil {
		return err
	}

	// Discover/upsert folders first
	var folderInputs []storage.UpsertFolderInput
	sortOrder := map[string]int{"inbox": 0, "starred": 1, "sent": 2, "drafts": 3, "archive": 4, "junk": 5, "trash": 6}

	for i, f := range folders {
		role := f.Role
		order, ok := sortOrder[role]
		if !ok {
			order = 100 + i
		}

		parentID := ""
		if f.Delimiter != 0 && containsDelimiter(f.Name, f.Delimiter) {
			parts := splitDelimiter(f.Name, f.Delimiter)
			parentID = folderIDFromRemote(accountID, parts[0])
		}

		folderInputs = append(folderInputs, storage.UpsertFolderInput{
			ID:        folderIDFromRemote(accountID, f.Name),
			AccountID: accountID,
			ParentID:  parentID,
			RemoteID:  f.Name,
			Name:      displayName(f.Name, role),
			Icon:      imap.RoleIcon(role),
			Role:      role,
			SortOrder: order,
		})
	}

	if len(folderInputs) > 0 {
		if err := o.db.UpsertFolders(ctx, folderInputs); err != nil {
			log.Printf("sync folders for %s: %v", accountID, err)
		}
	}

	// Sync messages for each folder
	for _, f := range folders {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		folderDBID := folderIDFromRemote(accountID, f.Name)
		o.syncFolderMessages(ctx, client, accountID, folderDBID, f.Name)
	}

	return nil
}

func (o *SyncOrchestrator) syncFolderMessages(ctx context.Context, client *imap.Client, accountID, folderID, remoteName string) {
	result, err := client.SyncFolder(ctx, folderID, remoteName, 500, func(msgs []storage.SyncMessage) error {
		return o.db.UpsertSyncMessages(ctx, msgs)
	})
	if err != nil {
		log.Printf("sync messages %s/%s: %v", accountID, remoteName, err)
		o.db.Write().ExecContext(ctx,
			`UPDATE folders SET sync_error = ? WHERE id = ?`, err.Error(), folderID)
		return
	}

	if result != nil {
		o.db.UpdateFolderSyncState(ctx, folderID, result.HighestUID, result.UIDValidity, int(result.NumMessages))
	}
	log.Printf("synced %s/%s: %d messages", accountID, remoteName, result.TotalFetched)
}

func folderIDFromRemote(accountID, remoteName string) string {
	return accountID + "_" + sanitizeRemote(remoteName)
}

func sanitizeRemote(name string) string {
	result := make([]rune, 0, len(name))
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			result = append(result, r)
		} else if r >= 'A' && r <= 'Z' {
			result = append(result, r+32)
		} else {
			result = append(result, '_')
		}
	}
	return string(result)
}

func containsDelimiter(name string, delim rune) bool {
	for _, r := range name {
		if r == delim {
			return true
		}
	}
	return false
}

func splitDelimiter(name string, delim rune) []string {
	for i, r := range name {
		if r == delim {
			return []string{name[:i], name[i+1:]}
		}
	}
	return []string{name}
}

func displayName(remoteName, role string) string {
	if role != "custom" {
		switch role {
		case "inbox":
			return "Inbox"
		case "sent":
			return "Sent"
		case "drafts":
			return "Drafts"
		case "trash":
			return "Trash"
		case "junk":
			return "Spam"
		case "archive":
			return "Archive"
		case "starred":
			return "Starred"
		}
	}
	return remoteName
}
