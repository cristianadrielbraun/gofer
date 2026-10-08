package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

var errUserRemovalBusy = errors.New("user cleanup queue is full")

type userRemovalJob struct{ owner, actorSession, account string }

func (j userRemovalJob) key() string {
	if j.account != "" {
		return "account:" + j.account
	}
	return "user:" + j.owner
}

type userRemovalWorker struct {
	h             *Handler
	ctx           context.Context
	cancel        context.CancelFunc
	queue         chan userRemovalJob
	wake          chan struct{}
	done          chan struct{}
	mu            sync.Mutex
	closed        bool
	pending       map[string]struct{}
	after         string // recovery cursor, accessed only by run
	accountAfter  string
	accountsFirst bool
}

// StartUserDeletionCleanup starts one bounded dispatcher/cleanup worker under
// the IMAP root lifecycle. Only durable confirmed intent authorizes its work;
// queued jobs hold no database leases. Startup and periodic pages recover jobs
// dropped by queue saturation, process failure or shutdown. No per-user worker
// or unbounded retained history is created.
func (h *Handler) StartUserDeletionCleanup(ctx context.Context) error {
	if h.ownedMailbox != nil {
		return h.ownedMailbox.StartUserDeletionCleanup(ctx)
	}
	if ctx == nil || h.userStorage == nil || h.userIMAP == nil || h.auth == nil || h.blobStore == nil {
		return errors.New("user deletion requires the complete owned lifecycle runtime")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h.userRemovalMu.Lock()
	defer h.userRemovalMu.Unlock()
	if h.userRemoval != nil {
		if err := h.userRemoval.ctx.Err(); err != nil {
			return err
		}
		return nil
	}
	if err := h.auth.SetUserStorage(h.userStorage); err != nil {
		return err
	}
	work, cancel := context.WithCancel(ctx)
	w := &userRemovalWorker{h: h, ctx: work, cancel: cancel, queue: make(chan userRemovalJob, 32), wake: make(chan struct{}, 1), done: make(chan struct{}), pending: make(map[string]struct{})}
	if err := h.userIMAP.StartBackgroundService(work, w.run); err != nil {
		cancel()
		return err
	}
	h.userRemoval = w
	return nil
}

func (h *Handler) removalWorker() *userRemovalWorker {
	if h.ownedMailbox != nil {
		return h.ownedMailbox.removalWorker()
	}
	h.userRemovalMu.Lock()
	defer h.userRemovalMu.Unlock()
	return h.userRemoval
}

func (h *Handler) wakeUserDeletionRecovery() {
	if w := h.removalWorker(); w != nil {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

// WaitUserDeletions also supports a separately canceled application component.
// Normal root shutdown joins this worker through UserIMAP.Wait.
func (h *Handler) WaitUserDeletions() {
	if w := h.removalWorker(); w != nil {
		w.cancel()
		<-w.done
	}
}

func (w *userRemovalWorker) enqueue(job userRemovalJob) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.ctx.Err() != nil {
		return context.Canceled
	}
	if _, exists := w.pending[job.key()]; exists {
		return nil
	}
	select {
	case w.queue <- job:
		w.pending[job.key()] = struct{}{}
		return nil
	default:
		return errUserRemovalBusy
	}
}

func (w *userRemovalWorker) recoverPage(ctx context.Context) {
	// Alternate discovery priority so neither durable queue monopolizes capacity.
	if w.accountsFirst {
		w.recoverAccounts(ctx)
		w.recoverUsers(ctx)
	} else {
		w.recoverUsers(ctx)
		w.recoverAccounts(ctx)
	}
	w.accountsFirst = !w.accountsFirst
}

func (w *userRemovalWorker) recoverUsers(ctx context.Context) {
	ids, err := w.h.auth.PendingUserDeletionPage(ctx, w.after, 64)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("user deletion recovery: %v", err)
		}
		return
	}
	if len(ids) == 0 {
		w.after = ""
		return
	}
	for _, owner := range ids {
		if err := w.enqueue(userRemovalJob{owner: owner}); err != nil {
			return // retry this cursor; durable intent remains authoritative
		}
		w.after = owner
	}
}

func (w *userRemovalWorker) recoverAccounts(ctx context.Context) {
	entries, err := w.h.userStorage.PendingAccountCleanupPage(ctx, w.accountAfter, 64)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("account deletion recovery: %v", err)
		}
		return
	}
	if len(entries) == 0 {
		w.accountAfter = ""
		return
	}
	for _, entry := range entries {
		if err := w.enqueue(userRemovalJob{owner: entry.UserID, account: entry.AccountID}); err != nil {
			return
		}
		w.accountAfter = entry.AccountID
	}
}

func (w *userRemovalWorker) run(ctx context.Context) {
	defer close(w.done)
	defer w.cancel()
	defer func() {
		w.mu.Lock()
		w.closed = true
		clear(w.pending)
		for len(w.queue) > 0 {
			<-w.queue
		}
		w.mu.Unlock()
	}()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	w.recoverPage(ctx)
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.recoverPage(ctx)
		case <-w.wake:
			w.recoverPage(ctx)
		case job := <-w.queue:
			work, cancel := context.WithTimeout(ctx, 30*time.Minute)
			var err error
			if job.account != "" {
				err = w.h.userAccounts.DeleteAccount(work, job.owner, job.account, w.h.userIMAP.Cleanup)
			} else {
				err = w.h.cleanupOwnedDeletingUser(work, job.owner, job.actorSession)
			}
			cancel()
			w.mu.Lock()
			delete(w.pending, job.key())
			w.mu.Unlock()
			if err != nil && ctx.Err() == nil {
				log.Printf("owned %s deletion remains pending: %v", job.key(), err)
			}
		}
	}
}

func (h *Handler) cleanupOwnedDeletingUser(ctx context.Context, owner, actorSession string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pending, err := h.userStorage.RecoverPendingUserDeletion(ctx, owner)
	if err != nil || !pending {
		return err
	}
	h.userIMAP.StopUser(owner)
	accounts := func(fn func(string) error) error {
		after := ""
		for {
			entries, err := h.userStorage.PendingUserAccounts(ctx, owner, after, 64)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				return nil
			}
			for _, entry := range entries {
				if err := fn(entry.AccountID); err != nil {
					return fmt.Errorf("user account %s cleanup: %w", entry.AccountID, err)
				}
				after = entry.AccountID
			}
		}
	}
	// Account work can acquire file pins; drain it before exclusive file access.
	if err := accounts(func(id string) error { return h.userStorage.DrainPendingUserAccount(ctx, owner, id) }); err != nil {
		return err
	}
	release, err := h.blobStore.BeginUserFileRemoval(ctx, owner)
	if err != nil {
		return err
	}
	defer release()
	if err := h.userStorage.RemovePendingUserStore(ctx, owner); err != nil {
		return err
	}
	if err := accounts(func(id string) error {
		return h.userStorage.RemovePendingUserAccount(ctx, owner, id, func(ctx context.Context) error { return h.userIMAP.Cleanup(ctx, id) })
	}); err != nil {
		return err
	}
	if err := h.userStorage.CompletePendingUserCleanup(ctx, owner, func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := h.blobStore.DeleteComposeAttachments(owner); err != nil {
			return err
		}
		return h.blobStore.SyncUserRemoval(ctx)
	}); err != nil {
		return err
	}
	_, err = h.auth.CompleteAdministratorUserDeletion(ctx, owner, actorSession)
	return err
}
