package handler

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// UserCalendarSyncOptions: background calendar work runs on MinWorkers
// workers, and on more, up to MaxWorkers, while accounts wait their turn.
type UserCalendarSyncOptions struct {
	ScanInterval, RecoveryInterval time.Duration
	MinWorkers, MaxWorkers         int
}
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
	// workers counts running workers, idle those waiting for a job.
	workers, idle int
}

// calendarWorkerIdle is how long a worker added for a backlog waits for
// another job before it stops.
const calendarWorkerIdle = 30 * time.Second

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
	if options.MinWorkers <= 0 {
		options.MinWorkers = 2
	}
	if options.MaxWorkers <= 0 {
		options.MaxWorkers = 16
	}
	options.MaxWorkers = max(options.MaxWorkers, options.MinWorkers)
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
	go h.runUserCalendarReminders(ctx)
	if h.userIMAP.Events() != nil {
		go h.wakeUserCalendarOnNewMail(ctx, h.userIMAP.Events())
	}
	return nil
}

// New mail may hold an invitation for a CalDAV calendar: its account's
// calendar work runs now rather than at its next turn. Only the mail is read
// early; the calendar itself is still refreshed on its own schedule.
func (h *Handler) wakeUserCalendarOnNewMail(ctx context.Context, events *mail.EventBus) {
	subscription := events.SubscribeFor(mail.SubscribeOptions{All: true, Types: []mail.EventType{mail.EventNewMail}})
	defer subscription.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case <-subscription.Ready():
			woken := false
			seen := map[userCalendarJob]bool{}
			for _, event := range subscription.Take() {
				job := userCalendarJob{owner: event.UserID, account: event.AccountID}
				if job.owner == "" || job.account == "" || seen[job] || !calendarInvitationFolderRole(event.FolderRole) {
					continue
				}
				seen[job] = true
				if !h.userAccountHasCalDAVCalendar(ctx, job.owner, job.account) {
					continue
				}
				if err := h.userStorage.NudgeServiceAccount(ctx, job.owner, job.account, storage.ScheduledCalendar, time.Now()); err != nil {
					if ctx.Err() == nil && !errors.Is(err, storage.ErrAccountRoute) {
						log.Printf("owned calendar: wake for new mail: %v", err)
					}
					continue
				}
				woken = true
			}
			if woken {
				h.signalUserCalendar()
			}
		}
	}
}

