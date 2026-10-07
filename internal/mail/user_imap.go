package mail

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

// UserIMAP is an opt-in, bounded IMAP/Gmail/Outlook worker. Main does not start it yet.
// A session holds an account activity guard, but leases user storage only for
// snapshots and commits. Background discovery/IDLE require explicit Start.
type UserIMAP struct {
	accounts          *config.UserAccountStore
	blobs             *store.BlobStore
	events            *EventBus
	ctx               context.Context
	slots             chan struct{}
	contactSlots      chan struct{}
	calendarSlots     chan struct{}
	queue             chan userIMAPJob
	mu                sync.Mutex
	gates             map[userIMAPKey]*userIMAPGate
	queued            map[string]*userIMAPQueueState
	background        bool
	backgroundOptions UserIMAPBackgroundOptions
	backgroundWake    chan struct{}
	activePollQueue   chan userActivePollJob
	activePollWake    chan struct{}
	activePollUsers   map[string]*userActivePollSession
	activePollPending map[string]*userActivePollSession
	rescan            bool
	manualRuns        map[string]*userIMAPManualRun
	mailQueue         UserMailQueue
	credentials       *mailauth.UserCredentials
	fileCleanup       map[string]time.Time
	watches           map[userIMAPWatchKey]*userIMAPWatch
	idleCount         int
	watchRuns         sync.WaitGroup
	workers           sync.WaitGroup
	operations        sync.WaitGroup
	closing           bool
}
type userIMAPJob struct{ owner, account string }
type userIMAPKey struct {
	account string
	message int64
	service AccountService
}
type userIMAPOperation struct{ cancel context.CancelFunc }
type userIMAPGate struct {
	token chan struct{}
	refs  int
	runs  map[*userIMAPOperation]struct{}
}
type userIMAPScope struct {
	accounts            *config.UserAccountStore
	owner, id, password string
	config              *models.AccountConfig
	account             *models.Account
	tokens              TokenProvider
	pollInterval        time.Duration
	retryAt             time.Time
}

func (r *userIMAPScope) call(ctx context.Context, fn func(*storage.DB) error) error {
	return r.accounts.Routing().WithAccountForUser(ctx, r.owner, r.id, func(db *storage.DB) error {
		if r.config != nil && (r.config.Provider == providers.ProviderGmail || r.config.Provider == providers.ProviderOutlook) {
			var provider, subject, method string
			if err := db.Read().QueryRowContext(ctx, `SELECT provider,provider_account_id,auth_method FROM accounts WHERE id=?`, r.id).Scan(&provider, &subject, &method); err != nil {
				return err
			}
			if provider != r.config.Provider || subject != r.config.ProviderAccountID || method != r.config.AuthMethod {
				return errors.New("mailbox identity changed during provider work")
			}
		}
		return fn(db)
	})
}

func NewUserIMAP(ctx context.Context, accounts *config.UserAccountStore, blobs *store.BlobStore, events *EventBus) (*UserIMAP, error) {
	if ctx == nil || accounts == nil || blobs == nil || events == nil {
		return nil, errors.New("user IMAP requires lifecycle context, accounts, blobs and events")
	}
	s := &UserIMAP{accounts: accounts, blobs: blobs, events: events, ctx: ctx, slots: make(chan struct{}, 4), contactSlots: make(chan struct{}, 2), calendarSlots: make(chan struct{}, 2), queue: make(chan userIMAPJob, 32), gates: make(map[userIMAPKey]*userIMAPGate), queued: make(map[string]*userIMAPQueueState), backgroundWake: make(chan struct{}, 1), activePollQueue: make(chan userActivePollJob, 32), activePollWake: make(chan struct{}, 1), activePollUsers: make(map[string]*userActivePollSession), activePollPending: make(map[string]*userActivePollSession), watches: make(map[userIMAPWatchKey]*userIMAPWatch)}
	for i := 0; i < 4; i++ {
		s.workers.Add(1)
		go s.work()
	}
	return s, nil
}
func (s *UserIMAP) Routing() *storage.AccountRouting { return s.accounts.Routing() }
func (s *UserIMAP) Blobs() *store.BlobStore          { return s.blobs }

// SupportsAccount reports providers configured for the routed receive service.
// Ownership and current authorization are checked separately for every call.
func (s *UserIMAP) SupportsAccount(cfg *models.AccountConfig) bool {
	if cfg == nil {
		return false
	}
	if cfg.Provider == providers.ProviderIMAP && cfg.AuthMethod == "plain" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return (cfg.Provider == providers.ProviderGmail || cfg.Provider == providers.ProviderOutlook) && cfg.AuthMethod == "oauth2" && s.credentials != nil
}

