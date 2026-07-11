package mail

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

const (
	manualSyncMaxParallelAccounts     = 0
	backgroundSyncMaxParallelAccounts = 0
	manualSyncTimeout                 = 30 * time.Minute
	manualRepairSyncTimeout           = 6 * time.Hour
)

type manualSyncJob struct {
	index     int
	accountID string
}

type manualSyncOperation func(context.Context, string) error

type folderMessageSyncer interface {
	SyncFolder(context.Context, string, string, int, func([]storage.SyncMessage) error) (*imap.SyncResult, error)
}

type manualSyncRun struct {
	runID      string
	mode       string
	accountIDs []string
	cancel     context.CancelFunc
}

type accountSyncKind string

const (
	accountSyncBackground accountSyncKind = "background"
	accountSyncManual     accountSyncKind = "manual"
)

type accountSyncProgressScope struct {
	kind          string
	mode          string
	userID        string
	runID         string
	accountIDs    []string
	accountsTotal int
	accountIndex  int
	parallelism   int
}

type accountSyncProgressScopeKey struct{}

type accountSyncRun struct {
	kind   accountSyncKind
	mode   string
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

type accountWorker struct {
	cancel context.CancelFunc
	ctx    context.Context
	done   chan struct{}
	ready  bool
}

type idleWatcher interface {
	Run(context.Context)
	Close()
}

type idleWatcherGroup struct {
	cancel    context.CancelFunc
	done      chan struct{}
	watchers  []idleWatcher
	folderIDs map[string]bool
}

type dirtyIMAPFolder struct {
	ctx        context.Context
	folderID   string
	remoteName string
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

type refreshingTokenProvider interface {
	RefreshOAuthTokenForAccount(ctx context.Context, accountID string) (string, error)
}

type graphMailTokenProvider interface {
	GetMicrosoftGraphMailTokenForAccount(ctx context.Context, accountID string) (string, error)
}

type SyncOrchestrator struct {
	db                  *storage.DB
	accountStore        *config.AccountStore
	blobStore           *store.BlobStore
	tokenProvider       TokenProvider
	events              *EventBus
	mu                  sync.Mutex
	running             map[string]*accountSyncRun
	manualRuns          map[string]map[string]*manualSyncRun
	backgroundSyncSlots chan struct{}
	idleWatchers        map[string]*idleWatcherGroup
	idleWatcherFactory  func(*models.AccountConfig, string, string, func()) idleWatcher
	idleMu              sync.Mutex
	dirtyIMAPFolders    map[string]map[string]dirtyIMAPFolder
	dirtyIMAPDraining   map[string]bool
	dirtyIMAPDrainDone  map[string]chan struct{}
	cancelFuncs         map[string]*accountWorker
	lifecycleCtx        context.Context
	syncAccountOverride func(context.Context, string, bool) error
	incrementalOverride func(context.Context, string, string, string)
	gmailPollMu         sync.Mutex
	activeUsers         map[string]int
	gmailPollRuntime    map[string]gmailPollRuntimeState
	interval            int
	intervalMu          sync.RWMutex
	intervalChanged     chan struct{}
}

func newAccountSyncSlots(limit int) chan struct{} {
	if limit <= 0 {
		return nil
	}
	return make(chan struct{}, limit)
}

func accountSyncParallelism(total, limit int) int {
	if total <= 0 {
		return 0
	}
	if limit <= 0 || limit > total {
		return total
	}
	return limit
}

func withAccountSyncProgressScope(ctx context.Context, scope accountSyncProgressScope) context.Context {
	return context.WithValue(ctx, accountSyncProgressScopeKey{}, scope)
}

func accountSyncProgressPayload(ctx context.Context, fallbackKind accountSyncKind, payload map[string]any) map[string]any {
	if payload == nil {
		payload = make(map[string]any)
	}
	if fallbackKind != "" {
		payload["kind"] = string(fallbackKind)
	}
	scope, ok := ctx.Value(accountSyncProgressScopeKey{}).(accountSyncProgressScope)
	if !ok {
		return payload
	}
	if scope.kind != "" {
		payload["kind"] = scope.kind
	}
	if scope.mode != "" {
		payload["mode"] = scope.mode
	}
	if scope.userID != "" {
		payload["user_id"] = scope.userID
	}
	if scope.runID != "" {
		payload["run_id"] = scope.runID
	}
	if len(scope.accountIDs) > 0 {
		payload["account_ids"] = append([]string(nil), scope.accountIDs...)
	}
	if scope.accountsTotal > 0 {
		payload["accounts_total"] = scope.accountsTotal
	}
	if scope.accountIndex > 0 {
		payload["account_index"] = scope.accountIndex
	}
	if scope.parallelism > 0 {
		payload["parallelism"] = scope.parallelism
	}
	return payload
}

func (o *SyncOrchestrator) accountSyncStatusPayload(ctx context.Context, accountID string, fallbackKind accountSyncKind, payload map[string]any) map[string]any {
	payload = accountSyncProgressPayload(ctx, fallbackKind, payload)
	if accountID == "" || o.accountStore == nil {
		return payload
	}
	account, err := o.accountStore.GetAccountByID(context.Background(), accountID)
	if err != nil || account == nil {
		return payload
	}
	if name := strings.TrimSpace(account.Name); name != "" {
		payload["account_name"] = name
	}
	if email := strings.TrimSpace(account.Email); email != "" {
		payload["account_email"] = email
	}
	return payload
}

func (o *SyncOrchestrator) folderSyncProgressPayload(ctx context.Context, accountID, folderName, provider string, payload map[string]any) map[string]any {
	if payload == nil {
		payload = make(map[string]any)
	}
	if folderName = strings.TrimSpace(folderName); folderName != "" {
		payload["current_folder"] = folderName
		payload["folder_name"] = folderName
	}
	if provider = strings.TrimSpace(provider); provider != "" {
		payload["provider"] = provider
	}
	return o.accountSyncStatusPayload(ctx, accountID, "", payload)
}

func (o *SyncOrchestrator) refreshOAuthTokenForAccount(ctx context.Context, accountID string) (string, error) {
	if o == nil || o.tokenProvider == nil {
		return "", errProviderLabelAuth
	}
	if refresher, ok := o.tokenProvider.(refreshingTokenProvider); ok {
		return refresher.RefreshOAuthTokenForAccount(ctx, accountID)
	}
	return "", errProviderLabelAuth
}

func NewSyncOrchestrator(db *storage.DB, accountStore *config.AccountStore, blobStore *store.BlobStore, tokenProvider TokenProvider) *SyncOrchestrator {
	return &SyncOrchestrator{
		db:                  db,
		accountStore:        accountStore,
		blobStore:           blobStore,
		tokenProvider:       tokenProvider,
		events:              NewEventBus(),
		running:             make(map[string]*accountSyncRun),
		manualRuns:          make(map[string]map[string]*manualSyncRun),
		backgroundSyncSlots: newAccountSyncSlots(backgroundSyncMaxParallelAccounts),
		idleWatchers:        make(map[string]*idleWatcherGroup),
		dirtyIMAPFolders:    make(map[string]map[string]dirtyIMAPFolder),
		dirtyIMAPDraining:   make(map[string]bool),
		dirtyIMAPDrainDone:  make(map[string]chan struct{}),
		idleWatcherFactory: func(cfg *models.AccountConfig, password, remoteName string, onNotify func()) idleWatcher {
			return imap.NewIdleWatcher(cfg, password, remoteName, onNotify)
		},
		cancelFuncs:      make(map[string]*accountWorker),
		activeUsers:      make(map[string]int),
		gmailPollRuntime: make(map[string]gmailPollRuntimeState),
		interval:         5,
		intervalChanged:  make(chan struct{}, 1),
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
	select {
	case o.intervalChanged <- struct{}{}:
	default:
	}
}

func (o *SyncOrchestrator) StopAccount(accountID string) {
	o.stopAccount(accountID)
}

func (o *SyncOrchestrator) stopAccount(accountID string) (<-chan struct{}, <-chan struct{}, <-chan struct{}) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, nil, nil
	}
	o.mu.Lock()
	worker := o.cancelFuncs[accountID]
	if worker != nil {
		delete(o.cancelFuncs, accountID)
	}
	run := o.running[accountID]
	o.mu.Unlock()

	if worker != nil {
		worker.cancel()
	}
	if run != nil {
		run.cancel()
	}
	o.stopIDLEWatchers(accountID)
	drainDone := o.clearDirtyIMAPFolders(accountID)
	var workerDone, runDone <-chan struct{}
	if worker != nil {
		workerDone = worker.done
	}
	if run != nil {
		runDone = run.done
	}
	return workerDone, runDone, drainDone
}

func (o *SyncOrchestrator) IsAccountSyncRunning(accountID string) bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.running[strings.TrimSpace(accountID)] != nil
}

