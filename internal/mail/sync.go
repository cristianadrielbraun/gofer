package mail

import (
	"context"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

const manualSyncMaxParallelAccounts = 3

type manualSyncJob struct {
	index     int
	accountID string
}

type manualSyncRun struct {
	runID  string
	cancel context.CancelFunc
}

type accountSyncKind string

const (
	accountSyncBackground accountSyncKind = "background"
	accountSyncManual     accountSyncKind = "manual"
)

type accountSyncRun struct {
	kind   accountSyncKind
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

type accountWorker struct {
	cancel context.CancelFunc
}

type newMailSummary struct {
	Count       int
	UnreadCount int
	Latest      storage.SyncMessage
	LatestSet   bool
}

type TokenProvider interface {
	GetOAuthTokenForAccount(ctx context.Context, accountID string) (string, error)
}

type SyncOrchestrator struct {
	db            *storage.DB
	accountStore  *config.AccountStore
	blobStore     *store.BlobStore
	tokenProvider TokenProvider
	events        *EventBus
	mu            sync.Mutex
	running       map[string]*accountSyncRun
	manualRuns    map[string]*manualSyncRun
	idleWatchers  map[string][]*imap.IdleWatcher
	cancelFuncs   map[string]*accountWorker
	interval      int
	intervalMu    sync.RWMutex
}

func NewSyncOrchestrator(db *storage.DB, accountStore *config.AccountStore, blobStore *store.BlobStore, tokenProvider TokenProvider) *SyncOrchestrator {
	return &SyncOrchestrator{
		db:            db,
		accountStore:  accountStore,
		blobStore:     blobStore,
		tokenProvider: tokenProvider,
		events:        NewEventBus(),
		running:       make(map[string]*accountSyncRun),
		manualRuns:    make(map[string]*manualSyncRun),
		idleWatchers:  make(map[string][]*imap.IdleWatcher),
		cancelFuncs:   make(map[string]*accountWorker),
		interval:      5,
	}
}

func (o *SyncOrchestrator) BlobStore() *store.BlobStore {
	return o.blobStore
}

func (o *SyncOrchestrator) Events() *EventBus {
	return o.events
}

func (o *SyncOrchestrator) UpdateInterval(minutes int) {
	o.intervalMu.Lock()
	o.interval = minutes
	o.intervalMu.Unlock()
}

func (o *SyncOrchestrator) StopAccount(accountID string) {
	var cancels []context.CancelFunc
	var watchers []*imap.IdleWatcher

	o.mu.Lock()
	if worker, ok := o.cancelFuncs[accountID]; ok {
		cancels = append(cancels, worker.cancel)
		delete(o.cancelFuncs, accountID)
	}
	if run := o.running[accountID]; run != nil {
		cancels = append(cancels, run.cancel)
	}
	watchers = append(watchers, o.idleWatchers[accountID]...)
	delete(o.idleWatchers, accountID)
	o.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	for _, w := range watchers {
		w.Close()
	}
}

func (o *SyncOrchestrator) StartAccount(ctx context.Context, accountID string) {
	if !o.db.IsEmailSyncEnabled(ctx, accountID) {
		return
	}
	o.mu.Lock()
	_, running := o.cancelFuncs[accountID]
	o.mu.Unlock()
	if running {
		return
	}
	go o.startAccount(ctx, accountID)
}

func (o *SyncOrchestrator) RestartIDLEWatchers(ctx context.Context) {
	o.mu.Lock()
	for accountID, watchers := range o.idleWatchers {
		for _, w := range watchers {
			w.Close()
		}
		delete(o.idleWatchers, accountID)
	}
	o.mu.Unlock()

	accounts, err := o.db.GetAllEmailSyncAccountIDs(ctx)
	if err != nil {
		return
	}

	for _, accountID := range accounts {
		o.startIDLEWatchers(ctx, accountID)
	}
}
func (o *SyncOrchestrator) startIDLEWatchers(ctx context.Context, accountID string) {
	if !o.db.IsEmailSyncEnabled(ctx, accountID) {
		return
	}
	cfg, err := o.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		log.Printf("idle %s: config: %v", accountID, err)
		return
	}

	password, err := o.resolvePassword(ctx, cfg, accountID)
	if err != nil {
		log.Printf("idle %s: password: %v", accountID, err)
		return
	}

	idleRoles := o.getIdleFoldersForAccount(ctx, accountID)
	var watchers []*imap.IdleWatcher

	for role := range idleRoles {
		folderID, remoteName, err := o.db.GetFolderIDByRole(ctx, accountID, role)
		if err != nil || folderID == "" {
			continue
		}

		fID := folderID
		watcher := imap.NewIdleWatcher(cfg, password, remoteName, func() {
			o.syncIncremental(ctx, accountID, fID, remoteName)
		})
		watchers = append(watchers, watcher)
		go watcher.Run(ctx)
	}

	o.mu.Lock()
	o.idleWatchers[accountID] = watchers
	o.mu.Unlock()
}

func (o *SyncOrchestrator) getInterval() int {
	o.intervalMu.RLock()
	defer o.intervalMu.RUnlock()
	return o.interval
}

func (o *SyncOrchestrator) resolvePassword(ctx context.Context, cfg *models.AccountConfig, accountID string) (string, error) {
	if cfg.AuthMethod == "oauth2" && o.tokenProvider != nil {
		return o.tokenProvider.GetOAuthTokenForAccount(ctx, accountID)
	}
	return o.accountStore.DecryptPassword(ctx, accountID)
}

func (o *SyncOrchestrator) getIdleFoldersForAccount(ctx context.Context, accountID string) map[string]bool {
	userID, err := o.db.GetAccountUserID(ctx, accountID)
	if err != nil || userID == "" {
		return map[string]bool{"inbox": true, "sent": true, "drafts": true}
	}
	return o.db.GetIdleFoldersForAccount(ctx, userID, accountID)
}

func (o *SyncOrchestrator) Start(ctx context.Context) {
	log.Printf("sync: startup scan started")
	accounts, err := o.db.GetAllEmailSyncAccountIDs(ctx)
	if err != nil {
		log.Printf("sync start: get accounts: %v", err)
		return
	}
	log.Printf("sync: found %d account(s)", len(accounts))

	if len(accounts) > 0 {
		userID, _ := o.db.GetAccountUserID(ctx, accounts[0])
		if userID != "" {
			if interval := o.db.GetSyncInterval(ctx, userID); interval > 0 {
				o.interval = interval
			}
		}
	}

	for _, accountID := range accounts {
		log.Printf("sync: starting account bootstrap for %s", accountID)
		o.startAccount(ctx, accountID)
	}
	log.Printf("sync: startup scan complete")
}

func (o *SyncOrchestrator) startAccount(ctx context.Context, accountID string) {
	if !o.db.IsEmailSyncEnabled(ctx, accountID) {
		log.Printf("sync: account %s email sync disabled", accountID)
		return
	}
	accountCtx, cancel := context.WithCancel(ctx)
	worker := &accountWorker{cancel: cancel}
	o.mu.Lock()
	if _, running := o.cancelFuncs[accountID]; running {
		o.mu.Unlock()
		cancel()
		return
	}
	o.cancelFuncs[accountID] = worker
	o.mu.Unlock()

	log.Printf("sync: account %s initial sync started", accountID)
	syncCtx, finish, ok := o.beginAccountSync(accountCtx, accountID, accountSyncBackground)
	if !ok {
		log.Printf("sync: account %s initial sync skipped, account already syncing", accountID)
	} else {
		if err := o.syncAccount(syncCtx, accountID); err != nil {
			log.Printf("sync account %s: %v", accountID, err)
		} else {
			log.Printf("sync: account %s initial sync finished", accountID)
		}
		finish()
	}
	if accountCtx.Err() != nil {
		o.clearAccountWorker(accountID, worker)
		return
	}

	o.startIDLEWatchers(accountCtx, accountID)
	log.Printf("sync: account %s IDLE watchers started", accountID)

	go o.runPeriodicSync(accountCtx, accountID)
	log.Printf("sync: account %s periodic sync worker started", accountID)
}

func (o *SyncOrchestrator) runPeriodicSync(ctx context.Context, accountID string) {
	for {
		interval := time.Duration(o.getInterval()) * time.Minute
		if interval < time.Minute {
			interval = time.Minute
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			o.periodicSync(ctx, accountID)
		}
	}
}

func (o *SyncOrchestrator) periodicSync(ctx context.Context, accountID string) {
	if !o.db.IsEmailSyncEnabled(ctx, accountID) {
		return
	}
	syncCtx, finish, ok := o.beginAccountSync(ctx, accountID, accountSyncBackground)
	if !ok {
		return
	}
	defer finish()

	folders, err := o.db.GetFoldersForAccount(syncCtx, accountID)
	if err != nil {
		log.Printf("periodic %s: get folders: %v", accountID, err)
		return
	}

	cfg, err := o.accountStore.GetConfig(syncCtx, accountID)
	if err != nil {
		log.Printf("periodic %s: config: %v", accountID, err)
		return
	}

	password, err := o.resolvePassword(syncCtx, cfg, accountID)
	if err != nil {
		log.Printf("periodic %s: password: %v", accountID, err)
		return
	}

	client, err := imap.NewClient(syncCtx, cfg, password)
	if err != nil {
		log.Printf("periodic %s: connect: %v", accountID, err)
		return
	}
	defer client.Close()

	for i, folder := range folders {
		select {
		case <-syncCtx.Done():
			return
		default:
		}

		if err := o.fullFolderSync(syncCtx, client, accountID, folder, i+1, len(folders)); err != nil {
			log.Printf("periodic %s/%s: %v", accountID, folder.RemoteID, err)
		}
	}
}

func (s *newMailSummary) Add(msgs []storage.SyncMessage) {
	for _, msg := range msgs {
		s.Count++
		if !msg.IsRead {
			s.UnreadCount++
		}
		if !s.LatestSet || msg.DateSent.After(s.Latest.DateSent) {
			s.Latest = msg
			s.LatestSet = true
		}
	}
}

func (o *SyncOrchestrator) publishNewMail(ctx context.Context, accountID, folderID, remoteName string, summary newMailSummary) {
	if summary.Count <= 0 {
		return
	}

	folderRole, _ := o.db.GetFolderRole(ctx, folderID)
	payload := map[string]any{
		"count":        summary.Count,
		"unread_count": summary.UnreadCount,
		"folder_name":  displayName(remoteName, folderRole),
	}
	if summary.LatestSet {
		payload["subject"] = summary.Latest.Subject
		payload["from_name"] = summary.Latest.FromName
		payload["from_email"] = summary.Latest.FromEmail
		if summary.Latest.RemoteUID > 0 {
			payload["remote_uid"] = summary.Latest.RemoteUID
		}
	}

	o.events.Publish(Event{
		Type:       EventNewMail,
		AccountID:  accountID,
		FolderID:   folderID,
		FolderRole: folderRole,
		Payload:    payload,
	})
}

func (o *SyncOrchestrator) fullFolderSync(ctx context.Context, client *imap.Client, accountID string, folder storage.FolderSyncInfo, folderIndex, folderTotal int) error {
	totalHint, _ := o.db.GetFolderEmailCount(ctx, folder.ID)
	o.events.Publish(Event{
		Type:       EventSyncStarted,
		AccountID:  accountID,
		FolderID:   folder.ID,
		FolderRole: folder.Role,
		Total:      totalHint,
		Payload: map[string]any{
			"account_folders_total": folderTotal,
			"account_folders_done":  folderIndex - 1,
			"current_folder":        displayName(folder.RemoteID, folder.Role),
		},
	})
	defer func() {
		if ctx.Err() != nil {
			return
		}
		o.events.Publish(Event{
			Type:       EventSyncComplete,
			AccountID:  accountID,
			FolderID:   folder.ID,
			FolderRole: folder.Role,
			Payload: map[string]any{
				"account_folders_total": folderTotal,
				"account_folders_done":  folderIndex,
				"current_folder":        displayName(folder.RemoteID, folder.Role),
			},
		})
	}()
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
		var summary newMailSummary
		result, err := client.SyncFolderIncremental(ctx, folder.ID, folder.RemoteID, highestUID, func(msgs []storage.SyncMessage) error {
			summary.Add(msgs)
			return o.db.UpsertSyncMessages(ctx, msgs)
		})
		if err != nil {
			log.Printf("periodic incremental %s/%s: %v", accountID, folder.RemoteID, err)
		} else if result != nil {
			o.db.UpdateFolderIncrementalSync(ctx, folder.ID, result.HighestUID, result.UIDValidity, int(result.NumMessages))
			if result.TotalFetched > 0 {
				log.Printf("periodic incremental %s/%s: %d new", accountID, folder.RemoteID, result.TotalFetched)
				o.publishNewMail(ctx, accountID, folder.ID, folder.RemoteID, summary)
			}
		}
	} else {
		o.syncFolderMessages(ctx, client, accountID, folder.ID, folder.RemoteID)
	}

	o.refreshFlags(ctx, client, accountID, folder)

	o.db.RefreshFolderUnreadCount(ctx, folder.ID)

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

func (o *SyncOrchestrator) beginAccountSync(ctx context.Context, accountID string, kind accountSyncKind) (context.Context, func(), bool) {
	syncCtx, cancel := context.WithCancel(ctx)
	run := &accountSyncRun{
		kind:   kind,
		cancel: cancel,
		done:   make(chan struct{}),
	}

	o.mu.Lock()
	if o.running[accountID] != nil {
		o.mu.Unlock()
		cancel()
		return nil, nil, false
	}
	o.running[accountID] = run
	o.mu.Unlock()

	finish := func() {
		run.once.Do(func() {
			o.mu.Lock()
			if o.running[accountID] == run {
				delete(o.running, accountID)
			}
			o.mu.Unlock()
			cancel()
			close(run.done)
		})
	}
	return syncCtx, finish, true
}

func (o *SyncOrchestrator) beginManualAccountSync(ctx context.Context, accountID string) (context.Context, func(), bool) {
	for {
		syncCtx, finish, ok := o.beginAccountSync(ctx, accountID, accountSyncManual)
		if ok {
			return syncCtx, finish, true
		}

		done := o.cancelAccountSync(accountID, accountSyncBackground)
		if done == nil {
			select {
			case <-ctx.Done():
				return nil, nil, false
			default:
				continue
			}
		}

		select {
		case <-ctx.Done():
			return nil, nil, false
		case <-done:
		}
	}
}

func (o *SyncOrchestrator) cancelAccountSync(accountID string, cancelKind accountSyncKind) <-chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	run := o.running[accountID]
	if run == nil {
		return nil
	}
	if run.kind == cancelKind {
		run.cancel()
	}
	return run.done
}

