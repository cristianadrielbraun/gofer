package handler

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type UserContactSyncOptions struct {
	ScanInterval     time.Duration
	RecoveryInterval time.Duration
}

type userContactJob struct{ owner, account string }

type userContactWorkers struct {
	h       *Handler
	options UserContactSyncOptions
	queue   chan userContactJob
	wake    chan struct{}
	mu      sync.Mutex
	pending map[userContactJob]bool
	rescan  bool
	done    chan struct{}
}

// StartUserContactSync is opt-in until the complete verified storage layout is
// activated. Two workers share a queue of 32 IDs; contacts protocol slots remain
// independent of mail and calendar. Discovery reads central metadata only.
func (h *Handler) StartUserContactSync(ctx context.Context, options UserContactSyncOptions) error {
	if ctx == nil || h.userIMAP == nil || h.userAccounts == nil || h.userStorage == nil {
		return errors.New("owned contact scheduling requires lifecycle, accounts and routing")
	}
	if options.ScanInterval < 0 || options.RecoveryInterval < 0 {
		return errors.New("contact scheduling intervals must be nonnegative")
	}
	if options.ScanInterval == 0 {
		options.ScanInterval = time.Minute
	}
	if options.RecoveryInterval == 0 {
		options.RecoveryInterval = 5 * time.Minute
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h.contactSyncMu.Lock()
	defer h.contactSyncMu.Unlock()
	if h.userContactWorkers != nil {
		return errors.New("owned contact scheduling already started")
	}
	if h.contactSyncRunning == nil {
		h.contactSyncRunning = make(map[string]struct{})
	}
	if err := h.userStorage.ResetContactSchedulesForStartup(ctx); err != nil {
		return err
	}
	w := &userContactWorkers{h: h, options: options, queue: make(chan userContactJob, 32), wake: make(chan struct{}, 1), pending: make(map[userContactJob]bool), done: make(chan struct{})}
	if err := h.userIMAP.StartBackgroundService(ctx, w.run); err != nil {
		return err
	}
	h.userContactWorkers = w
	return nil
}

func (h *Handler) signalUserContacts() {
	h.contactSyncMu.Lock()
	w := h.userContactWorkers
	h.contactSyncMu.Unlock()
	if w != nil {
		w.signal(true)
	}
}

// WakeUserContactQueue runs after the local transaction and lease have ended.
// If central metadata cannot be updated, the durable local operation survives
// and the bounded recovery probe will rediscover it.
func (h *Handler) WakeUserContactQueue(ctx context.Context, owner string) error {
	if h.userStorage == nil {
		return errors.New("owned contact routing is unavailable")
	}
	err := h.userStorage.ResetContactQueue(ctx, owner)
	if err == nil {
		h.signalUserContacts()
	}
	return err
}

func (h *Handler) WakeUserContactAccount(ctx context.Context, owner, account string) error {
	if h.userStorage == nil {
		return errors.New("owned contact routing is unavailable")
	}
	err := h.userStorage.ResetServiceAccounts(ctx, owner, account, storage.ScheduledContacts)
	if err == nil {
		h.signalUserContacts()
	}
	return err
}

// Lifecycle hooks receive a trusted globally unique account ID. Resolve it
// through the existing routing facade and release the lease before the wake.
func (h *Handler) wakeUserContactAccountID(ctx context.Context, account string) error {
	var owner string
	if err := h.userStorage.WithAccount(ctx, account, func(_ *storage.DB, user string) error { owner = user; return nil }); err != nil {
		return err
	}
	return h.WakeUserContactAccount(ctx, owner, account)
}

func (p *userContactPull) wakeFanout(ctx context.Context, results []storage.InboundContactResult) {
	for _, result := range results {
		if result.FanoutOperationID != "" {
			if err := p.h.WakeUserContactQueue(ctx, p.snapshot.OwnerID()); err != nil && ctx.Err() == nil {
				log.Printf("owned contacts: wake committed fanout: %v", err)
			}
			return
		}
	}
}

func (w *userContactWorkers) signal(rescan bool) {
	if rescan {
		w.mu.Lock()
		w.rescan = true
		w.mu.Unlock()
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *userContactWorkers) enqueue(job userContactJob) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending[job] {
		return true
	}
	select {
	case w.queue <- job:
		w.pending[job] = true
		return true
	default:
		return false
	}
}

func (w *userContactWorkers) run(ctx context.Context) {
	defer close(w.done)
	var workers sync.WaitGroup
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-w.queue:
					w.process(ctx, job)
					w.mu.Lock()
					delete(w.pending, job)
					w.mu.Unlock()
					w.signal(false)
				}
			}
		}()
	}
	defer workers.Wait()
	ticker := time.NewTicker(w.options.ScanInterval)
	defer ticker.Stop()
	accountCursor, ownerCursor := "", ""
	accountsDone, ownersDone := false, false
	scanning := true
	accountFirst := false
	for {
		if ctx.Err() != nil {
			return
		}
		if scanning {
			progress := false
			var owners []string
			var accounts []storage.AccountRoute
			if !ownersDone {
				var err error
				owners, err = w.h.userStorage.ListDueContactQueueOwners(ctx, ownerCursor, time.Now(), 16, false)
				if err != nil {
					w.report(ctx, "discover outbound queues", err)
					ownersDone = true
				} else if len(owners) == 0 {
					ownersDone = true
				}
			}
			if !accountsDone {
				var err error
				accounts, err = w.h.userStorage.ListDueServiceAccounts(ctx, storage.ScheduledContacts, accountCursor, time.Now(), 16, false)
				if err != nil {
					w.report(ctx, "discover provider pulls", err)
					accountsDone = true
				} else if len(accounts) == 0 {
					accountsDone = true
				}
			}
			// Interleave both kinds, alternating the first kind when the queue
			// has only one free slot. An idle-owner backlog must not starve pulls.
			jobs := make([]userContactJob, 0, len(owners)+len(accounts))
			for i := 0; i < max(len(owners), len(accounts)); i++ {
				var pair []userContactJob
				if i < len(owners) {
					pair = append(pair, userContactJob{owner: owners[i]})
				}
				if i < len(accounts) {
					pair = append(pair, userContactJob{owner: accounts[i].UserID, account: accounts[i].AccountID})
				}
				if accountFirst && len(pair) == 2 {
					pair[0], pair[1] = pair[1], pair[0]
				}
				jobs = append(jobs, pair...)
			}
			accountFirst = !accountFirst
			for _, job := range jobs {
				if !w.enqueue(job) {
					break
				}
				if job.account == "" {
					ownerCursor = job.owner
				} else {
					accountCursor = job.account
				}
				progress = true
			}
			if accountsDone && ownersDone {
				w.mu.Lock()
				rescan := w.rescan
				w.rescan = false
				w.mu.Unlock()
				scanning = false
				if rescan {
					accountCursor, ownerCursor, accountsDone, ownersDone, scanning = "", "", false, false, true
					w.signal(false)
				}
			} else if progress {
				w.signal(false)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !scanning {
				accountCursor, ownerCursor, accountsDone, ownersDone, scanning = "", "", false, false, true
			}
		case <-w.wake:
			// Finish an ongoing page pass before restarting. Frequent saves
			// cannot continually reset the cursor and starve later owners.
			if !scanning {
				w.mu.Lock()
				w.rescan = false
				w.mu.Unlock()
				accountCursor, ownerCursor, accountsDone, ownersDone, scanning = "", "", false, false, true
			}
		}
	}
}