// Wait joins workers and request sessions after cancelling the lifecycle context.
func (s *UserIMAP) Wait() {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.workers.Wait()
	s.operations.Wait()
	s.watchRuns.Wait()
}

// QueueAccount resolves a trusted globally unique account ID. Queued jobs keep
// only IDs; saturation is reported to the caller rather than spawning workers.
func (s *UserIMAP) QueueAccount(ctx context.Context, id string) error {
	var owner string
	err := s.accounts.WithAccount(ctx, id, func(_ *config.AccountStore, _ *storage.DB, user string) error { owner = user; return nil })
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return errors.New("IMAP worker is shutting down")
	}
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	return s.enqueueLocked(userIMAPJob{owner, id}, true)
}

// RestartAccount cancels sessions using the previous configuration before
// queuing a fresh snapshot. It is called with a trusted, routed account ID.
func (s *UserIMAP) RestartAccount(ctx context.Context, id string) error {
	s.mu.Lock()
	s.stopAccountLocked(id, true)
	s.mu.Unlock()
	if err := s.Routing().ResetActivePollingForAccount(ctx, id); err != nil {
		return err
	}
	s.wakeActivePoll()
	return s.QueueAccount(ctx, id)
}

// StopAccount cancels current sessions and IDLE for a trusted global account ID.
// It never deletes account data or queues a replacement. Future work snapshots
// current configuration and installation transport policy again.
func (s *UserIMAP) StopAccount(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopAccountLocked(id, false)
}

func (s *UserIMAP) stopAccountLocked(id string, resync bool) {
	if !resync {
		for key, w := range s.watches {
			if key.account == id {
				w.suppressResync = true
			}
		}
	}
	s.stopWatchesLocked(id)
	for key, g := range s.gates {
		if key.account == id {
			for run := range g.runs {
				run.cancel()
			}
		}
	}
}

// StopMailSecuritySessions fails closed after a policy revocation when damaged
// storage prevents enumerating affected accounts. It only cancels sessions;
// discovery and later requests must read the now-revoked central policy.
func (s *UserIMAP) StopMailSecuritySessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, w := range s.watches {
		w.suppressResync = true
		delete(s.watches, key)
		w.cancel()
	}
	for _, gate := range s.gates {
		for run := range gate.runs {
			run.cancel()
		}
	}
}

func (s *UserIMAP) work() {
	defer s.workers.Done()
	for {
		var job userIMAPJob
		select {
		case <-s.ctx.Done():
			return
		case job = <-s.queue:
		}
		for {
			s.mu.Lock()
			if state := s.queued[job.account]; state != nil {
				state.running = true
			}
			s.mu.Unlock()
			s.wakeBackground()
			if err := s.Sync(s.ctx, job.owner, job.account); err != nil && s.ctx.Err() == nil {
				log.Printf("user IMAP sync %s: %v", job.account, err)
			}
			next := s.finishJob(job)
			s.wakeBackground()
			if next == nil {
				break
			}
			job = *next
		}
	}
}

// Serialize duplicate syncs and duplicate message fetches independently. Opening
// a message need not wait for a full mailbox sync. Publication rechecks the
// message/folder identity in its SQL transaction if sync resets the mailbox.
func (s *UserIMAP) operation(ctx context.Context, owner, id string, messageID int64, timeout time.Duration, fn func(context.Context) error) error {
	return s.operationWithSlots(ctx, owner, id, messageID, 0, timeout, s.slots, fn)
}

