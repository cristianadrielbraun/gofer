package mail

import (
	"context"
	"log"
	"sync"
	"time"

	"gofer.email/internal/config"
	"gofer.email/internal/mail/imap"
	"gofer.email/internal/storage"
	"gofer.email/internal/store"
)

type SyncOrchestrator struct {
	db           *storage.DB
	accountStore *config.AccountStore
	blobStore    *store.BlobStore
	events       *EventBus
	mu           sync.Mutex
	running      map[string]bool
	idleWatchers map[string][]*imap.IdleWatcher
	cancelFuncs  map[string]context.CancelFunc
}

func NewSyncOrchestrator(db *storage.DB, accountStore *config.AccountStore, blobStore *store.BlobStore) *SyncOrchestrator {
	return &SyncOrchestrator{
		db:           db,
		accountStore: accountStore,
		blobStore:    blobStore,
		events:       NewEventBus(),
		running:      make(map[string]bool),
		idleWatchers: make(map[string][]*imap.IdleWatcher),
		cancelFuncs:  make(map[string]context.CancelFunc),
	}
}

func (o *SyncOrchestrator) BlobStore() *store.BlobStore {
	return o.blobStore
}

func (o *SyncOrchestrator) Events() *EventBus {
	return o.events
}

func (o *SyncOrchestrator) Start(ctx context.Context) {
	accounts, err := o.db.GetAccountIDs(ctx)
	if err != nil {
		log.Printf("sync start: get accounts: %v", err)
		return
	}

	for _, accountID := range accounts {
		o.startAccount(ctx, accountID)
	}
}

func (o *SyncOrchestrator) startAccount(ctx context.Context, accountID string) {
	if err := o.syncAccount(ctx, accountID); err != nil {
		log.Printf("sync account %s: %v", accountID, err)
	}

	cfg, err := o.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		log.Printf("sync idle %s: get config: %v", accountID, err)
		return
	}

	password, err := o.accountStore.DecryptPassword(ctx, accountID)
	if err != nil {
		log.Printf("sync idle %s: decrypt: %v", accountID, err)
		return
	}

	idleFolders := []string{"inbox"}
	var watchers []*imap.IdleWatcher

	for _, role := range idleFolders {
		folderID, remoteName, err := o.db.GetFolderIDByRole(ctx, accountID, role)
		if err != nil {
			log.Printf("sync idle %s: find %s: %v", accountID, role, err)
			continue
		}
		if folderID == "" {
			continue
		}

		watcher := imap.NewIdleWatcher(cfg, password, remoteName, func() {
			o.syncIncremental(ctx, accountID, folderID, remoteName)
		})
		watchers = append(watchers, watcher)
		go watcher.Run(ctx)
	}

	o.mu.Lock()
	o.idleWatchers[accountID] = watchers
	o.mu.Unlock()

	accountCtx, cancel := context.WithCancel(ctx)
	o.mu.Lock()
	o.cancelFuncs[accountID] = cancel
	o.mu.Unlock()

	go o.runPeriodicSync(accountCtx, accountID)
}

func (o *SyncOrchestrator) runPeriodicSync(ctx context.Context, accountID string) {
	mediumPriorityRoles := map[string]bool{"sent": true, "drafts": true, "starred": true}

	mediumTicker := time.NewTicker(5 * time.Minute)
	defer mediumTicker.Stop()
	lowTicker := time.NewTicker(30 * time.Minute)
	defer lowTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-mediumTicker.C:
			o.periodicSync(ctx, accountID, mediumPriorityRoles, 5*time.Minute)
		case <-lowTicker.C:
			o.periodicSync(ctx, accountID, nil, 30*time.Minute)
		}
	}
}

func (o *SyncOrchestrator) periodicSync(ctx context.Context, accountID string, onlyRoles map[string]bool, interval time.Duration) {
	folders, err := o.db.GetFoldersForAccount(ctx, accountID)
	if err != nil {
		log.Printf("periodic %s: get folders: %v", accountID, err)
		return
	}

	cfg, err := o.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		log.Printf("periodic %s: config: %v", accountID, err)
		return
	}

	password, err := o.accountStore.DecryptPassword(ctx, accountID)
	if err != nil {
		log.Printf("periodic %s: password: %v", accountID, err)
		return
	}

	client, err := imap.NewClient(ctx, cfg, password)
	if err != nil {
		log.Printf("periodic %s: connect: %v", accountID, err)
		return
	}
	defer client.Close()

	for _, folder := range folders {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if onlyRoles != nil && !onlyRoles[folder.Role] {
			continue
		}

		if folder.Role == "inbox" {
			if folder.LastIncrementalAt.Valid && time.Since(folder.LastIncrementalAt.Time) < interval {
				continue
			}
		}

		if err := o.fullFolderSync(ctx, client, accountID, folder); err != nil {
			log.Printf("periodic %s/%s: %v", accountID, folder.RemoteID, err)
		}
	}
}