func (w *userContactWorkers) report(ctx context.Context, action string, err error) {
	if err != nil && ctx.Err() == nil {
		log.Printf("owned contacts: %s: %v", action, err)
	}
}

func (w *userContactWorkers) process(parent context.Context, job userContactJob) {
	ctx, cancel := context.WithTimeout(parent, 4*time.Minute)
	defer cancel()
	now := time.Now()
	if job.account == "" {
		revision, reserved, err := w.h.userStorage.ReserveContactQueue(ctx, job.owner, now, now.Add(userContactClaimTimeout), false)
		if err != nil || !reserved {
			w.report(ctx, "reserve outbound queue", err)
			return
		}
		_, processErr := w.h.ProcessUserContactSyncOperations(ctx, job.owner, 5)
		w.report(ctx, "process outbound queue", processErr)
		if ctx.Err() != nil {
			return
		}
		next, err := w.h.userAccounts.NextContactSyncAttempt(ctx, job.owner, userContactClaimTimeout)
		if err != nil {
			w.report(ctx, "read outbound deadline", err)
			return
		}
		// A bounded recovery probe catches a local commit followed by a failed
		// central wake, including after restart. The local queue is authoritative.
		recovery := time.Now().Add(w.options.RecoveryInterval)
		if next.IsZero() || next.After(recovery) {
			next = recovery
		}
		updated, err := w.h.userStorage.CompleteContactQueue(ctx, job.owner, revision, next)
		w.report(ctx, "defer outbound queue", err)
		if err == nil && (!updated || !next.After(time.Now())) {
			w.signal(true)
		}
		return
	}
	revision, reserved, err := w.h.userStorage.ReserveServiceAccount(ctx, job.owner, job.account, storage.ScheduledContacts, now, now.Add(3*time.Minute), false)
	if err != nil || !reserved {
		w.report(ctx, "reserve provider pull", err)
		return
	}
	_, pullErr := w.h.SyncUserContactAccount(ctx, job.owner, job.account)
	w.report(ctx, "pull provider contacts", pullErr)
	if ctx.Err() != nil {
		return
	}
	interval := 5 * time.Minute
	err = w.h.userAccounts.WithUser(ctx, job.owner, func(_ *config.AccountStore, db *storage.DB) error {
		if minutes := db.GetSyncInterval(ctx, job.owner); minutes > 0 {
			interval = time.Duration(minutes) * time.Minute
		}
		return nil
	})
	if err != nil {
		w.report(ctx, "read contact cadence", err)
		return
	}
	next := time.Now().Add(interval)
	if pullErr != nil {
		var hint interface{ RetryAfter() (time.Time, bool) }
		if errors.As(pullErr, &hint) {
			if at, ok := hint.RetryAfter(); ok && at.After(next) {
				next = at
			}
		}
	}
	updated, err := w.h.userStorage.CompleteServiceAccount(ctx, job.owner, job.account, storage.ScheduledContacts, revision, next)
	w.report(ctx, "defer provider pull", err)
	if err == nil && !updated {
		w.signal(true)
	}
}