func (s *UserIMAP) operationWithSlots(ctx context.Context, owner, id string, messageID int64, service AccountService, timeout time.Duration, slots chan struct{}, fn func(context.Context) error) error {
	if err := s.Routing().ValidateUser(ctx, owner); err != nil {
		return err
	}
	state, err := s.Routing().AccountStateForUser(ctx, owner, id)
	if err != nil {
		return err
	}
	if state != storage.AccountActive {
		return storage.ErrAccountRoute
	}
	workCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stopRoot := context.AfterFunc(s.ctx, cancel)
	defer stopRoot()
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return errors.New("IMAP worker is shutting down")
	}
	s.operations.Add(1)
	defer s.operations.Done()
	key := userIMAPKey{account: id, message: messageID, service: service}
	g := s.gates[key]
	if g == nil {
		g = &userIMAPGate{token: make(chan struct{}, 1), runs: make(map[*userIMAPOperation]struct{})}
		s.gates[key] = g
	}
	g.refs++
	run := &userIMAPOperation{cancel: cancel}
	g.runs[run] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		g.refs--
		delete(g.runs, run)
		if g.refs == 0 {
			delete(s.gates, key)
		}
		s.mu.Unlock()
	}()
	select {
	case g.token <- struct{}{}:
		defer func() { <-g.token }()
	case <-workCtx.Done():
		return workCtx.Err()
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-workCtx.Done():
		return workCtx.Err()
	}
	return s.Routing().WithAccountActivityForUser(workCtx, owner, id, func() error {
		// Deletion commits its intent before draining. Cancel protocol waits when
		// that intent becomes visible, so cleanup does not wait for the timeout.
		stopped := make(chan struct{})
		defer func() { cancel(); <-stopped }()
		go func() {
			defer close(stopped)
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-workCtx.Done():
					return
				case <-tick.C:
					state, err := s.Routing().AccountStateForUser(workCtx, owner, id)
					if err != nil || state != storage.AccountActive || s.Routing().ValidateUser(workCtx, owner) != nil {
						cancel()
						return
					}
				}
			}
		}()
		return fn(workCtx)
	})
}
func (s *UserIMAP) snapshot(ctx context.Context, owner, id string) (*userIMAPScope, error) {
	r := &userIMAPScope{accounts: s.accounts, owner: owner, id: id, pollInterval: s.defaultPollInterval()}
	s.mu.Lock()
	credentials := s.credentials
	s.mu.Unlock()
	err := s.accounts.WithAccountForUser(ctx, owner, id, func(local *config.AccountStore, db *storage.DB) error {
		var err error
		interval, err := db.GetSetting(ctx, owner, "sync_interval_minutes")
		if err != nil {
			return err
		}
		if n, err := strconv.ParseInt(interval, 10, 64); err == nil && n > 0 && n <= int64((1<<63-1)/time.Minute) {
			r.pollInterval = time.Duration(n) * time.Minute
		}
		r.config, err = local.GetConfig(ctx, id)
		if err != nil {
			return err
		}
		if (r.config.Provider == providers.ProviderGmail || r.config.Provider == providers.ProviderOutlook) && r.config.AuthMethod == "oauth2" && credentials != nil {
			r.tokens = credentials.Account(owner, id)
		} else if r.config.Provider != providers.ProviderIMAP || r.config.AuthMethod != "plain" {
			return errors.New("routed worker supports plain IMAP and configured Gmail/Outlook OAuth accounts only")
		}
		r.account, err = local.GetAccountByIDForUser(ctx, owner, id)
		if err != nil {
			return err
		}
		if r.tokens == nil {
			r.password, err = local.DecryptPassword(ctx, id)
		}
		return err
	})
	return r, err
}
func (s *UserIMAP) Sync(ctx context.Context, owner, id string) error {
	return s.sync(ctx, owner, id, false)
}