func (o *SyncOrchestrator) fullFolderSync(ctx context.Context, client *imap.Client, accountID string, folder storage.FolderSyncInfo) error {
	storedValidity, _ := o.db.GetStoredUIDValidity(ctx, folder.ID)

	if folder.LastFullSyncAt.Valid && storedValidity > 0 {
		currentValidity, _, err := client.CheckUIDValidity(ctx, folder.RemoteID)
		if err != nil {
			return err
		}
		if currentValidity != storedValidity && currentValidity > 0 {
			log.Printf("UIDVALIDITY changed for %s/%s: %d -> %d, clearing local state", accountID, folder.RemoteID, storedValidity, currentValidity)
			if err := o.db.ClearFolderMessages(ctx, folder.ID); err != nil {
				return err
			}
			o.syncFolderMessages(ctx, client, accountID, folder.ID, folder.RemoteID)
			o.events.Publish(Event{
				Type:      EventSyncComplete,
				AccountID: accountID,
				FolderID:  folder.ID,
			})
			return nil
		}
	}

	if folder.LastFullSyncAt.Valid {
		o.reconcileFolder(ctx, client, accountID, folder)
	}

	highestUID, err := o.db.GetHighestSeenUID(ctx, folder.ID)
	if err != nil {
		return err
	}

	if highestUID > 0 {
		result, err := client.SyncFolderIncremental(ctx, folder.ID, folder.RemoteID, highestUID, func(msgs []storage.SyncMessage) error {
			return o.db.UpsertSyncMessages(ctx, msgs)
		})
		if err != nil {
			log.Printf("periodic incremental %s/%s: %v", accountID, folder.RemoteID, err)
		} else if result != nil {
			o.db.UpdateFolderIncrementalSync(ctx, folder.ID, result.HighestUID, result.UIDValidity, int(result.NumMessages))
			if result.TotalFetched > 0 {
				log.Printf("periodic incremental %s/%s: %d new", accountID, folder.RemoteID, result.TotalFetched)
			}
		}
	} else {
		o.syncFolderMessages(ctx, client, accountID, folder.ID, folder.RemoteID)
	}

	o.refreshFlags(ctx, client, accountID, folder)

	o.db.RefreshFolderUnreadCount(ctx, folder.ID)

	o.events.Publish(Event{
		Type:      EventSyncComplete,
		AccountID: accountID,
		FolderID:  folder.ID,
	})

	return nil
}

func (o *SyncOrchestrator) reconcileFolder(ctx context.Context, client *imap.Client, accountID string, folder storage.FolderSyncInfo) {
	serverUIDs, err := client.FetchAllUIDs(ctx, folder.RemoteID)
	if err != nil {
		log.Printf("reconcile %s/%s: fetch uids: %v", accountID, folder.RemoteID, err)
		return
	}

	localUIDs, err := o.db.GetLocalUIDs(ctx, folder.ID)
	if err != nil {
		log.Printf("reconcile %s/%s: local uids: %v", accountID, folder.RemoteID, err)
		return
	}

	serverSet := make(map[uint32]bool, len(serverUIDs))
	for _, uid := range serverUIDs {
		serverSet[uid] = true
	}

	var expunged []uint32
	for uid := range localUIDs {
		if !serverSet[uid] {
			expunged = append(expunged, uid)
		}
	}

	if len(expunged) > 0 {
		removed, err := o.db.RemoveExpungedUIDs(ctx, folder.ID, expunged)
		if err != nil {
			log.Printf("reconcile %s/%s: remove: %v", accountID, folder.RemoteID, err)
		} else {
			log.Printf("reconcile %s/%s: removed %d expunged messages", accountID, folder.RemoteID, removed)
		}
	}
}

func (o *SyncOrchestrator) refreshFlags(ctx context.Context, client *imap.Client, accountID string, folder storage.FolderSyncInfo) {
	localUIDs, err := o.db.GetLocalUIDs(ctx, folder.ID)
	if err != nil {
		log.Printf("flags %s/%s: local uids: %v", accountID, folder.RemoteID, err)
		return
	}

	if len(localUIDs) == 0 {
		return
	}

	uids := make([]uint32, 0, len(localUIDs))
	for uid := range localUIDs {
		uids = append(uids, uid)
	}

	flagUpdates, err := client.FetchFlags(ctx, folder.RemoteID, uids)
	if err != nil {
		log.Printf("flags %s/%s: fetch: %v", accountID, folder.RemoteID, err)
		return
	}

	changed, err := o.db.BatchUpdateFlags(ctx, folder.ID, convertFlagUpdates(flagUpdates))
	if err != nil {
		log.Printf("flags %s/%s: update: %v", accountID, folder.RemoteID, err)
	} else if changed > 0 {
		log.Printf("flags %s/%s: %d changed", accountID, folder.RemoteID, changed)
		o.db.RefreshFolderUnreadCount(ctx, folder.ID)
	}
}