func (o *SyncOrchestrator) StartAccount(ctx context.Context, accountID string) {
	ctx = o.accountLifecycleContext(ctx)
	if !o.db.IsEmailSyncEnabled(ctx, accountID) {
		return
	}
	o.startAccount(ctx, accountID)
}

func (o *SyncOrchestrator) RestartAccount(accountID string) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return
	}
	workerDone, runDone, drainDone := o.stopAccount(accountID)
	ctx := o.accountLifecycleContext(context.Background())
	go func() {
		for _, done := range []<-chan struct{}{workerDone, runDone, drainDone} {
			if done == nil {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-done:
			}
		}
		o.StartAccount(ctx, accountID)
	}()
}

func (o *SyncOrchestrator) RestartIDLEWatchers(accountIDs []string) {
	seen := make(map[string]bool, len(accountIDs))
	for _, accountID := range accountIDs {
		accountID = strings.TrimSpace(accountID)
		if accountID == "" || seen[accountID] {
			continue
		}
		seen[accountID] = true

		o.mu.Lock()
		worker := o.cancelFuncs[accountID]
		ready := worker != nil && worker.ready
		o.mu.Unlock()
		if worker == nil {
			o.StartAccount(o.accountLifecycleContext(context.Background()), accountID)
			continue
		}
		if !ready {
			continue
		}
		o.startIDLEWatchers(worker.ctx, accountID)
	}
}

func (o *SyncOrchestrator) stopIDLEWatchers(accountID string) {
	o.idleMu.Lock()
	defer o.idleMu.Unlock()
	o.stopIDLEWatchersLocked(accountID)
}

func (o *SyncOrchestrator) stopIDLEWatchersLocked(accountID string) {
	o.mu.Lock()
	group := o.idleWatchers[accountID]
	delete(o.idleWatchers, accountID)
	o.mu.Unlock()
	if group == nil {
		return
	}
	group.cancel()
	for _, watcher := range group.watchers {
		watcher.Close()
	}
	<-group.done
}

func (o *SyncOrchestrator) activeIdleFolderIDs(accountID string) map[string]bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	group := o.idleWatchers[accountID]
	if group == nil {
		return map[string]bool{}
	}
	result := make(map[string]bool, len(group.folderIDs))
	for folderID := range group.folderIDs {
		result[folderID] = true
	}
	return result
}

func (o *SyncOrchestrator) markIMAPFolderDirty(ctx context.Context, accountID, folderID, remoteName string) {
	accountID = strings.TrimSpace(accountID)
	folderID = strings.TrimSpace(folderID)
	remoteName = strings.TrimSpace(remoteName)
	if ctx == nil {
		ctx = context.Background()
	}
	if accountID == "" || folderID == "" || remoteName == "" || ctx.Err() != nil {
		return
	}

	o.mu.Lock()
	if o.dirtyIMAPFolders == nil {
		o.dirtyIMAPFolders = make(map[string]map[string]dirtyIMAPFolder)
	}
	if o.dirtyIMAPDraining == nil {
		o.dirtyIMAPDraining = make(map[string]bool)
	}
	if o.dirtyIMAPDrainDone == nil {
		o.dirtyIMAPDrainDone = make(map[string]chan struct{})
	}
	folders := o.dirtyIMAPFolders[accountID]
	if folders == nil {
		folders = make(map[string]dirtyIMAPFolder)
		o.dirtyIMAPFolders[accountID] = folders
	}
	folders[folderID] = dirtyIMAPFolder{
		ctx:        ctx,
		folderID:   folderID,
		remoteName: remoteName,
	}
	if o.dirtyIMAPDraining[accountID] {
		o.mu.Unlock()
		return
	}
	o.dirtyIMAPDraining[accountID] = true
	done := make(chan struct{})
	o.dirtyIMAPDrainDone[accountID] = done
	o.mu.Unlock()

	go o.drainDirtyIMAPFolders(accountID, done)
}

func (o *SyncOrchestrator) clearDirtyIMAPFolders(accountID string) <-chan struct{} {
	o.mu.Lock()
	accountID = strings.TrimSpace(accountID)
	delete(o.dirtyIMAPFolders, accountID)
	done := o.dirtyIMAPDrainDone[accountID]
	o.mu.Unlock()
	return done
}

func (o *SyncOrchestrator) drainDirtyIMAPFolders(accountID string, done chan struct{}) {
	for {
		o.mu.Lock()
		folders := o.dirtyIMAPFolders[accountID]
		folderIDs := make([]string, 0, len(folders))
		for folderID, folder := range folders {
			if folder.ctx.Err() != nil {
				delete(folders, folderID)
				continue
			}
			folderIDs = append(folderIDs, folderID)
		}
		if len(folderIDs) == 0 {
			delete(o.dirtyIMAPFolders, accountID)
			if o.dirtyIMAPDrainDone[accountID] == done {
				delete(o.dirtyIMAPDraining, accountID)
				delete(o.dirtyIMAPDrainDone, accountID)
			}
			o.mu.Unlock()
			close(done)
			return
		}
		if run := o.running[accountID]; run != nil {
			done := run.done
			o.mu.Unlock()
			<-done
			continue
		}
		sort.Strings(folderIDs)
		folder := folders[folderIDs[0]]
		delete(folders, folder.folderID)
		o.mu.Unlock()

		if folder.ctx.Err() != nil {
			continue
		}
		if !o.trySyncIncremental(folder.ctx, accountID, folder.folderID, folder.remoteName) && folder.ctx.Err() == nil {
			o.mu.Lock()
			folders := o.dirtyIMAPFolders[accountID]
			if folders == nil {
				folders = make(map[string]dirtyIMAPFolder)
				o.dirtyIMAPFolders[accountID] = folders
			}
			if _, alreadyDirty := folders[folder.folderID]; !alreadyDirty {
				folders[folder.folderID] = folder
			}
			o.mu.Unlock()
		}
	}
}

func (o *SyncOrchestrator) startIDLEWatchers(ctx context.Context, accountID string) {
	o.idleMu.Lock()
	defer o.idleMu.Unlock()
	o.stopIDLEWatchersLocked(accountID)

	if !o.db.IsEmailSyncEnabled(ctx, accountID) {
		return
	}
	cfg, err := o.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		log.Printf("idle %s: config: %v", accountID, err)
		o.markAccountSyncError(ctx, accountID, err)
		return
	}
	if o.shouldUseOutlookGraphMail(cfg) {
		log.Printf("idle %s: skipping IMAP IDLE for Graph-first Outlook sync", accountID)
		return
	}
	if o.shouldUseGmailAPIMail(cfg) {
		log.Printf("idle %s: skipping IMAP IDLE for Gmail API-first sync", accountID)
		return
	}

	password, err := o.resolvePassword(ctx, cfg, accountID)
	if err != nil {
		log.Printf("idle %s: password: %v", accountID, err)
		o.markAccountSyncError(ctx, accountID, err)
		return
	}

	idleFolderIDs := o.getIdleFolderIDsForAccount(ctx, accountID)
	folders, err := o.db.GetFoldersForAccount(ctx, accountID)
	if err != nil {
		log.Printf("idle %s: folders: %v", accountID, err)
		o.markAccountSyncError(ctx, accountID, err)
		return
	}
	watcherCtx, cancel := context.WithCancel(ctx)
	var watchers []idleWatcher
	folderIDs := make(map[string]bool)
	factory := o.idleWatcherFactory
	if factory == nil {
		factory = func(cfg *models.AccountConfig, password, remoteName string, onNotify func()) idleWatcher {
			return imap.NewIdleWatcher(cfg, password, remoteName, onNotify)
		}
	}

	for _, folder := range folders {
		if !idleFolderIDs[folder.ID] || strings.TrimSpace(folder.RemoteID) == "" {
			continue
		}

		folderID := folder.ID
		remoteName := folder.RemoteID
		watcher := factory(cfg, password, remoteName, func() {
			o.markIMAPFolderDirty(watcherCtx, accountID, folderID, remoteName)
		})
		watchers = append(watchers, watcher)
		folderIDs[folderID] = true
	}

	if len(watchers) == 0 {
		cancel()
		return
	}
	group := &idleWatcherGroup{
		cancel:    cancel,
		done:      make(chan struct{}),
		watchers:  watchers,
		folderIDs: folderIDs,
	}
	o.mu.Lock()
	o.idleWatchers[accountID] = group
	o.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(len(watchers))
	for _, watcher := range watchers {
		go func(w idleWatcher) {
			defer wg.Done()
			w.Run(watcherCtx)
		}(watcher)
	}
	go func() {
		wg.Wait()
		close(group.done)
	}()
}