func (s *UserIMAP) sync(ctx context.Context, owner, id string, repair bool) error {
	timeout := manualSyncTimeout
	if repair {
		timeout = manualRepairSyncTimeout
	}
	return s.operation(ctx, owner, id, 0, timeout, func(ctx context.Context) error {
		revision, err := s.Routing().PollRevision(ctx, owner, id)
		if err != nil {
			return err
		}
		interval := s.defaultPollInterval()
		s.mu.Lock()
		queue := s.mailQueue
		s.mu.Unlock()
		var scope *userIMAPScope
		queueFailed := false
		var receiveErr error
		defer func() {
			if ctx.Err() != nil {
				return
			}
			next := time.Now().Add(interval)
			if receiveErr != nil && scope != nil && scope.config.Provider == providers.ProviderOutlook {
				next = minTime(next, time.Now().Add(30*time.Second))
				queueFailed = true
			}
			if scope != nil {
				var queued time.Time
				err := scope.call(ctx, func(db *storage.DB) error {
					var err error
					if scope.config.Provider == providers.ProviderGmail {
						queued, err = db.NextGmailQueueAttempt(ctx, id)
					} else if scope.config.Provider == providers.ProviderOutlook {
						queued, err = db.NextProviderLabelQueueAttempt(ctx, id, storage.LabelProviderOutlook)
					}
					if err != nil {
						return err
					}
					labelNext, labelErr := db.NextUserLabelMutationAttempt(ctx, id)
					if labelErr != nil {
						return labelErr
					}
					if !labelNext.IsZero() && (queued.IsZero() || labelNext.Before(queued)) {
						queued = labelNext
					}
					mutationNext, mutationErr := db.NextMessageMutationAttempt(ctx, id)
					if mutationErr != nil {
						return mutationErr
					}
					if !mutationNext.IsZero() && (queued.IsZero() || mutationNext.Before(queued)) {
						queued = mutationNext
					}
					if err == nil && queue != nil {
						mailNext, mailErr := db.NextAccountMailQueueAttempt(ctx, id)
						if mailErr != nil {
							return mailErr
						}
						if !mailNext.IsZero() && (queued.IsZero() || mailNext.Before(queued)) {
							queued = mailNext
						}
					}
					return err
				})
				if err != nil {
					// Retry a failed queue read rather than hiding durable outbound work
					// behind a potentially day-long receive interval.
					log.Printf("user IMAP mutation deadline %s: %v", id, err)
					next = minTime(next, time.Now().Add(30*time.Second))
				} else if !queued.IsZero() {
					next = minTime(next, queued)
				}
			}
			if queueFailed && next.Before(time.Now().Add(30*time.Second)) {
				// A failed claim/finalization can leave a due processing row. Avoid
				// spinning on persistent local storage failure; a new wake still
				// wins through the polling revision check below.
				next = time.Now().Add(30 * time.Second)
			}
			if scope != nil && scope.retryAt.After(next) {
				next = scope.retryAt
			}
			updated, err := s.Routing().DeferAccountPoll(ctx, owner, id, revision, next)
			if err != nil {
				log.Printf("user IMAP polling deadline %s: %v", id, err)
			}
			if err == nil && !updated {
				s.mu.Lock()
				if state := s.queued[id]; state != nil {
					state.dirty = true
				}
				s.mu.Unlock()
				s.requestDiscovery()
			}
		}()
		scope, err = s.snapshot(ctx, owner, id)
		if err != nil {
			scope = nil
			s.mu.Lock()
			s.stopWatchesLocked(id)
			s.mu.Unlock()
			return err
		}
		interval = scope.pollInterval
		if scope.tokens != nil {
			scope.retryAt, err = s.Routing().ProviderRetryUntil(ctx, owner, id)
			if err != nil {
				return err
			}
			if scope.retryAt.After(time.Now()) {
				return fmt.Errorf("provider retry is deferred until %s", scope.retryAt.Format(time.RFC3339))
			}
		}
		var mutationErr error
		mutationErr = s.replayLabelMutations(ctx, scope)
		if mutationErr != nil && scope.retryAt.After(time.Now()) {
			queueFailed = true
			return mutationErr
		}
		mutationErr = errors.Join(mutationErr, s.replayMutations(ctx, scope))
		if queue != nil && !scope.retryAt.After(time.Now()) {
			var providerMail *UserProviderMail
			if scope.tokens != nil {
				providerMail = &UserProviderMail{scope: scope, lifetime: ctx}
			}
			mutationErr = errors.Join(mutationErr, queue.Run(ctx, owner, id, providerMail))
		}
		queueFailed = mutationErr != nil
		if mutationErr != nil && scope.retryAt.After(time.Now()) {
			return mutationErr
		}
		var enabled int
		if err := scope.call(ctx, func(db *storage.DB) error {
			return db.Read().QueryRowContext(ctx, `SELECT COALESCE(email_sync_enabled,1) FROM accounts WHERE id=?`, id).Scan(&enabled)
		}); err != nil {
			return err
		}
		if enabled != 1 {
			s.mu.Lock()
			s.stopWatchesLocked(id)
			s.mu.Unlock()
			if progress, ok := ctx.Value(accountSyncProgressScopeKey{}).(accountSyncProgressScope); ok && progress.kind == string(accountSyncManual) {
				return errUserIMAPDisabled
			}
			return mutationErr
		}
		// Fresh orchestration state for one bounded session, sharing only services.
		o := NewSyncOrchestrator(nil, nil, s.blobs, scope.tokens)
		o.imapScope = scope
		o.events = s.events
		if repair {
			err = o.repairGmailAPIAccount(ctx, id)
		} else {
			err = o.syncAccount(ctx, id, true)
		}
		receiveErr = err
		if err == nil {
			err = s.reconcileWatches(ctx, scope)
		}
		err = errors.Join(err, mutationErr)
		if ctx.Err() == nil {
			statusErr := scope.call(ctx, func(db *storage.DB) error {
				if err != nil {
					return db.MarkEmailSyncError(ctx, id, err.Error(), time.Now().UTC())
				}
				return db.ClearEmailSyncError(ctx, id)
			})
			err = errors.Join(err, statusErr)
			payload := map[string]any{"status": "ok"}
			if err != nil {
				payload["status"] = "error"
				payload["error"] = err.Error()
			}
			o.publishEvent(Event{Type: EventAccountSyncStatus, AccountID: id, Payload: o.accountSyncStatusPayload(ctx, id, "", payload)})
		}
		return err
	})
}