func convertFlagUpdates(imapUpdates []imap.FlagUpdate) []storage.FlagUpdate {
	updates := make([]storage.FlagUpdate, len(imapUpdates))
	for i, u := range imapUpdates {
		updates[i] = storage.FlagUpdate{
			UID:       u.UID,
			IsRead:    u.IsRead,
			IsStarred: u.IsStarred,
		}
	}
	return updates
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
	o.db.RefreshFolderUnreadCount(ctx, folderID)
	log.Printf("synced %s/%s: %d messages", accountID, remoteName, result.TotalFetched)
}

func (o *SyncOrchestrator) syncIncremental(ctx context.Context, accountID, folderID, remoteName string) {
	highestUID, err := o.db.GetHighestSeenUID(ctx, folderID)
	if err != nil {
		log.Printf("incremental %s/%s: get uid: %v", accountID, remoteName, err)
		return
	}

	cfg, err := o.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		log.Printf("incremental %s/%s: config: %v", accountID, remoteName, err)
		return
	}

	password, err := o.accountStore.DecryptPassword(ctx, accountID)
	if err != nil {
		log.Printf("incremental %s/%s: password: %v", accountID, remoteName, err)
		return
	}

	client, err := imap.NewClient(ctx, cfg, password)
	if err != nil {
		log.Printf("incremental %s/%s: connect: %v", accountID, remoteName, err)
		return
	}
	defer client.Close()

	o.reconcileAndRefresh(ctx, client, accountID, folderID, remoteName)

	result, err := client.SyncFolderIncremental(ctx, folderID, remoteName, highestUID, func(msgs []storage.SyncMessage) error {
		return o.db.UpsertSyncMessages(ctx, msgs)
	})
	if err != nil {
		log.Printf("incremental %s/%s: %v", accountID, remoteName, err)
		return
	}

	if result.TotalFetched > 0 {
		log.Printf("incremental %s/%s: %d new messages", accountID, remoteName, result.TotalFetched)
	}

	if result != nil {
		o.db.UpdateFolderIncrementalSync(ctx, folderID, result.HighestUID, result.UIDValidity, int(result.NumMessages))
	}

	unread, _ := o.db.RefreshFolderUnreadCount(ctx, folderID)

	o.events.Publish(Event{
		Type:      EventNewMail,
		AccountID: accountID,
		FolderID:  folderID,
	})
	_ = unread
}

func (o *SyncOrchestrator) reconcileAndRefresh(ctx context.Context, client *imap.Client, accountID, folderID, remoteName string) {
	localUIDs, err := o.db.GetLocalUIDs(ctx, folderID)
	if err != nil {
		log.Printf("reconcile %s/%s: local uids: %v", accountID, remoteName, err)
		return
	}

	if len(localUIDs) == 0 {
		return
	}

	serverUIDs, err := client.FetchAllUIDs(ctx, remoteName)
	if err != nil {
		log.Printf("reconcile %s/%s: fetch uids: %v", accountID, remoteName, err)
		return
	}

	serverSet := make(map[uint32]bool, len(serverUIDs))
	for _, uid := range serverUIDs {
		serverSet[uid] = true
	}

	var expunged []uint32
	for uid := range localUIDs {
		if !serverSet[uid] {
			expunged = append(expunged, uid)
		}
	}

	if len(expunged) > 0 {
		removed, err := o.db.RemoveExpungedUIDs(ctx, folderID, expunged)
		if err != nil {
			log.Printf("reconcile %s/%s: remove: %v", accountID, remoteName, err)
		} else if removed > 0 {
			log.Printf("reconcile %s/%s: removed %d expunged", accountID, remoteName, removed)
		}
	}

	uids := make([]uint32, 0, len(localUIDs))
	for uid := range localUIDs {
		if serverSet[uid] {
			uids = append(uids, uid)
		}
	}

	flagUpdates, err := client.FetchFlags(ctx, remoteName, uids)
	if err != nil {
		log.Printf("flags %s/%s: fetch: %v", accountID, remoteName, err)
		return
	}

	changed, err := o.db.BatchUpdateFlags(ctx, folderID, convertFlagUpdates(flagUpdates))
	if err != nil {
		log.Printf("flags %s/%s: update: %v", accountID, remoteName, err)
	} else if changed > 0 {
		log.Printf("flags %s/%s: %d changed", accountID, remoteName, changed)
	}
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