func (o *SyncOrchestrator) getInterval() int {
	o.intervalMu.RLock()
	defer o.intervalMu.RUnlock()
	return o.interval
}

func (o *SyncOrchestrator) accountLifecycleContext(fallback context.Context) context.Context {
	if fallback == nil {
		fallback = context.Background()
	}
	o.mu.Lock()
	ctx := o.lifecycleCtx
	o.mu.Unlock()
	if ctx != nil {
		return ctx
	}
	return fallback
}

func (o *SyncOrchestrator) resolvePassword(ctx context.Context, cfg *models.AccountConfig, accountID string) (string, error) {
	if strings.TrimSpace(cfg.Provider) == providers.ProviderOutlook {
		return "", fmt.Errorf("outlook mail uses Microsoft Graph; IMAP credential resolution is disabled")
	}
	if cfg.AuthMethod == "oauth2" && o.tokenProvider != nil {
		return o.tokenProvider.GetOAuthTokenForAccount(ctx, accountID)
	}
	return o.accountStore.DecryptPassword(ctx, accountID)
}

func (o *SyncOrchestrator) markAccountSyncError(ctx context.Context, accountID string, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return
	}
	failedAt := time.Now().UTC()
	if storeErr := o.db.MarkEmailSyncError(context.Background(), accountID, message, failedAt); storeErr != nil {
		log.Printf("sync %s: store account error: %v", accountID, storeErr)
	}
	o.events.Publish(Event{Type: EventAccountSyncStatus, AccountID: accountID, Payload: o.accountSyncStatusPayload(ctx, accountID, "", map[string]any{
		"status":    "error",
		"error":     message,
		"failed_at": failedAt.Format(time.RFC3339),
	})})
}

func (o *SyncOrchestrator) clearAccountSyncError(ctx context.Context, accountID string) {
	if ctx.Err() != nil {
		return
	}
	if err := o.db.ClearEmailSyncError(context.Background(), accountID); err != nil {
		log.Printf("sync %s: clear account error: %v", accountID, err)
	}
	o.events.Publish(Event{Type: EventAccountSyncStatus, AccountID: accountID, Payload: o.accountSyncStatusPayload(ctx, accountID, "", map[string]any{
		"status": "ok",
	})})
}

func (o *SyncOrchestrator) getIdleFolderIDsForAccount(ctx context.Context, accountID string) map[string]bool {
	userID, err := o.db.GetAccountUserID(ctx, accountID)
	if err != nil || userID == "" {
		return map[string]bool{}
	}
	return o.db.GetIdleFolderIDsForAccount(ctx, userID, accountID)
}

func pollingFoldersForPeriodicSync(folders []storage.FolderSyncInfo, idleFolderIDs map[string]bool) ([]storage.FolderSyncInfo, int) {
	if len(folders) == 0 || len(idleFolderIDs) == 0 {
		return folders, 0
	}

	polling := make([]storage.FolderSyncInfo, 0, len(folders))
	excluded := 0
	for _, folder := range folders {
		if idleFolderIDs[folder.ID] {
			excluded++
			continue
		}
		polling = append(polling, folder)
	}
	return polling, excluded
}

func pollingIMAPFoldersForAutomaticSync(folders []imap.FolderInfo, idleRemoteNames map[string]bool) ([]imap.FolderInfo, int) {
	if len(folders) == 0 || len(idleRemoteNames) == 0 {
		return folders, 0
	}

	polling := make([]imap.FolderInfo, 0, len(folders))
	excluded := 0
	for _, folder := range folders {
		if idleRemoteNames[folder.Name] {
			excluded++
			continue
		}
		polling = append(polling, folder)
	}
	return polling, excluded
}

func idleRemoteNamesFromFolders(folders []storage.FolderSyncInfo, idleFolderIDs map[string]bool) map[string]bool {
	if len(folders) == 0 || len(idleFolderIDs) == 0 {
		return map[string]bool{}
	}
	remoteNames := make(map[string]bool, len(idleFolderIDs))
	for _, folder := range folders {
		if idleFolderIDs[folder.ID] && folder.RemoteID != "" {
			remoteNames[folder.RemoteID] = true
		}
	}
	return remoteNames
}

func (o *SyncOrchestrator) publishAutomaticSyncScope(ctx context.Context, accountID string, idleFolderIDs map[string]bool) {
	allFolders, err := o.db.GetFoldersForAccount(ctx, accountID)
	if err != nil || len(allFolders) == 0 {
		return
	}
	folders, idleExcluded := pollingFoldersForPeriodicSync(allFolders, idleFolderIDs)
	payload := accountSyncProgressPayload(ctx, accountSyncBackground, map[string]any{
		"status":                "syncing",
		"account_folders_total": len(folders),
	})
	if idleExcluded > 0 {
		payload["idle_folders_excluded"] = idleExcluded
	}
	o.events.Publish(Event{Type: EventAccountSyncStatus, AccountID: accountID, Payload: payload})
}

func (o *SyncOrchestrator) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	o.mu.Lock()
	o.lifecycleCtx = ctx
	o.mu.Unlock()
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
		o.StartAccount(ctx, accountID)
	}
	if o.tokenProvider != nil && gmailAPIPollEnabled() {
		go o.runGmailAPIPoller(ctx)
		log.Printf("sync: Gmail API active poll worker started")
	}
	go o.runScheduledSync(ctx)
	log.Printf("sync: scheduled sync worker started")
	log.Printf("sync: startup scan complete")
}

func (o *SyncOrchestrator) startAccount(ctx context.Context, accountID string) {
	if !o.db.IsEmailSyncEnabled(ctx, accountID) {
		log.Printf("sync: account %s email sync disabled", accountID)
		return
	}
	accountCtx, cancel := context.WithCancel(ctx)
	worker := &accountWorker{cancel: cancel, ctx: accountCtx, done: make(chan struct{})}
	o.mu.Lock()
	if _, running := o.cancelFuncs[accountID]; running {
		o.mu.Unlock()
		cancel()
		return
	}
	o.cancelFuncs[accountID] = worker
	o.mu.Unlock()
	go o.runAccountWorker(accountCtx, accountID, worker)
}

func (o *SyncOrchestrator) runAccountWorker(ctx context.Context, accountID string, worker *accountWorker) {
	defer close(worker.done)
	defer o.clearAccountWorker(accountID, worker)
	if ctx.Err() != nil {
		return
	}

	o.runInitialAccountSync(ctx, accountID)
	if ctx.Err() != nil {
		return
	}
	o.startIDLEWatchers(ctx, accountID)
	o.mu.Lock()
	if o.cancelFuncs[accountID] == worker {
		worker.ready = true
	}
	o.mu.Unlock()
	log.Printf("sync: account %s IDLE watchers started after baseline sync", accountID)

	<-ctx.Done()
	o.stopIDLEWatchers(accountID)
}

func (o *SyncOrchestrator) runInitialAccountSync(ctx context.Context, accountID string) {
	log.Printf("sync: account %s initial sync started", accountID)
	syncCtx, finish, ok := o.beginAccountSync(ctx, accountID, accountSyncBackground)
	if !ok {
		log.Printf("sync: account %s initial sync skipped, account already syncing", accountID)
		return
	}
	defer finish()

	includeIDLEFolders := false
	if cfg, err := o.accountStore.GetConfig(syncCtx, accountID); err == nil {
		includeIDLEFolders = !o.shouldUseOutlookGraphMail(cfg) && !o.shouldUseGmailAPIMail(cfg)
	}
	syncAccount := o.syncAccount
	if o.syncAccountOverride != nil {
		syncAccount = o.syncAccountOverride
	}
	if err := syncAccount(syncCtx, accountID, includeIDLEFolders); err != nil {
		log.Printf("sync account %s: %v", accountID, err)
		o.markAccountSyncError(syncCtx, accountID, err)
	} else {
		o.clearAccountSyncError(syncCtx, accountID)
		log.Printf("sync: account %s initial sync finished", accountID)
	}
}

