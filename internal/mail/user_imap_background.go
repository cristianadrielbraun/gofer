package mail

import (
	"context"
	"errors"
	"log"
	"sort"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

var errUserIMAPQueueFull = errors.New("IMAP sync queue is full")

type UserIMAPBackgroundOptions struct {
	PollInterval        time.Duration
	ScanInterval        time.Duration
	FileCleanupInterval time.Duration
	MaxIdleWatchers     int
	DisableIDLE         bool
}
type userIMAPQueueState struct{ running, dirty bool }
type userIMAPWatchKey struct{ account, folder string }
type userIMAPWatch struct {
	owner   string
	remote  string
	cancel  context.CancelFunc
	watcher *imap.IdleWatcher
	dirty   bool
	status  IDLEFolderRuntimeStatus
}

// Start enables background discovery, periodic reconciliation and bounded IDLE.
// It is explicit: constructing the service alone still starts no discovery.
func (s *UserIMAP) Start(options UserIMAPBackgroundOptions) error {
	if options.PollInterval < 0 || options.ScanInterval < 0 || options.FileCleanupInterval < 0 || options.MaxIdleWatchers < 0 || options.MaxIdleWatchers > 4096 {
		return errors.New("IMAP background limits must be nonnegative and at most 4096 watchers")
	}
	if options.PollInterval == 0 {
		options.PollInterval = 5 * time.Minute
	}
	if options.ScanInterval == 0 {
		options.ScanInterval = min(options.PollInterval, time.Minute)
	}
	if options.MaxIdleWatchers == 0 {
		options.MaxIdleWatchers = 64
	}
	if options.FileCleanupInterval == 0 {
		options.FileCleanupInterval = time.Hour
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.ctx.Err() != nil {
		return errors.New("IMAP worker is shutting down")
	}
	if s.background {
		return errors.New("IMAP background service already started")
	}
	s.background = true
	s.rescan = false // Startup always refreshes all accounts, including pre-start manual receives.
	s.backgroundOptions = options
	s.workers.Add(1)
	go s.backgroundLoop(options)
	s.workers.Add(1)
	go s.runFileCleanup(options.FileCleanupInterval)
	return nil
}

func (s *UserIMAP) defaultPollInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backgroundOptions.PollInterval > 0 {
		return s.backgroundOptions.PollInterval
	}
	return 5 * time.Minute
}

func (s *UserIMAP) requestDiscovery() {
	s.mu.Lock()
	s.rescan = true
	s.mu.Unlock()
	s.wakeBackground()
}

// RefreshUserSettings is called after local preferences commit, without a user
// lease. Reset deadlines durably; queue saturation is retried by discovery.
func (s *UserIMAP) RefreshUserSettings(ctx context.Context, owner string) error {
	if err := s.Routing().ResetUserPolling(ctx, owner); err != nil {
		return err
	}
	s.mu.Lock()
	for key, w := range s.watches {
		if w.owner == owner {
			delete(s.watches, key)
			w.cancel()
		}
	}
	s.rescan = true
	s.mu.Unlock()
	s.wakeBackground()
	return nil
}
func (s *UserIMAP) wakeBackground() {
	select {
	case s.backgroundWake <- struct{}{}:
	default:
	}
}

// Caller holds mu. Pending jobs coalesce; a notification during a running sync
// records one follow-up so changes arriving after its snapshot are not lost.
func (s *UserIMAP) enqueueLocked(job userIMAPJob, changed bool) error {
	if s.closing || s.ctx.Err() != nil {
		return errors.New("IMAP worker is shutting down")
	}
	if state := s.queued[job.account]; state != nil {
		if changed && state.running {
			state.dirty = true
		}
		return nil
	}
	select {
	case s.queue <- job:
		s.queued[job.account] = &userIMAPQueueState{}
		return nil
	default:
		return errUserIMAPQueueFull
	}
}

// A full queue must not deadlock all workers trying to enqueue their follow-up.
// Swap its oldest job into this worker and append the follow-up at the tail.
func (s *UserIMAP) finishJob(job userIMAPJob) *userIMAPJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.queued[job.account]
	if state == nil {
		return nil
	}
	if !state.dirty || s.ctx.Err() != nil || s.closing {
		delete(s.queued, job.account)
		return nil
	}
	state.running = false
	state.dirty = false
	select {
	case s.queue <- job:
		return nil
	default:
	}
	var next *userIMAPJob
	select {
	case old := <-s.queue:
		next = &old
	default:
	}
	s.queue <- job // Only receives can occur while mu is held; a slot is now free.
	return next
}
func (s *UserIMAP) backgroundLoop(options UserIMAPBackgroundOptions) {
	defer s.workers.Done()
	poll := time.NewTicker(options.ScanInterval)
	defer poll.Stop()
	monitor := time.NewTicker(250 * time.Millisecond)
	defer monitor.Stop()
	cursor := ""
	scanning := true
	bootstrap := true
	for {
		s.mu.Lock()
		if s.rescan {
			s.rescan = false
			scanning = true
			cursor = ""
			bootstrap = false
		}
		s.mu.Unlock()
		if scanning {
			var page []storage.AccountRoute
			var err error
			if bootstrap {
				page, err = s.Routing().ListAccounts(s.ctx, "", storage.AccountActive, cursor, 64)
			} else {
				page, err = s.Routing().ListDueAccounts(s.ctx, cursor, time.Now(), 64)
			}
			if err != nil {
				if s.ctx.Err() == nil {
					log.Printf("user IMAP discovery: %v", err)
				}
				scanning = false
			} else if len(page) == 0 {
				scanning = false
				bootstrap = false
				cursor = ""
			} else {
				progressed := true
				for _, entry := range page {
					s.mu.Lock()
					err := s.enqueueLocked(userIMAPJob{entry.UserID, entry.AccountID}, false)
					s.mu.Unlock()
					if err != nil {
						progressed = false
						break
					}
					cursor = entry.AccountID
				}
				if progressed {
					s.wakeBackground()
				}
			}
		}
		select {
		case <-s.ctx.Done():
			s.stopAllWatches()
			return
		case <-poll.C:
			if !scanning {
				cursor = ""
				scanning = true
			}
		case <-s.backgroundWake:
			s.flushIdleChanges()
		case <-monitor.C:
			s.validateWatches()
			s.flushIdleChanges()
		}
	}
}
func (s *UserIMAP) flushIdleChanges() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, w := range s.watches {
		if w.dirty {
			if s.enqueueLocked(userIMAPJob{w.owner, key.account}, true) == nil {
				w.dirty = false
			}
		}
	}
}
func (s *UserIMAP) validateWatches() {
	s.mu.Lock()
	owners := make(map[string]string)
	for key, w := range s.watches {
		owners[key.account] = w.owner
	}
	s.mu.Unlock()
	if len(owners) == 0 {
		return
	}
	ids := make([]string, 0, len(owners))
	for id := range owners {
		ids = append(ids, id)
	}
	active, err := s.Routing().ActiveAccountOwners(s.ctx, ids)
	// A transient central read failure does not authorize new work. Existing
	// connections retry validation on the next tick; all writes still route.
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, owner := range owners {
		if active[id] != owner {
			s.stopWatchesLocked(id)
		}
	}
}
func (s *UserIMAP) stopWatchesLocked(id string) {
	for key, w := range s.watches {
		if key.account == id {
			delete(s.watches, key)
			w.cancel()
		}
	}
}
func (s *UserIMAP) stopAllWatches() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, w := range s.watches {
		delete(s.watches, key)
		w.cancel()
	}
}