// wakeUserCalendarOnline checks an owner's calendars as they open Gofer,
// rather than at their next turn. The checks themselves keep their pace.
func (h *Handler) wakeUserCalendarOnline(ctx context.Context, owner string) {
	h.calendarSyncMu.Lock()
	running := h.userCalendarWorkers != nil
	h.calendarSyncMu.Unlock()
	if !running {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := h.userStorage.NudgeServiceAccount(ctx, owner, "", storage.ScheduledCalendar, time.Now()); err != nil {
		log.Printf("owned calendar: wake for an owner who opened Gofer: %v", err)
		return
	}
	h.signalUserCalendar()
}

// calendarInvitationFolderRole reports whether invitations are read from
// mail in a folder of this role, as ListCalendarInvitationMessages does.
func calendarInvitationFolderRole(role string) bool {
	switch role {
	case "sent", "trash", "junk", "spam", "drafts":
		return false
	}
	return true
}

func (h *Handler) userAccountHasCalDAVCalendar(ctx context.Context, owner, account string) bool {
	found := false
	_ = h.userAccounts.WithAccountForUser(ctx, owner, account, func(_ *config.AccountStore, db *storage.DB) error {
		sources, err := db.ListSelectedCalendarSources(ctx, owner)
		for _, source := range sources {
			found = found || (source.AccountID == account && storage.CalendarSourceMailServer(source.Provider))
		}
		return err
	})
	return found
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

// work runs jobs until ctx ends; a worker added for a backlog also stops
// once it has waited calendarWorkerIdle for one. The caller counted it as
// running and idle.
func (w *userCalendarWorkers) work(ctx context.Context, extra bool) {
	var idle <-chan time.Time
	for {
		if extra {
			idle = time.After(calendarWorkerIdle)
		}
		select {
		case <-ctx.Done():
			w.mu.Lock()
			w.workers--
			w.idle--
			w.mu.Unlock()
			return
		case <-idle:
			w.mu.Lock()
			w.workers--
			w.idle--
			w.mu.Unlock()
			return
		case job := <-w.queue:
			w.mu.Lock()
			w.idle--
			w.mu.Unlock()
			w.process(ctx, job)
			w.mu.Lock()
			delete(w.pending, job)
			w.idle++
			w.mu.Unlock()
			w.signal(false)
		}
	}
}

// grow adds workers while more accounts wait than workers are free.
func (w *userCalendarWorkers) grow(start func(extra bool)) {
	w.mu.Lock()
	added := 0
	for len(w.queue) > w.idle && w.workers < w.options.MaxWorkers {
		w.workers++
		w.idle++
		added++
	}
	w.mu.Unlock()
	for range added {
		start(true)
	}
}

func (w *userCalendarWorkers) run(ctx context.Context) {
	defer close(w.done)
	var workers sync.WaitGroup
	start := func(extra bool) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w.work(ctx, extra)
		}()
	}
	w.mu.Lock()
	w.workers, w.idle = w.options.MinWorkers, w.options.MinWorkers
	w.mu.Unlock()
	for range w.options.MinWorkers {
		start(false)
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
					w.grow(start)
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
	// CalDAV and CardDAV servers are checked against the owner's lists.
	ctx, cancel := context.WithTimeout(withMailAllowlistOwner(parent, job.owner), 4*time.Minute)
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
	incomingErr := w.h.runUserCalendarIncoming(ctx, job.owner, job.account)
	w.report(ctx, "apply verified guest replies", incomingErr)
	if ctx.Err() != nil {
		return
	}
	invitationErr := w.h.runUserCalendarInvitations(ctx, job.owner, job.account)
	w.report(ctx, "add invitations from email", invitationErr)
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
	online := w.h.userIMAP.OwnerOnline(job.owner)
	var checkErr error
	if online && !forced {
		checkErr = w.h.checkUserCalendarChanges(ctx, job.owner, job.account, start, end)
		w.report(ctx, "check for changes", checkErr)
		if ctx.Err() != nil {
			return
		}
	}
	_, syncErr := w.h.syncUserCalendarWindow(ctx, job.owner, start, end, job.account, true, forced)
	if !errors.Is(syncErr, errCalendarAccountNotConfigured) {
		w.report(ctx, "refresh account", syncErr)
		if w.h.calendarListDue(job.account, time.Now(), calendarSourceGone(checkErr) || calendarSourceGone(syncErr)) {
			_, listErr := w.h.runUserCalendarDiscovery(ctx, job.owner, job.account, nil, nil, true)
			w.report(ctx, "refresh calendar list", listErr)
		}
	}
	if ctx.Err() != nil {
		return
	}
	next := time.Now().Add(w.options.RecoveryInterval)
	if online && !errors.Is(syncErr, errCalendarAccountNotConfigured) {
		next = time.Now().Add(calendarQuickCheckInterval)
	}
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
	incomingAt, err := w.h.userAccounts.NextCalendarIncomingAttempt(ctx, job.owner, job.account)
	if err != nil {
		w.report(ctx, "read incoming reply deadline", err)
		return
	}
	if !incomingAt.IsZero() {
		if minimum := time.Now().Add(5 * time.Second); incomingAt.Before(minimum) {
			incomingAt = minimum
		}
		if incomingAt.Before(next) {
			next = incomingAt
		}
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
	if errors.As(incomingErr, &hint) {
		if at, ok := hint.RetryAfter(); ok && at.After(next) {
			next = at
		}
	}
	if errors.As(checkErr, &hint) {
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