func (o *SyncOrchestrator) runScheduledSync(ctx context.Context) {
	for {
		intervalMinutes := o.getInterval()
		interval := time.Duration(intervalMinutes) * time.Minute
		if interval < time.Minute {
			interval = time.Minute
		}
		timer := time.NewTimer(interval)

		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-o.intervalChanged:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			log.Printf("scheduled sync: interval updated to %d minute(s), rescheduled", o.getInterval())
			continue
		case <-timer.C:
			o.scheduledSync(ctx)
		}
	}
}

func (o *SyncOrchestrator) scheduledSync(ctx context.Context) {
	accountIDs, err := o.db.GetAllEmailSyncAccountIDs(ctx)
	if err != nil {
		log.Printf("scheduled sync: get accounts: %v", err)
		return
	}
	if len(accountIDs) == 0 {
		return
	}

	accountsByUser := make(map[string][]string)
	for _, accountID := range accountIDs {
		userID, err := o.db.GetAccountUserID(ctx, accountID)
		if err != nil || userID == "" {
			log.Printf("scheduled sync %s: get user: %v", accountID, err)
			continue
		}
		accountsByUser[userID] = append(accountsByUser[userID], accountID)
	}

	for userID, ids := range accountsByUser {
		o.runScheduledSyncForUser(ctx, userID, ids)
	}
}

func (o *SyncOrchestrator) runScheduledSyncForUser(ctx context.Context, userID string, accountIDs []string) {
	if len(accountIDs) == 0 {
		return
	}
	runID := userID + "-scheduled-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	runAccountIDs := append([]string(nil), accountIDs...)
	total := len(accountIDs)
	parallelism := accountSyncParallelism(total, backgroundSyncMaxParallelAccounts)
	log.Printf("scheduled sync: run %s started with %d account(s)", runID, total)

	var progressMu sync.Mutex
	completed := 0
	skipped := 0
	failures := 0
	cancelled := 0

	o.events.Publish(Event{Type: EventScheduledSyncStarted, Payload: map[string]any{
		"user_id":        userID,
		"run_id":         runID,
		"accounts_total": total,
		"accounts_done":  0,
		"account_ids":    append([]string(nil), runAccountIDs...),
		"parallelism":    parallelism,
		"kind":           "scheduled",
	}})

	jobs := make(chan manualSyncJob)
	var wg sync.WaitGroup
	for worker := 0; worker < parallelism; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if ctx.Err() != nil {
					return
				}

				progressMu.Lock()
				done := completed
				currentFailures := failures
				currentSkipped := skipped
				currentCancelled := cancelled
				progressMu.Unlock()

				o.events.Publish(Event{Type: EventScheduledSyncProgress, AccountID: job.accountID, Payload: map[string]any{
					"user_id":        userID,
					"run_id":         runID,
					"account_ids":    append([]string(nil), runAccountIDs...),
					"accounts_total": total,
					"accounts_done":  done,
					"account_index":  job.index + 1,
					"parallelism":    parallelism,
					"status":         "syncing",
					"failures":       currentFailures,
					"skipped":        currentSkipped,
					"cancelled":      currentCancelled,
					"kind":           "scheduled",
				}})

				accountCtx := withAccountSyncProgressScope(ctx, accountSyncProgressScope{
					kind:          "scheduled",
					userID:        userID,
					runID:         runID,
					accountIDs:    runAccountIDs,
					accountsTotal: total,
					accountIndex:  job.index + 1,
					parallelism:   parallelism,
				})

				status := "synced"
				errorText := ""
				accountCtx, finish, ok := o.beginAccountSync(accountCtx, job.accountID, accountSyncBackground)
				if !ok {
					status = "skipped"
					errorText = "account is already syncing"
					log.Printf("scheduled sync: account %s skipped, account already syncing", job.accountID)
				} else {
					err := o.syncAccount(accountCtx, job.accountID, false)
					if err != nil {
						if ctx.Err() != nil {
							status = "cancelled"
						} else {
							status = "error"
						}
						errorText = err.Error()
						log.Printf("scheduled sync account %s: %v", job.accountID, err)
						o.markAccountSyncError(accountCtx, job.accountID, err)
					} else {
						o.clearAccountSyncError(accountCtx, job.accountID)
					}
					finish()
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
					"account_ids":    append([]string(nil), runAccountIDs...),
					"accounts_total": total,
					"accounts_done":  done,
					"account_index":  job.index + 1,
					"parallelism":    parallelism,
					"status":         status,
					"failures":       currentFailures,
					"skipped":        currentSkipped,
					"cancelled":      currentCancelled,
					"kind":           "scheduled",
				}
				if errorText != "" {
					payload["error"] = errorText
				}
				o.events.Publish(Event{Type: EventScheduledSyncProgress, AccountID: job.accountID, Payload: payload})
			}
		}()
	}