func (o *SyncOrchestrator) clearAccountWorker(accountID string, worker *accountWorker) {
	o.mu.Lock()
	if o.cancelFuncs[accountID] == worker {
		delete(o.cancelFuncs, accountID)
	}
	o.mu.Unlock()
}

func (o *SyncOrchestrator) markManualRunning(userID string, run *manualSyncRun) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.manualRuns[userID] != nil {
		return false
	}
	o.manualRuns[userID] = run
	return true
}

func (o *SyncOrchestrator) clearManualRunning(userID string, run *manualSyncRun) {
	o.mu.Lock()
	if o.manualRuns[userID] == run {
		delete(o.manualRuns, userID)
	}
	o.mu.Unlock()
}

func (o *SyncOrchestrator) CancelManualSync(userID string) bool {
	o.mu.Lock()
	run := o.manualRuns[userID]
	o.mu.Unlock()
	if run == nil {
		return false
	}
	run.cancel()
	return true
}

func (o *SyncOrchestrator) SyncAccounts(ctx context.Context, userID string, accountIDs []string) (string, bool) {
	if len(accountIDs) == 0 {
		return "", false
	}

	accountIDs = append([]string(nil), accountIDs...)
	runID := userID + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	run := &manualSyncRun{runID: runID, cancel: cancel}
	if !o.markManualRunning(userID, run) {
		cancel()
		return "", false
	}

	go func() {
		defer cancel()
		defer o.clearManualRunning(userID, run)

		total := len(accountIDs)
		parallelism := manualSyncMaxParallelAccounts
		if parallelism > total {
			parallelism = total
		}
		if parallelism < 1 {
			parallelism = 1
		}

		var progressMu sync.Mutex
		completed := 0
		skipped := 0
		failures := 0
		cancelled := 0

		o.events.Publish(Event{Type: EventManualSyncStarted, Payload: map[string]any{
			"user_id":        userID,
			"run_id":         runID,
			"accounts_total": total,
			"accounts_done":  0,
			"parallelism":    parallelism,
		}})

		jobs := make(chan manualSyncJob)
		var wg sync.WaitGroup
		for worker := 0; worker < parallelism; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for job := range jobs {
					if syncCtx.Err() != nil {
						return
					}

					progressMu.Lock()
					done := completed
					currentFailures := failures
					currentSkipped := skipped
					currentCancelled := cancelled
					progressMu.Unlock()

					o.events.Publish(Event{Type: EventManualSyncProgress, AccountID: job.accountID, Payload: map[string]any{
						"user_id":        userID,
						"run_id":         runID,
						"accounts_total": total,
						"accounts_done":  done,
						"account_index":  job.index + 1,
						"parallelism":    parallelism,
						"status":         "syncing",
						"failures":       currentFailures,
						"skipped":        currentSkipped,
						"cancelled":      currentCancelled,
					}})

					status := "synced"
					errorText := ""
					accountCtx, finish, ok := o.beginManualAccountSync(syncCtx, job.accountID)
					if !ok {
						status = "cancelled"
						errorText = "manual sync could not start"
						if err := syncCtx.Err(); err != nil {
							errorText = err.Error()
						}
					} else {
						err := o.syncAccount(accountCtx, job.accountID)
						finish()
						if err != nil {
							if syncCtx.Err() != nil {
								status = "cancelled"
							} else {
								status = "error"
							}
							errorText = err.Error()
							log.Printf("manual sync account %s: %v", job.accountID, err)
						}
					}

					progressMu.Lock()
					completed++
					if status == "skipped" {
						skipped++
					} else if status == "cancelled" {
						cancelled++
					} else if status == "error" {
						failures++
					}
					done = completed
					currentFailures = failures
					currentSkipped = skipped
					currentCancelled = cancelled
					progressMu.Unlock()

					payload := map[string]any{
						"user_id":        userID,
						"run_id":         runID,
						"accounts_total": total,
						"accounts_done":  done,
						"account_index":  job.index + 1,
						"parallelism":    parallelism,
						"status":         status,
						"failures":       currentFailures,
						"skipped":        currentSkipped,
						"cancelled":      currentCancelled,
					}
					if errorText != "" {
						payload["error"] = errorText
					}
					o.events.Publish(Event{Type: EventManualSyncProgress, AccountID: job.accountID, Payload: payload})
				}
			}()
		}

	queueLoop:
		for i, accountID := range accountIDs {
			select {
			case <-syncCtx.Done():
				break queueLoop
			case jobs <- manualSyncJob{index: i, accountID: accountID}:
			}
		}
		close(jobs)
		wg.Wait()

		progressMu.Lock()
		notDone := total - completed
		finalFailures := failures
		finalCancelled := cancelled
		status := "ok"
		successful := completed - skipped - failures - cancelled
		if successful < 0 {
			successful = 0
		}
		if syncCtx.Err() != nil {
			status = "cancelled"
		} else if (finalFailures > 0 || notDone > 0) && successful == 0 && skipped == 0 {
			status = "error"
		} else if finalFailures > 0 || skipped > 0 || notDone > 0 || finalCancelled > 0 {
			status = "partial"
		}
		finalCompleted := completed
		finalSkipped := skipped
		progressMu.Unlock()

		o.events.Publish(Event{Type: EventManualSyncComplete, Payload: map[string]any{
			"user_id":        userID,
			"run_id":         runID,
			"accounts_total": total,
			"accounts_done":  finalCompleted,
			"failures":       finalFailures,
			"skipped":        finalSkipped,
			"cancelled":      finalCancelled,
			"not_done":       notDone,
			"parallelism":    parallelism,
			"status":         status,
		}})
	}()

	return runID, true
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
	if !o.db.IsEmailSyncEnabled(ctx, accountID) {
		return
	}
	syncCtx, finish, ok := o.beginAccountSync(ctx, accountID, accountSyncBackground)
	if !ok {
		return
	}

	go func() {
		defer finish()

		if err := o.syncAccount(syncCtx, accountID); err != nil {
			log.Printf("sync account %s: %v", accountID, err)
		}
	}()
}