// EnsureBody fetches and persists synchronously, so errors are visible and retry
// never mistakes a partial cache for success. The account gate coalesces readers.
func (s *UserIMAP) EnsureBody(ctx context.Context, owner string, msgID int64) error {
	return s.ensureBody(ctx, owner, msgID, false)
}

// RefetchBody bypasses saved MIME and replaces the cache only after a complete,
// verified fetch. Failed refreshes leave the readable body and attachments intact.
func (s *UserIMAP) RefetchBody(ctx context.Context, owner string, msgID int64) error {
	return s.ensureBody(ctx, owner, msgID, true)
}

func (s *UserIMAP) ensureBody(ctx context.Context, owner string, msgID int64, force bool) error {
	release, err := s.blobs.PinUserFiles(ctx, owner)
	if err != nil {
		return err
	}
	defer release()
	var id string
	err = s.Routing().WithUser(ctx, owner, func(db *storage.DB) error {
		info, err := db.GetMessageStorageInfoForUser(ctx, msgID, owner)
		if err != nil {
			return err
		}
		if info == nil {
			return sql.ErrNoRows
		}
		id = info.AccountID
		return nil
	})
	if err != nil {
		return err
	}
	return s.operation(ctx, owner, id, msgID, 2*time.Minute, func(ctx context.Context) error {
		var fetched bool
		var info *storage.MessageFetchInfo
		var rawPath string
		var validity uint32
		var providerID string
		err := s.Routing().WithAccountForUser(ctx, owner, id, func(db *storage.DB) error {
			stored, err := db.GetMessageStorageInfoForUser(ctx, msgID, owner)
			if err != nil {
				return err
			}
			if stored == nil || stored.AccountID != id {
				return sql.ErrNoRows
			}
			rawPath = stored.RawPath
			fetched = !force && db.IsBodyFetchedInternal(ctx, msgID)
			if fetched {
				return nil
			}
			provider, err := db.GetMessageMutationInfoForUser(ctx, msgID, owner)
			if err != nil {
				return err
			}
			if provider == nil || provider.AccountID != id {
				return sql.ErrNoRows
			}
			if provider.AccountProvider == providers.ProviderGmail || provider.AccountProvider == providers.ProviderOutlook {
				providerID = provider.RemoteMessageID
				if providerID == "" {
					return errors.New("provider message identity is unavailable")
				}
				return nil
			}
			info, err = db.GetMessageFetchInfoForUser(ctx, msgID, owner)
			if err != nil {
				return err
			}
			if info == nil || info.AccountID != id {
				return sql.ErrNoRows
			}
			return db.Read().QueryRowContext(ctx, `SELECT uid_validity FROM folders WHERE account_id=? AND remote_id=?`, id, info.FolderRemoteID).Scan(&validity)
		})
		if err != nil || fetched {
			return err
		}
		scope, err := s.snapshot(ctx, owner, id)
		if err != nil {
			return err
		}
		var raw []byte
		if rawPath != "" && !force {
			raw, _ = os.ReadFile(rawPath)
		}
		if len(raw) == 0 {
			if scope.config.Provider == providers.ProviderOutlook {
				raw, err = s.fetchOutlookRaw(ctx, scope, providerID)
				if err != nil {
					return err
				}
			} else if scope.config.Provider == providers.ProviderGmail {
				raw, err = s.fetchGmailRaw(ctx, scope, providerID)
				if err != nil {
					return err
				}
			} else {
				if info == nil || validity == 0 || info.RemoteUID == 0 {
					return errors.New("message has no verified IMAP body identity")
				}
				client, err := imap.NewContextClient(ctx, scope.config, scope.password)
				if err != nil {
					return err
				}
				defer client.Close()
				raw, err = client.FetchBodyWithValidity(ctx, info.FolderRemoteID, info.RemoteUID, validity)
				if err != nil {
					return err
				}
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		candidate, err := s.blobs.NewMessageVersion()
		if err != nil {
			return err
		}
		published := false
		defer func() {
			if !published {
				if err := candidate.DeleteMessage(id, msgID); err != nil {
					log.Printf("discard IMAP body candidate: %v", err)
				}
			}
		}()
		parsed, err := message.ParseMessage(ctx, bytes.NewReader(raw), candidate, id, msgID)
		if err != nil {
			return err
		}
		cache, err := prepareUserIMAPBody(ctx, candidate, id, msgID, parsed)
		cache.FetchInfo, cache.UIDValidity = info, validity
		if scope.config.Provider == providers.ProviderGmail || scope.config.Provider == providers.ProviderOutlook {
			cache.ProviderMessageID, cache.ProviderAccountID, cache.ProviderType = providerID, scope.config.ProviderAccountID, scope.config.Provider
		}
		if err != nil {
			return err
		}
		err = scope.call(ctx, func(db *storage.DB) error {
			if scope.config.Provider == providers.ProviderGmail || scope.config.Provider == providers.ProviderOutlook {
				return db.SaveMessageBodyCache(ctx, msgID, id, cache)
			}
			current, err := db.GetMessageFetchInfoForUser(ctx, msgID, owner)
			if err != nil {
				return err
			}
			if current == nil || *current != *info {
				return errors.New("message identity changed during body fetch")
			}
			var currentValidity uint32
			if err := db.Read().QueryRowContext(ctx, `SELECT uid_validity FROM folders WHERE account_id=? AND remote_id=?`, id, info.FolderRemoteID).Scan(&currentValidity); err != nil {
				return err
			}
			if currentValidity != validity {
				return errors.New("folder identity changed during body fetch")
			}
			return db.SaveMessageBodyCache(ctx, msgID, id, cache)
		})
		published = err == nil
		return err
	})
}
func prepareUserIMAPBody(ctx context.Context, blobs *store.BlobStore, id string, msgID int64, p *message.ParsedMessage) (storage.MessageBodyCache, error) {
	c := storage.MessageBodyCache{Parsed: p}
	var err error
	if p.TextBody != "" || len(p.HTMLBody) == 0 {
		c.TextPath, err = blobs.StoreBodyText(ctx, id, msgID, []byte(p.TextBody))
		if err != nil {
			return c, err
		}
	}
	cid := map[string]string{}
	for _, a := range p.Attachments {
		if a.Inline && a.ContentID != "" {
			cid[a.ContentID] = fmt.Sprintf("/api/inline-content/%d/%s", msgID, url.PathEscape(a.ContentID))
		}
	}
	if len(p.HTMLBody) > 0 {
		c.OriginalHTMLPath, err = blobs.StoreBodyOriginalHTML(ctx, id, msgID, p.HTMLBody)
		if err != nil {
			return c, err
		}
		c.HTMLPath, err = blobs.StoreBodyHTML(ctx, id, msgID, message.RewriteCIDReferences(message.SanitizeHTML(p.HTMLBody), cid))
		if err != nil {
			return c, err
		}
	}
	return c, nil
}

// Cleanup is invoked only after routing has drained active sessions. Queued jobs
// subsequently fail directory validation and cannot recreate account blobs.
func (s *UserIMAP) Cleanup(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	credentials := s.credentials
	s.mu.Unlock()
	if credentials != nil {
		if err := credentials.CleanupAccount(ctx, id); err != nil {
			return err
		}
	}
	return s.blobs.DeleteAccount(id)
}

// SetCredentials installs account-bound provider tokens and central credential
// cleanup before work starts.
func (s *UserIMAP) SetCredentials(credentials *mailauth.UserCredentials) error {
	if credentials == nil || credentials.Routing() != s.Routing() {
		return errors.New("mailbox credentials must use the same account router")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.credentials != nil || s.background || len(s.gates) != 0 || len(s.queued) != 0 || s.closing || s.ctx.Err() != nil {
		return errors.New("mailbox credentials must be configured once before IMAP work starts")
	}
	s.credentials = credentials
	return nil
}

func (s *UserIMAP) Accounts() *config.UserAccountStore { return s.accounts }
func (s *UserIMAP) Events() *EventBus                  { return s.events }