queueLoop:
	for i, accountID := range accountIDs {
		select {
		case <-ctx.Done():
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
	if ctx.Err() != nil {
		status = "cancelled"
	} else if (finalFailures > 0 || notDone > 0) && successful == 0 && skipped == 0 {
		status = "error"
	} else if finalFailures > 0 || skipped > 0 || notDone > 0 || finalCancelled > 0 {
		status = "partial"
	}
	finalCompleted := completed
	finalSkipped := skipped
	progressMu.Unlock()

	o.events.Publish(Event{Type: EventScheduledSyncComplete, Payload: map[string]any{
		"user_id":        userID,
		"run_id":         runID,
		"account_ids":    append([]string(nil), runAccountIDs...),
		"accounts_total": total,
		"accounts_done":  finalCompleted,
		"failures":       finalFailures,
		"skipped":        finalSkipped,
		"cancelled":      finalCancelled,
		"not_done":       notDone,
		"parallelism":    parallelism,
		"status":         status,
		"kind":           "scheduled",
	}})
	log.Printf("scheduled sync: run %s finished with status %s", runID, status)
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
		if avatarURL := o.senderAvatarURL(ctx, summary.Latest.FromEmail); avatarURL != "" {
			payload["avatar_url"] = avatarURL
		}
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

func (o *SyncOrchestrator) recordFolderSyncError(ctx context.Context, folderID string, syncErr error) {
	if syncErr == nil || ctx.Err() != nil {
		return
	}
	if _, err := o.db.Write().ExecContext(ctx,
		`UPDATE folders SET sync_error = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, syncErr.Error(), folderID); err != nil {
		log.Printf("sync folder %s: store error: %v", folderID, err)
	}
}

func (o *SyncOrchestrator) resetFolderUIDStateAndSync(ctx context.Context, client folderMessageSyncer, accountID, accountProvider string, folder storage.FolderSyncInfo, oldUIDValidity, newUIDValidity uint32) (syncErr error) {
	defer func() {
		if syncErr != nil {
			o.recordFolderSyncError(ctx, folder.ID, syncErr)
		}
	}()

	log.Printf("UIDVALIDITY changed for %s/%s: %d -> %d, rebuilding local folder state", accountID, folder.RemoteID, oldUIDValidity, newUIDValidity)
	if err := o.db.ResetFolderUIDState(ctx, folder.ID, newUIDValidity); err != nil {
		return err
	}
	return o.syncFolderMessages(ctx, client, accountID, accountProvider, folder.ID, folder.RemoteID)
}

func folderUIDStateNeedsReset(expectedUIDValidity, currentUIDValidity, highestUID uint32) bool {
	if currentUIDValidity == 0 {
		return false
	}
	if expectedUIDValidity > 0 {
		return expectedUIDValidity != currentUIDValidity
	}
	return highestUID > 0
}

func (o *SyncOrchestrator) senderAvatarURL(ctx context.Context, email string) string {
	if o == nil || o.db == nil {
		return ""
	}
	hash := avatarresolver.GravatarHash(email)
	if hash == "" {
		return ""
	}
	rec, err := o.db.GetSenderAvatarByHash(ctx, hash)
	if err != nil || rec == nil || rec.Status != "found" {
		return ""
	}
	if rec.ExpiresAtValid && time.Now().After(rec.ExpiresAt) {
		return ""
	}
	if rec.StoragePath == "" && len(rec.ImageData) == 0 {
		return ""
	}
	return storage.SenderAvatarURL(rec.EmailHash, rec.ExpiresAt)
}

func (o *SyncOrchestrator) fullFolderSync(ctx context.Context, client *imap.Client, accountID, accountProvider string, folder storage.FolderSyncInfo, folderIndex, folderTotal, idleExcluded int) (syncErr error) {
	totalHint, _ := o.db.GetFolderEmailCount(ctx, folder.ID)
	folderName := displayName(folder.RemoteID, folder.Role)
	startPayload := map[string]any{
		"account_folders_total": folderTotal,
		"account_folders_done":  folderIndex - 1,
	}
	if idleExcluded > 0 {
		startPayload["idle_folders_excluded"] = idleExcluded
	}
	startPayload = o.folderSyncProgressPayload(ctx, accountID, folderName, accountProvider, startPayload)
	o.events.Publish(Event{
		Type:       EventSyncStarted,
		AccountID:  accountID,
		FolderID:   folder.ID,
		FolderRole: folder.Role,
		Total:      totalHint,
		Payload:    startPayload,
	})
	defer func() {
		if syncErr != nil {
			o.recordFolderSyncError(ctx, folder.ID, syncErr)
			return
		}
		if ctx.Err() != nil {
			return
		}
		completePayload := map[string]any{
			"account_folders_total": folderTotal,
			"account_folders_done":  folderIndex,
		}
		if idleExcluded > 0 {
			completePayload["idle_folders_excluded"] = idleExcluded
		}
		completePayload = o.folderSyncProgressPayload(ctx, accountID, folderName, accountProvider, completePayload)
		o.events.Publish(Event{
			Type:       EventSyncComplete,
			AccountID:  accountID,
			FolderID:   folder.ID,
			FolderRole: folder.Role,
			Payload:    completePayload,
		})
	}()
	storedValidity, err := o.db.GetStoredUIDValidity(ctx, folder.ID)
	if err != nil {
		return err
	}
	highestUID, err := o.db.GetHighestSeenUID(ctx, folder.ID)
	if err != nil {
		return err
	}
	expectedUIDValidity := storedValidity

	if folder.LastFullSyncAt.Valid {
		currentValidity, changed, err := o.reconcileFolder(ctx, client, accountID, folder, expectedUIDValidity, highestUID)
		if err != nil {
			return err
		}
		if changed {
			return o.resetFolderUIDStateAndSync(ctx, client, accountID, accountProvider, folder, storedValidity, currentValidity)
		}
		if currentValidity > 0 {
			expectedUIDValidity = currentValidity
		}
	}

	if highestUID > 0 {
		var summary newMailSummary
		result, err := client.SyncFolderIncremental(ctx, folder.ID, folder.RemoteID, highestUID, expectedUIDValidity, func(msgs []storage.SyncMessage) error {
			summary.Add(msgs)
			return o.db.UpsertSyncMessages(ctx, withFolderLabels(msgs, accountProvider, folder.RemoteID, folder.Role))
		})
		if err != nil {
			return fmt.Errorf("incremental %s/%s: %w", accountID, folder.RemoteID, err)
		}
		if result == nil {
			return fmt.Errorf("incremental %s/%s returned no result", accountID, folder.RemoteID)
		}
		if result.UIDValidityChanged {
			return o.resetFolderUIDStateAndSync(ctx, client, accountID, accountProvider, folder, expectedUIDValidity, result.UIDValidity)
		}
		expectedUIDValidity = result.UIDValidity
		if err := o.db.UpdateFolderIncrementalSync(ctx, folder.ID, result.HighestUID, result.UIDValidity, int(result.NumMessages)); err != nil {
			return fmt.Errorf("save incremental state %s/%s: %w", accountID, folder.RemoteID, err)
		}
		if result.TotalFetched > 0 {
			log.Printf("periodic incremental %s/%s: %d new", accountID, folder.RemoteID, result.TotalFetched)
			o.publishNewMail(ctx, accountID, folder.ID, folder.RemoteID, summary)
		}
	} else {
		return o.syncFolderMessages(ctx, client, accountID, accountProvider, folder.ID, folder.RemoteID)
	}

	currentValidity, changed, err := o.refreshFlags(ctx, client, accountID, folder, expectedUIDValidity)
	if err != nil {
		return err
	}
	if changed {
		return o.resetFolderUIDStateAndSync(ctx, client, accountID, accountProvider, folder, expectedUIDValidity, currentValidity)
	}

	if _, err := o.db.RefreshFolderUnreadCount(ctx, folder.ID); err != nil {
		return fmt.Errorf("refresh unread count %s/%s: %w", accountID, folder.RemoteID, err)
	}

	return nil
}

func (o *SyncOrchestrator) reconcileFolder(ctx context.Context, client *imap.Client, accountID string, folder storage.FolderSyncInfo, expectedUIDValidity, highestUID uint32) (uint32, bool, error) {
	serverUIDs, currentUIDValidity, changed, err := client.FetchAllUIDs(ctx, folder.RemoteID, expectedUIDValidity)
	if err != nil {
		return currentUIDValidity, false, fmt.Errorf("reconcile %s/%s: fetch uids: %w", accountID, folder.RemoteID, err)
	}
	if changed || folderUIDStateNeedsReset(expectedUIDValidity, currentUIDValidity, highestUID) {
		return currentUIDValidity, true, nil
	}

	localUIDs, err := o.db.GetLocalUIDs(ctx, folder.ID)
	if err != nil {
		return currentUIDValidity, false, fmt.Errorf("reconcile %s/%s: local uids: %w", accountID, folder.RemoteID, err)
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
			return currentUIDValidity, false, fmt.Errorf("reconcile %s/%s: remove: %w", accountID, folder.RemoteID, err)
		} else {
			log.Printf("reconcile %s/%s: removed %d expunged messages", accountID, folder.RemoteID, removed)
		}
	}
	return currentUIDValidity, false, nil
}

func (o *SyncOrchestrator) refreshFlags(ctx context.Context, client *imap.Client, accountID string, folder storage.FolderSyncInfo, expectedUIDValidity uint32) (uint32, bool, error) {
	localUIDs, err := o.db.GetLocalUIDs(ctx, folder.ID)
	if err != nil {
		return expectedUIDValidity, false, fmt.Errorf("flags %s/%s: local uids: %w", accountID, folder.RemoteID, err)
	}

	if len(localUIDs) == 0 {
		return expectedUIDValidity, false, nil
	}

	uids := make([]uint32, 0, len(localUIDs))
	for uid := range localUIDs {
		uids = append(uids, uid)
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })

	result, err := client.FetchFlagChanges(ctx, folder.RemoteID, uids, expectedUIDValidity, folder.HighestModSeq)
	if err != nil {
		return result.UIDValidity, false, fmt.Errorf("flags %s/%s: fetch: %w", accountID, folder.RemoteID, err)
	}
	if result.UIDValidityChanged {
		return result.UIDValidity, true, nil
	}

	converted := convertFlagUpdates(result.Updates)
	var changedCount int
	if result.CheckpointValid {
		changedCount, err = o.db.ApplyIMAPFlagChanges(ctx, folder.ID, result.UIDValidity, converted, result.HighestModSeq)
	} else {
		changedCount, err = o.db.BatchUpdateFlags(ctx, folder.ID, converted)
	}
	if err != nil {
		return result.UIDValidity, false, fmt.Errorf("flags %s/%s: update: %w", accountID, folder.RemoteID, err)
	} else if changedCount > 0 {
		log.Printf("flags %s/%s: %d changed", accountID, folder.RemoteID, changedCount)
		if _, err := o.db.RefreshFolderUnreadCount(ctx, folder.ID); err != nil {
			return result.UIDValidity, false, fmt.Errorf("flags %s/%s: refresh unread count: %w", accountID, folder.RemoteID, err)
		}
	}
	if result.UsedCondStore {
		log.Printf("flags %s/%s: CONDSTORE advanced to %d", accountID, folder.RemoteID, result.HighestModSeq)
	}
	return result.UIDValidity, false, nil
}

func (o *SyncOrchestrator) acquireBackgroundSyncSlot(ctx context.Context) (func(), bool) {
	if o.backgroundSyncSlots == nil {
		return func() {}, true
	}
	select {
	case o.backgroundSyncSlots <- struct{}{}:
		return func() { <-o.backgroundSyncSlots }, true
	case <-ctx.Done():
		return nil, false
	}
}

func (o *SyncOrchestrator) beginAccountSync(ctx context.Context, accountID string, kind accountSyncKind) (context.Context, func(), bool) {
	return o.beginAccountSyncWithMode(ctx, accountID, kind, "")
}

func (o *SyncOrchestrator) beginAccountSyncWithMode(ctx context.Context, accountID string, kind accountSyncKind, mode string) (context.Context, func(), bool) {
	releaseSlot := func() {}
	if kind == accountSyncBackground {
		var ok bool
		releaseSlot, ok = o.acquireBackgroundSyncSlot(ctx)
		if !ok {
			return nil, nil, false
		}
	}

	syncCtx, cancel := context.WithCancel(ctx)
	run := &accountSyncRun{
		kind:   kind,
		mode:   strings.TrimSpace(mode),
		cancel: cancel,
		done:   make(chan struct{}),
	}

	o.mu.Lock()
	if o.running[accountID] != nil {
		o.mu.Unlock()
		cancel()
		releaseSlot()
		return nil, nil, false
	}
	o.running[accountID] = run
	o.mu.Unlock()

	o.events.Publish(Event{Type: EventAccountSyncStatus, AccountID: accountID, Payload: accountSyncProgressPayload(ctx, kind, map[string]any{
		"status": "syncing",
	})})

	finish := func() {
		run.once.Do(func() {
			o.mu.Lock()
			if o.running[accountID] == run {
				delete(o.running, accountID)
			}
			o.mu.Unlock()
			cancel()
			releaseSlot()
			close(run.done)
		})
	}
	return syncCtx, finish, true
}

func (o *SyncOrchestrator) accountManualMode(accountID string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	run := o.running[accountID]
	if run == nil || run.kind != accountSyncManual {
		return ""
	}
	return strings.TrimSpace(run.mode)
}

func (o *SyncOrchestrator) beginManualAccountSync(ctx context.Context, accountID, mode string) (context.Context, func(), bool) {
	for {
		syncCtx, finish, ok := o.beginAccountSyncWithMode(ctx, accountID, accountSyncManual, mode)
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

func (o *SyncOrchestrator) markManualRunning(userID, mode string, run *manualSyncRun) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = "sync"
	}
	if o.manualRuns[userID] == nil {
		o.manualRuns[userID] = make(map[string]*manualSyncRun)
	}
	if o.manualRuns[userID][mode] != nil {
		return false
	}
	run.mode = mode
	o.manualRuns[userID][mode] = run
	return true
}

func (o *SyncOrchestrator) clearManualRunning(userID string, run *manualSyncRun) {
	o.mu.Lock()
	mode := strings.TrimSpace(run.mode)
	if mode == "" {
		mode = "sync"
	}
	if runs := o.manualRuns[userID]; runs != nil && runs[mode] == run {
		delete(runs, mode)
		if len(runs) == 0 {
			delete(o.manualRuns, userID)
		}
	}
	o.mu.Unlock()
}

func (o *SyncOrchestrator) CancelManualSync(userID string) bool {
	o.mu.Lock()
	runs := o.manualRuns[userID]
	toCancel := make([]*manualSyncRun, 0, len(runs))
	for _, run := range runs {
		if run != nil {
			toCancel = append(toCancel, run)
		}
	}
	o.mu.Unlock()
	if len(toCancel) == 0 {
		return false
	}
	for _, run := range toCancel {
		run.cancel()
	}
	return true
}

func (o *SyncOrchestrator) ActiveManualSyncSnapshot(ctx context.Context, userID string) []Event {
	if o == nil {
		return nil
	}
	type runSnapshot struct {
		runID      string
		mode       string
		accountIDs []string
		active     map[string]bool
	}

	o.mu.Lock()
	runs := o.manualRuns[userID]
	snapshots := make([]runSnapshot, 0, len(runs))
	for mode, run := range runs {
		if run == nil {
			continue
		}
		snap := runSnapshot{
			runID:      run.runID,
			mode:       strings.TrimSpace(mode),
			accountIDs: append([]string(nil), run.accountIDs...),
			active:     make(map[string]bool),
		}
		if snap.mode == "" {
			snap.mode = strings.TrimSpace(run.mode)
		}
		if snap.mode == "" {
			snap.mode = "sync"
		}
		for _, accountID := range snap.accountIDs {
			accountRun := o.running[accountID]
			if accountRun != nil && accountRun.kind == accountSyncManual && strings.TrimSpace(accountRun.mode) == snap.mode {
				snap.active[accountID] = true
			}
		}
		snapshots = append(snapshots, snap)
	}
	o.mu.Unlock()

	var events []Event
	for _, snap := range snapshots {
		total := len(snap.accountIDs)
		parallelism := accountSyncParallelism(total, manualSyncMaxParallelAccounts)
		events = append(events, Event{Type: EventManualSyncStarted, Payload: map[string]any{
			"user_id":        userID,
			"run_id":         snap.runID,
			"mode":           snap.mode,
			"accounts_total": total,
			"accounts_done":  0,
			"account_ids":    append([]string(nil), snap.accountIDs...),
			"parallelism":    parallelism,
		}})
		for index, accountID := range snap.accountIDs {
			if !snap.active[accountID] {
				continue
			}
			scope := accountSyncProgressScope{
				kind:          string(accountSyncManual),
				mode:          snap.mode,
				userID:        userID,
				runID:         snap.runID,
				accountIDs:    snap.accountIDs,
				accountsTotal: total,
				accountIndex:  index + 1,
				parallelism:   parallelism,
			}
			accountCtx := withAccountSyncProgressScope(ctx, scope)
			events = append(events, Event{Type: EventManualSyncProgress, AccountID: accountID, Payload: o.accountSyncStatusPayload(accountCtx, accountID, accountSyncManual, map[string]any{
				"status": "syncing",
			})})
			events = append(events, Event{Type: EventAccountSyncStatus, AccountID: accountID, Payload: o.accountSyncStatusPayload(accountCtx, accountID, accountSyncManual, map[string]any{
				"status": "syncing",
			})})
			if snap.mode == "repair" && o.db != nil {
				folders, err := o.db.GetFoldersForAccount(ctx, accountID)
				if err != nil {
					log.Printf("manual repair snapshot account=%s: folders: %v", accountID, err)
					continue
				}
				for _, folder := range folders {
					providerID := strings.TrimSpace(folder.ProviderRemoteID)
					if providerID == "" {
						continue
					}
					events = append(events, Event{
						Type:       EventSyncStarted,
						AccountID:  accountID,
						FolderID:   folder.ID,
						FolderRole: folder.Role,
						Payload:    o.gmailAPIFolderRefreshPayload(accountCtx, accountID, displayName(folder.RemoteID, folder.Role), true),
					})
				}
			}
		}
	}
	return events
}

func (o *SyncOrchestrator) SyncAccounts(ctx context.Context, userID string, accountIDs []string) (string, bool) {
	return o.syncAccountsWithOperation(ctx, userID, accountIDs, manualSyncTimeout, "sync", func(accountCtx context.Context, accountID string) error {
		return o.syncAccount(accountCtx, accountID, true)
	})
}

func (o *SyncOrchestrator) RepairGmailAPIAccount(ctx context.Context, userID, accountID string) (string, bool) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return "", false
	}
	return o.syncAccountsWithOperation(ctx, userID, []string{accountID}, manualRepairSyncTimeout, "repair", func(accountCtx context.Context, accountID string) error {
		return o.repairGmailAPIAccount(accountCtx, accountID)
	})
}

func (o *SyncOrchestrator) syncAccountsWithOperation(ctx context.Context, userID string, accountIDs []string, timeout time.Duration, mode string, operation manualSyncOperation) (string, bool) {
	if len(accountIDs) == 0 {
		return "", false
	}
	if timeout <= 0 {
		timeout = manualSyncTimeout
	}
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = "sync"
	}

	accountIDs = append([]string(nil), accountIDs...)
	runAccountIDs := append([]string(nil), accountIDs...)
	runID := userID + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	syncCtx, cancel := context.WithTimeout(ctx, timeout)
	run := &manualSyncRun{runID: runID, mode: mode, accountIDs: append([]string(nil), runAccountIDs...), cancel: cancel}
	if !o.markManualRunning(userID, mode, run) {
		cancel()
		return "", false
	}

	go func() {
		defer cancel()
		defer o.clearManualRunning(userID, run)

		total := len(accountIDs)
		parallelism := accountSyncParallelism(total, manualSyncMaxParallelAccounts)

		var progressMu sync.Mutex
		completed := 0
		skipped := 0
		failures := 0
		cancelled := 0

		o.events.Publish(Event{Type: EventManualSyncStarted, Payload: map[string]any{
			"user_id":        userID,
			"run_id":         runID,
			"mode":           mode,
			"accounts_total": total,
			"accounts_done":  0,
			"account_ids":    append([]string(nil), runAccountIDs...),
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
						"mode":           mode,
						"account_ids":    append([]string(nil), runAccountIDs...),
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
					accountRunCtx := withAccountSyncProgressScope(syncCtx, accountSyncProgressScope{
						kind:          string(accountSyncManual),
						mode:          mode,
						userID:        userID,
						runID:         runID,
						accountIDs:    runAccountIDs,
						accountsTotal: total,
						accountIndex:  job.index + 1,
						parallelism:   parallelism,
					})
					var accountCtx context.Context
					var finish func()
					var ok bool
					if mode == "sync" && o.accountManualMode(job.accountID) == "repair" {
						status = "skipped"
						errorText = "account is being repaired"
					} else {
						accountCtx, finish, ok = o.beginManualAccountSync(accountRunCtx, job.accountID, mode)
					}
					if status == "skipped" {
						// Account is intentionally omitted from this regular sync because a repair run owns it.
					} else if !ok {
						status = "cancelled"
						errorText = "manual sync could not start"
						if err := syncCtx.Err(); err != nil {
							errorText = err.Error()
						}
					} else {
						err := operation(accountCtx, job.accountID)
						if err != nil {
							if syncCtx.Err() != nil {
								status = "cancelled"
							} else {
								status = "error"
							}
							errorText = err.Error()
							log.Printf("manual %s account %s: %v", mode, job.accountID, err)
							o.markAccountSyncError(accountCtx, job.accountID, err)
						} else {
							o.clearAccountSyncError(accountCtx, job.accountID)
						}
						finish()
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
						"mode":           mode,
						"account_ids":    append([]string(nil), runAccountIDs...),
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
			"mode":           mode,
			"account_ids":    append([]string(nil), runAccountIDs...),
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
			UID:           u.UID,
			IsRead:        u.IsRead,
			IsStarred:     u.IsStarred,
			Labels:        u.Labels,
			LabelsKnown:   true,
			LabelProvider: storage.LabelProviderIMAPKeyword,
		}
	}
	return updates
}

func (o *SyncOrchestrator) SyncAccount(ctx context.Context, accountID string) bool {
	if !o.db.IsEmailSyncEnabled(ctx, accountID) {
		return false
	}
	syncCtx, finish, ok := o.beginAccountSync(ctx, accountID, accountSyncBackground)
	if !ok {
		return false
	}

	go func() {
		defer finish()

		if err := o.syncAccount(syncCtx, accountID, false); err != nil {
			log.Printf("sync account %s: %v", accountID, err)
			o.markAccountSyncError(syncCtx, accountID, err)
		} else {
			o.clearAccountSyncError(syncCtx, accountID)
		}
	}()
	return true
}

func (o *SyncOrchestrator) syncAccount(ctx context.Context, accountID string, includeIDLEFolders bool) error {
	if !o.db.IsEmailSyncEnabled(ctx, accountID) {
		return nil
	}
	cfg, err := o.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		return err
	}

	if o.shouldUseOutlookGraphMail(cfg) {
		return o.syncOutlookGraphAccount(ctx, accountID, includeIDLEFolders)
	}
	if o.shouldUseGmailAPIMail(cfg) {
		return o.syncGmailAPIAccount(ctx, accountID, includeIDLEFolders)
	}

	var idleFolderIDs map[string]bool
	if !includeIDLEFolders {
		idleFolderIDs = o.activeIdleFolderIDs(accountID)
		o.publishAutomaticSyncScope(ctx, accountID, idleFolderIDs)
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
			return fmt.Errorf("save IMAP folders for %s: %w", accountID, err)
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
	if !includeIDLEFolders {
		idleFolderIDs = o.activeIdleFolderIDs(accountID)
	}
	idleRemoteNames := idleRemoteNamesFromFolders(folderInfos, idleFolderIDs)

	var syncFolders []imap.FolderInfo
	for _, f := range folders {
		if f.Selectable {
			syncFolders = append(syncFolders, f)
		}
	}
	idleExcluded := 0
	if !includeIDLEFolders {
		syncFolders, idleExcluded = pollingIMAPFoldersForAutomaticSync(syncFolders, idleRemoteNames)
	}
	if len(syncFolders) == 0 {
		if err := o.syncProviderLabelChanges(ctx, accountID, cfg.Provider); err != nil {
			return err
		}
		payload := accountSyncProgressPayload(ctx, accountSyncBackground, map[string]any{
			"status":                "ok",
			"account_folders_total": 0,
		})
		if idleExcluded > 0 {
			payload["idle_folders_excluded"] = idleExcluded
		}
		o.events.Publish(Event{Type: EventAccountSyncStatus, AccountID: accountID, Payload: payload})
		return nil
	}

	var firstFolderErr error
	failedFolders := 0
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
		if err := o.fullFolderSync(ctx, client, accountID, cfg.Provider, folderInfo, i+1, len(syncFolders), idleExcluded); err != nil {
			log.Printf("sync folder %s/%s: %v", accountID, f.Name, err)
			failedFolders++
			if firstFolderErr == nil {
				firstFolderErr = err
			}
		}
	}

	var labelSyncErr error
	if includeIDLEFolders {
		labelSyncErr = o.syncProviderLabels(ctx, accountID, cfg.Provider)
	} else {
		labelSyncErr = o.syncProviderLabelChanges(ctx, accountID, cfg.Provider)
	}
	if failedFolders > 0 {
		folderSyncErr := fmt.Errorf("%d IMAP folder sync(s) failed: %w", failedFolders, firstFolderErr)
		return errors.Join(folderSyncErr, labelSyncErr)
	}
	return labelSyncErr
}

func (o *SyncOrchestrator) syncFolderMessages(ctx context.Context, client folderMessageSyncer, accountID, accountProvider, folderID, remoteName string) (syncErr error) {
	defer func() {
		if syncErr != nil {
			o.recordFolderSyncError(ctx, folderID, syncErr)
		}
	}()

	folderRole, err := o.db.GetFolderRole(ctx, folderID)
	if err != nil {
		return fmt.Errorf("load folder role %s/%s: %w", accountID, remoteName, err)
	}
	totalHint, _ := o.db.GetFolderEmailCount(ctx, folderID)
	folderName := displayName(remoteName, folderRole)
	o.events.Publish(Event{Type: EventSyncStarted, AccountID: accountID, FolderID: folderID, FolderRole: folderRole, Total: totalHint, Payload: o.folderSyncProgressPayload(ctx, accountID, folderName, accountProvider, nil)})
	fetched := 0
	result, err := client.SyncFolder(ctx, folderID, remoteName, 500, func(msgs []storage.SyncMessage) error {
		fetched += len(msgs)
		o.events.Publish(Event{Type: EventSyncProgress, AccountID: accountID, FolderID: folderID, FolderRole: folderRole, Current: fetched, Total: totalHint, Payload: o.folderSyncProgressPayload(ctx, accountID, folderName, accountProvider, nil)})
		return o.db.UpsertSyncMessages(ctx, withFolderLabels(msgs, accountProvider, remoteName, folderRole))
	})
	if err != nil {
		return fmt.Errorf("sync messages %s/%s: %w", accountID, remoteName, err)
	}
	if result == nil {
		return fmt.Errorf("sync messages %s/%s returned no result", accountID, remoteName)
	}
	if err := o.db.UpdateFolderSyncState(ctx, folderID, result.HighestUID, result.UIDValidity, int(result.NumMessages)); err != nil {
		return fmt.Errorf("save sync state %s/%s: %w", accountID, remoteName, err)
	}
	total := totalHint
	if total <= 0 {
		total = int(result.NumMessages)
	}
	o.events.Publish(Event{Type: EventSyncProgress, AccountID: accountID, FolderID: folderID, FolderRole: folderRole, Current: int(result.TotalFetched), Total: total, Payload: o.folderSyncProgressPayload(ctx, accountID, folderName, accountProvider, nil)})
	if _, err := o.db.RefreshFolderUnreadCount(ctx, folderID); err != nil {
		return fmt.Errorf("refresh unread count %s/%s: %w", accountID, remoteName, err)
	}
	log.Printf("synced %s/%s: %d messages", accountID, remoteName, result.TotalFetched)
	o.events.Publish(Event{Type: EventSyncComplete, AccountID: accountID, FolderID: folderID, FolderRole: folderRole, Payload: o.folderSyncProgressPayload(ctx, accountID, folderName, accountProvider, nil)})
	return nil
}

func (o *SyncOrchestrator) trySyncIncremental(ctx context.Context, accountID, folderID, remoteName string) bool {
	syncCtx, finish, ok := o.beginAccountSync(ctx, accountID, accountSyncBackground)
	if !ok {
		return false
	}
	defer finish()
	if o.incrementalOverride != nil {
		o.incrementalOverride(syncCtx, accountID, folderID, remoteName)
		return true
	}
	o.syncIncremental(syncCtx, accountID, folderID, remoteName)
	return true
}

func (o *SyncOrchestrator) syncIncremental(syncCtx context.Context, accountID, folderID, remoteName string) {
	storedUIDValidity, err := o.db.GetStoredUIDValidity(syncCtx, folderID)
	if err != nil {
		log.Printf("incremental %s/%s: get UIDVALIDITY: %v", accountID, remoteName, err)
		o.markAccountSyncError(syncCtx, accountID, err)
		return
	}

	highestUID, err := o.db.GetHighestSeenUID(syncCtx, folderID)
	if err != nil {
		log.Printf("incremental %s/%s: get uid: %v", accountID, remoteName, err)
		o.markAccountSyncError(syncCtx, accountID, err)
		return
	}
	highestModSeq, err := o.db.GetHighestModSeq(syncCtx, folderID)
	if err != nil {
		log.Printf("incremental %s/%s: get modseq: %v", accountID, remoteName, err)
		o.markAccountSyncError(syncCtx, accountID, err)
		return
	}

	cfg, err := o.accountStore.GetConfig(syncCtx, accountID)
	if err != nil {
		log.Printf("incremental %s/%s: config: %v", accountID, remoteName, err)
		o.markAccountSyncError(syncCtx, accountID, err)
		return
	}

	password, err := o.resolvePassword(syncCtx, cfg, accountID)
	if err != nil {
		log.Printf("incremental %s/%s: password: %v", accountID, remoteName, err)
		o.markAccountSyncError(syncCtx, accountID, err)
		return
	}

	client, err := imap.NewClient(syncCtx, cfg, password)
	if err != nil {
		log.Printf("incremental %s/%s: connect: %v", accountID, remoteName, err)
		o.markAccountSyncError(syncCtx, accountID, err)
		return
	}
	defer client.Close()

	folderRole, _ := o.db.GetFolderRole(syncCtx, folderID)
	folder := storage.FolderSyncInfo{
		ID:            folderID,
		AccountID:     accountID,
		RemoteID:      remoteName,
		Role:          folderRole,
		HighestModSeq: highestModSeq,
	}

	currentUIDValidity, changed, err := o.reconcileAndRefresh(syncCtx, client, accountID, folder, storedUIDValidity, highestUID)
	if err != nil {
		log.Printf("incremental %s/%s: %v", accountID, remoteName, err)
		o.markAccountSyncError(syncCtx, accountID, err)
		return
	}
	if changed {
		if err := o.resetFolderUIDStateAndSync(syncCtx, client, accountID, cfg.Provider, folder, storedUIDValidity, currentUIDValidity); err != nil {
			log.Printf("incremental %s/%s: reset UID state: %v", accountID, remoteName, err)
			o.markAccountSyncError(syncCtx, accountID, err)
		}
		return
	}
	if currentUIDValidity > 0 {
		storedUIDValidity = currentUIDValidity
	}

	var summary newMailSummary
	result, err := client.SyncFolderIncremental(syncCtx, folderID, remoteName, highestUID, storedUIDValidity, func(msgs []storage.SyncMessage) error {
		summary.Add(msgs)
		return o.db.UpsertSyncMessages(syncCtx, withFolderLabels(msgs, cfg.Provider, remoteName, folderRole))
	})
	if err != nil {
		log.Printf("incremental %s/%s: %v", accountID, remoteName, err)
		o.markAccountSyncError(syncCtx, accountID, err)
		return
	}
	if result == nil {
		return
	}
	if result.UIDValidityChanged {
		if err := o.resetFolderUIDStateAndSync(syncCtx, client, accountID, cfg.Provider, folder, storedUIDValidity, result.UIDValidity); err != nil {
			log.Printf("incremental %s/%s: reset UID state: %v", accountID, remoteName, err)
			o.markAccountSyncError(syncCtx, accountID, err)
		}
		return
	}
	o.clearAccountSyncError(syncCtx, accountID)

	if result.TotalFetched > 0 {
		log.Printf("incremental %s/%s: %d new messages", accountID, remoteName, result.TotalFetched)
	}

	if err := o.db.UpdateFolderIncrementalSync(syncCtx, folderID, result.HighestUID, result.UIDValidity, int(result.NumMessages)); err != nil {
		log.Printf("incremental %s/%s: update sync state: %v", accountID, remoteName, err)
		o.markAccountSyncError(syncCtx, accountID, err)
		return
	}

	if err := o.syncProviderLabelChanges(syncCtx, accountID, cfg.Provider); err != nil {
		log.Printf("incremental %s/%s labels: %v", accountID, remoteName, err)
		o.markAccountSyncError(syncCtx, accountID, err)
		return
	}

	unread, _ := o.db.RefreshFolderUnreadCount(syncCtx, folderID)

	o.publishNewMail(syncCtx, accountID, folderID, remoteName, summary)
	_ = unread
}

func (o *SyncOrchestrator) reconcileAndRefresh(ctx context.Context, client *imap.Client, accountID string, folder storage.FolderSyncInfo, expectedUIDValidity, highestUID uint32) (uint32, bool, error) {
	currentUIDValidity, changed, err := o.reconcileFolder(ctx, client, accountID, folder, expectedUIDValidity, highestUID)
	if err != nil || changed {
		return currentUIDValidity, changed, err
	}
	if currentUIDValidity > 0 {
		expectedUIDValidity = currentUIDValidity
	}
	return o.refreshFlags(ctx, client, accountID, folder, expectedUIDValidity)
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

func withFolderLabels(msgs []storage.SyncMessage, accountProvider, remoteName, role string) []storage.SyncMessage {
	label, ok := syncedFolderLabel(accountProvider, remoteName, role)
	for i := range msgs {
		if role == "drafts" {
			msgs[i].IsDraft = true
		}
		if ok && !messageHasLabel(msgs[i].Labels, label) {
			msgs[i].Labels = append(msgs[i].Labels, label)
		}
	}
	return msgs
}

func syncedFolderLabel(accountProvider, remoteName, role string) (storage.LabelInput, bool) {
	if strings.TrimSpace(accountProvider) != providers.ProviderGmail || role != "custom" {
		return storage.LabelInput{}, false
	}
	name := strings.TrimSpace(remoteName)
	if name == "" || strings.HasPrefix(strings.ToUpper(name), "[GMAIL]") {
		return storage.LabelInput{}, false
	}
	return storage.LabelInput{
		Name:         name,
		ProviderID:   name,
		ProviderType: storage.LabelProviderGmail,
	}, true
}

func messageHasLabel(labels []storage.LabelInput, label storage.LabelInput) bool {
	name := strings.ToLower(strings.TrimSpace(label.Name))
	providerType := strings.TrimSpace(label.ProviderType)
	for _, existing := range labels {
		if strings.EqualFold(strings.TrimSpace(existing.Name), name) && strings.TrimSpace(existing.ProviderType) == providerType {
			return true
		}
	}
	return false
}