func (o *SyncOrchestrator) syncAccount(ctx context.Context, accountID string) error {
	if !o.db.IsEmailSyncEnabled(ctx, accountID) {
		return nil
	}
	cfg, err := o.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		return err
	}

	password, err := o.resolvePassword(ctx, cfg, accountID)
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
	knownFolders := make(map[string]bool, len(folders))
	for _, f := range folders {
		knownFolders[f.Name] = true
	}

	for i, f := range folders {
		role := f.Role
		order, ok := sortOrder[role]
		if !ok {
			order = 100 + i
		}

		parentID := ""
		if f.Delimiter != 0 && containsDelimiter(f.Name, f.Delimiter) {
			parts := splitDelimiter(f.Name, f.Delimiter)
			if knownFolders[parts[0]] {
				parentID = folderIDFromRemote(accountID, parts[0])
			}
		}

		folderInputs = append(folderInputs, storage.UpsertFolderInput{
			ID:         folderIDFromRemote(accountID, f.Name),
			AccountID:  accountID,
			ParentID:   parentID,
			RemoteID:   f.Name,
			Name:       displayName(f.Name, role),
			Icon:       imap.RoleIcon(role),
			Role:       role,
			Selectable: f.Selectable,
			SortOrder:  order,
		})
	}

	if len(folderInputs) > 0 {
		if err := o.db.UpsertFolders(ctx, folderInputs); err != nil {
			log.Printf("sync folders for %s: %v", accountID, err)
		}
	}

	folderInfos, err := o.db.GetFoldersForAccount(ctx, accountID)
	if err != nil {
		return err
	}
	folderInfoByRemote := make(map[string]storage.FolderSyncInfo, len(folderInfos))
	for _, folder := range folderInfos {
		folderInfoByRemote[folder.RemoteID] = folder
	}

	var syncFolders []imap.FolderInfo
	for _, f := range folders {
		if f.Selectable {
			syncFolders = append(syncFolders, f)
		}
	}

	for i, f := range syncFolders {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		folderDBID := folderIDFromRemote(accountID, f.Name)
		folderInfo, ok := folderInfoByRemote[f.Name]
		if !ok {
			folderInfo = storage.FolderSyncInfo{ID: folderDBID, AccountID: accountID, RemoteID: f.Name, Role: f.Role}
		}
		if err := o.fullFolderSync(ctx, client, accountID, folderInfo, i+1, len(syncFolders)); err != nil {
			log.Printf("sync folder %s/%s: %v", accountID, f.Name, err)
		}
	}

	return nil
}