// Called inside the account sync activity/gate, after successful reconciliation.
// Copy folder/configuration data, then start watchers with no retained DB lease.
func (s *UserIMAP) reconcileWatches(ctx context.Context, scope *userIMAPScope) error {
	s.mu.Lock()
	if scope.config.Provider != "imap" || scope.config.AuthMethod != "plain" {
		s.stopWatchesLocked(scope.id)
		s.mu.Unlock()
		return nil
	}
	enabled := s.background && !s.backgroundOptions.DisableIDLE
	s.mu.Unlock()
	if !enabled {
		return nil
	}
	var folders []storage.FolderSyncInfo
	var selected map[string]bool
	if err := scope.call(ctx, func(db *storage.DB) error {
		var err error
		folders, err = db.GetFoldersForAccount(ctx, scope.id)
		if err != nil {
			return err
		}
		selected = db.GetIdleFolderIDsForAccount(ctx, scope.owner, scope.id)
		return nil
	}); err != nil {
		return err
	}
	wanted := map[string]storage.FolderSyncInfo{}
	for _, f := range folders {
		if selected[f.ID] && f.RemoteID != "" {
			wanted[f.ID] = f
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || ctx.Err() != nil || s.ctx.Err() != nil {
		return ctx.Err()
	}
	for key, w := range s.watches {
		if key.account == scope.id {
			f, ok := wanted[key.folder]
			if !ok || f.RemoteID != w.remote {
				delete(s.watches, key)
				w.cancel()
			}
		}
	}
	// Repository order gives a stable preference under the configured bound.
	for _, folder := range folders {
		if _, ok := wanted[folder.ID]; !ok {
			continue
		}
		key := userIMAPWatchKey{scope.id, folder.ID}
		if s.watches[key] != nil || s.idleCount >= s.backgroundOptions.MaxIdleWatchers {
			continue
		}
		watchCtx, cancel := context.WithCancel(s.ctx)
		w := &userIMAPWatch{owner: scope.owner, remote: folder.RemoteID, cancel: cancel, status: IDLEFolderRuntimeStatus{AccountID: scope.id, FolderID: folder.ID, RemoteName: folder.RemoteID, ReasonCode: "connecting", Reason: "Connecting to real-time notifications; periodic polling is active."}}
		w.watcher = imap.NewIdleWatcher(scope.config, scope.password, folder.RemoteID, func() {
			s.mu.Lock()
			if s.watches[key] == w && watchCtx.Err() == nil {
				w.dirty = true
			}
			s.mu.Unlock()
			s.wakeBackground()
		}, func(status imap.IdleWatcherStatus) {
			s.mu.Lock()
			current := s.watches[key] == w && watchCtx.Err() == nil
			if current {
				wasHealthy := w.status.Healthy
				w.status.Healthy = status.Healthy
				w.status.ReasonCode = status.ReasonCode
				w.status.Reason = status.Reason
				w.status.RetryAt = status.RetryAt
				if status.Healthy && !wasHealthy {
					w.dirty = true
				}
			}
			snapshot := w.status
			s.mu.Unlock()
			if current {
				event := idleFolderStatusEvent(snapshot)
				event.UserID = scope.owner
				s.events.Publish(event)
				s.wakeBackground()
			}
		})
		s.watches[key] = w
		s.idleCount++
		s.watchRuns.Add(1)
		go s.runWatch(watchCtx, key, w)
	}
	return nil
}
func (s *UserIMAP) runWatch(ctx context.Context, key userIMAPWatchKey, w *userIMAPWatch) {
	defer s.watchRuns.Done()
	defer func() {
		w.cancel()
		w.watcher.Close()
		s.mu.Lock()
		if s.watches[key] == w {
			delete(s.watches, key)
		}
		s.idleCount--
		// A fresh sync may finish before a cancelled watcher releases the last
		// available slot. Revisit its account after joining the old connection.
		// Saturation is covered by the next discovery pass; never block a watcher.
		if s.ctx.Err() == nil && !s.closing {
			_ = s.enqueueLocked(userIMAPJob{w.owner, key.account}, true)
		}
		s.mu.Unlock()
		s.wakeBackground()
	}()
	_ = s.Routing().WithAccountActivityForUser(ctx, w.owner, key.account, func() error { w.watcher.Run(ctx); return ctx.Err() })
}

func (s *UserIMAP) IdleStatusesForUser(ctx context.Context, owner string) ([]IDLEFolderRuntimeStatus, error) {
	if err := s.Routing().ValidateUser(ctx, owner); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var states []IDLEFolderRuntimeStatus
	for _, w := range s.watches {
		if w.owner == owner {
			states = append(states, w.status)
		}
	}
	sort.Slice(states, func(i, j int) bool { return states[i].FolderID < states[j].FolderID })
	return states, nil
}

func (s *UserIMAP) IdleEventsForUser(ctx context.Context, owner string) ([]Event, error) {
	states, err := s.IdleStatusesForUser(ctx, owner)
	if err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(states))
	for _, state := range states {
		event := idleFolderStatusEvent(state)
		event.UserID = owner
		events = append(events, event)
	}
	return events, nil
}
