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

type UserCalendarSyncOptions struct{ ScanInterval, RecoveryInterval time.Duration }
type userCalendarJob struct{ owner, account string }
type userCalendarWorkers struct {
	h       *Handler
	options UserCalendarSyncOptions
	queue   chan userCalendarJob
	wake    chan struct{}
	mu      sync.Mutex
	pending map[userCalendarJob]bool
	rescan  bool
	done    chan struct{}
}

// Central due hints discover bounded account pages. Only the two workers open
// local stores; no local lease survives a provider call or a dispatch wait.
func (h *Handler) StartUserCalendarSync(ctx context.Context, options UserCalendarSyncOptions) error {
	if ctx == nil || h.userIMAP == nil || h.userAccounts == nil || h.userStorage == nil {
		return errors.New("owned calendar scheduling requires lifecycle, accounts and routing")
	}
	if options.ScanInterval < 0 || options.RecoveryInterval < 0 {
		return errors.New("calendar scheduling intervals must be nonnegative")
	}
	if options.ScanInterval == 0 {
		options.ScanInterval = 5 * time.Second
	}
	if options.RecoveryInterval == 0 {
		options.RecoveryInterval = 5 * time.Minute
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h.calendarSyncMu.Lock()
	defer h.calendarSyncMu.Unlock()
	if h.userCalendarWorkers != nil {
		return errors.New("owned calendar scheduling already started")
	}
	if err := h.userStorage.ResetCalendarSchedulesForStartup(ctx); err != nil {
		return err
	}
	w := &userCalendarWorkers{h: h, options: options, queue: make(chan userCalendarJob, 32), wake: make(chan struct{}, 1), pending: make(map[userCalendarJob]bool), done: make(chan struct{})}
	if err := h.userIMAP.StartBackgroundService(ctx, w.run); err != nil {
		return err
	}
	h.userCalendarWorkers = w
	return nil
}

func (h *Handler) signalUserCalendar() {
	h.calendarSyncMu.Lock()
	w := h.userCalendarWorkers
	h.calendarSyncMu.Unlock()
	if w != nil {
		w.signal(true)
	}
}
func (h *Handler) WakeUserCalendarAccount(ctx context.Context, owner, account string) error {
	if h.userStorage == nil {
		return errors.New("owned calendar routing is unavailable")
	}
	err := h.userStorage.ResetServiceAccounts(ctx, owner, account, storage.ScheduledCalendar)
	if err == nil {
		h.signalUserCalendar()
	}
	return err
}
func (h *Handler) wakeUserCalendarAccountID(ctx context.Context, account string) error {
	var owner string
	if err := h.userStorage.WithAccount(ctx, account, func(_ *storage.DB, user string) error { owner = user; return nil }); err != nil {
		return err
	}
	return h.WakeUserCalendarAccount(ctx, owner, account)
}
func (w *userCalendarWorkers) signal(rescan bool) {
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
func (w *userCalendarWorkers) enqueue(job userCalendarJob) bool {
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
func (w *userCalendarWorkers) run(ctx context.Context) {
	defer close(w.done)
	var workers sync.WaitGroup
	for range 2 {
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
	cursor := ""
	scanning := true
	for {
		if ctx.Err() != nil {
			return
		}
		if scanning {
			accounts, err := w.h.userStorage.ListDueServiceAccounts(ctx, storage.ScheduledCalendar, cursor, time.Now(), 16, false)
			if err != nil {
				w.report(ctx, "discover due accounts", err)
				scanning = false
			} else if len(accounts) == 0 {
				w.mu.Lock()
				rescan := w.rescan
				w.rescan = false
				w.mu.Unlock()
				scanning = false
				if rescan {
					cursor = ""
					scanning = true
					w.signal(false)
				}
			} else {
				progress := false
				for _, account := range accounts {
					if !w.enqueue(userCalendarJob{owner: account.UserID, account: account.AccountID}) {
						break
					}
					cursor = account.AccountID
					progress = true
				}
				if progress {
					w.signal(false)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !scanning {
				cursor = ""
				scanning = true
			}
		case <-w.wake:
			if !scanning {
				w.mu.Lock()
				w.rescan = false
				w.mu.Unlock()
				cursor = ""
				scanning = true
			}
		}
	}
}
func (w *userCalendarWorkers) report(ctx context.Context, action string, err error) {
	if err != nil && ctx.Err() == nil {
		log.Printf("owned calendar: %s: %v", action, err)
	}
}
func (w *userCalendarWorkers) process(parent context.Context, job userCalendarJob) {
	ctx, cancel := context.WithTimeout(parent, 4*time.Minute)
	defer cancel()
	now := time.Now()
	revision, reserved, forced, err := w.h.userStorage.ReserveServiceAccountWithWake(ctx, job.owner, job.account, storage.ScheduledCalendar, now, now.Add(5*time.Minute), false)
	if err != nil || !reserved {
		w.report(ctx, "reserve due account", err)
		return
	}
	replyErr := w.h.runUserCalendarReplyFollowups(ctx, job.owner, job.account)
	w.report(ctx, "finish accepted replies", replyErr)
	if ctx.Err() != nil {
		return
	}
	cleanupErr := w.h.runUserCalendarMeetingCleanup(ctx, job.owner, job.account)
	w.report(ctx, "clean temporary meetings", cleanupErr)
	if ctx.Err() != nil {
		return
	}
	var start, end time.Time
	err = w.h.userAccounts.WithAccountForUser(ctx, job.owner, job.account, func(_ *config.AccountStore, db *storage.DB) error {
		start, end = calendarBackgroundWindow(now, viewsCalendarLocation(db.GetUISettings(ctx, job.owner)))
		return nil
	})
	if err != nil {
		w.report(ctx, "read background window", err)
		return
	}
	_, syncErr := w.h.syncUserCalendarWindow(ctx, job.owner, start, end, job.account, true, forced)
	if !errors.Is(syncErr, errCalendarAccountNotConfigured) {
		w.report(ctx, "refresh account", syncErr)
	}
	if ctx.Err() != nil {
		return
	}
	next := time.Now().Add(w.options.RecoveryInterval)
	err = w.h.userAccounts.WithAccountForUser(ctx, job.owner, job.account, func(_ *config.AccountStore, db *storage.DB) error {
		sources, err := db.ListSelectedCalendarSources(ctx, job.owner)
		if err != nil {
			return err
		}
		for _, source := range sources {
			if source.AccountID != job.account {
				continue
			}
			candidate := time.Now().Add(w.options.ScanInterval)
			if source.SyncState != "syncing" && source.NextAttemptAt != nil && source.NextAttemptAt.After(time.Now()) {
				candidate = *source.NextAttemptAt
			}
			if candidate.Before(next) {
				next = candidate
			}
		}
		pending, err := db.ListAccountCalendarReplyFollowups(ctx, job.owner, job.account, 1)
		if err != nil {
			return err
		}
		if len(pending) != 0 {
			retry := time.Now().Add(5 * time.Second)
			if retry.Before(next) {
				next = retry
			}
		}
		return nil
	})
	if err != nil {
		w.report(ctx, "read next source deadline", err)
		return
	}
	var hint interface{ RetryAfter() (time.Time, bool) }
	if errors.As(syncErr, &hint) {
		if at, ok := hint.RetryAfter(); ok && at.After(next) {
			next = at
		}
	}
	if errors.As(replyErr, &hint) {
		if at, ok := hint.RetryAfter(); ok && at.After(next) {
			next = at
		}
	}
	if errors.As(cleanupErr, &hint) {
		if at, ok := hint.RetryAfter(); ok && at.After(next) {
			next = at
		}
	}
	updated, err := w.h.userStorage.CompleteServiceAccount(ctx, job.owner, job.account, storage.ScheduledCalendar, revision, next)
	w.report(ctx, "defer account", err)
	if err == nil && !updated {
		w.signal(true)
	}
}