func (o *SyncOrchestrator) syncFolderMessages(ctx context.Context, client *imap.Client, accountID, folderID, remoteName string) {
	folderRole, _ := o.db.GetFolderRole(ctx, folderID)
	totalHint, _ := o.db.GetFolderEmailCount(ctx, folderID)
	o.events.Publish(Event{Type: EventSyncStarted, AccountID: accountID, FolderID: folderID, FolderRole: folderRole, Total: totalHint})
	fetched := 0
	result, err := client.SyncFolder(ctx, folderID, remoteName, 500, func(msgs []storage.SyncMessage) error {
		fetched += len(msgs)
		o.events.Publish(Event{Type: EventSyncProgress, AccountID: accountID, FolderID: folderID, FolderRole: folderRole, Current: fetched, Total: totalHint})
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
		total := totalHint
		if total <= 0 {
			total = int(result.NumMessages)
		}
		o.events.Publish(Event{Type: EventSyncProgress, AccountID: accountID, FolderID: folderID, FolderRole: folderRole, Current: int(result.TotalFetched), Total: total})
	}
	o.db.RefreshFolderUnreadCount(ctx, folderID)
	log.Printf("synced %s/%s: %d messages", accountID, remoteName, result.TotalFetched)
	o.events.Publish(Event{Type: EventSyncComplete, AccountID: accountID, FolderID: folderID, FolderRole: folderRole})
}

func (o *SyncOrchestrator) syncIncremental(ctx context.Context, accountID, folderID, remoteName string) {
	syncCtx, finish, ok := o.beginAccountSync(ctx, accountID, accountSyncBackground)
	if !ok {
		return
	}
	defer finish()

	highestUID, err := o.db.GetHighestSeenUID(syncCtx, folderID)
	if err != nil {
		log.Printf("incremental %s/%s: get uid: %v", accountID, remoteName, err)
		return
	}

	cfg, err := o.accountStore.GetConfig(syncCtx, accountID)
	if err != nil {
		log.Printf("incremental %s/%s: config: %v", accountID, remoteName, err)
		return
	}

	password, err := o.resolvePassword(syncCtx, cfg, accountID)
	if err != nil {
		log.Printf("incremental %s/%s: password: %v", accountID, remoteName, err)
		return
	}

	client, err := imap.NewClient(syncCtx, cfg, password)
	if err != nil {
		log.Printf("incremental %s/%s: connect: %v", accountID, remoteName, err)
		return
	}
	defer client.Close()

	o.reconcileAndRefresh(syncCtx, client, accountID, folderID, remoteName)

	var summary newMailSummary
	result, err := client.SyncFolderIncremental(syncCtx, folderID, remoteName, highestUID, func(msgs []storage.SyncMessage) error {
		summary.Add(msgs)
		return o.db.UpsertSyncMessages(syncCtx, msgs)
	})
	if err != nil {
		log.Printf("incremental %s/%s: %v", accountID, remoteName, err)
		return
	}

	if result.TotalFetched > 0 {
		log.Printf("incremental %s/%s: %d new messages", accountID, remoteName, result.TotalFetched)
	}

	if result != nil {
		o.db.UpdateFolderIncrementalSync(syncCtx, folderID, result.HighestUID, result.UIDValidity, int(result.NumMessages))
	}

	unread, _ := o.db.RefreshFolderUnreadCount(syncCtx, folderID)

	o.publishNewMail(syncCtx, accountID, folderID, remoteName, summary)
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
