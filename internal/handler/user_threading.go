package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// UserThreadingStatus preserves the processing API's original fields. Failures
// remain visible instead of reporting an incomplete startup repair as success.
type UserThreadingStatus struct {
	storage.ThreadingState
	LastError   string `json:"last_error,omitempty"`
	FailedUsers int    `json:"failed_users,omitempty"`
}

type userThreadingWorker struct {
	h          *Handler
	mu         sync.RWMutex
	state      UserThreadingStatus
	firstError error
	cancel     context.CancelFunc
	done       chan struct{}
}

// StartUserThreading performs one trusted sweep at managed startup. Only this
// worker snapshots
// message IDs, one owner at a time; discovery copies at most 64 central IDs.
// Provider startup may await completion through AwaitUserThreading, preserving
// the original threading-before-receive ordering. Runtime shutdown joins it.
func (h *Handler) StartUserThreading(ctx context.Context) error {
	if h.ownedMailbox != nil {
		return h.ownedMailbox.StartUserThreading(ctx)
	}
	if ctx == nil || h.userStorage == nil {
		return errors.New("startup threading requires owned routing and lifecycle")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h.userThreadingMu.Lock()
	defer h.userThreadingMu.Unlock()
	if h.userThreadingWorker != nil {
		return errors.New("startup threading already started")
	}
	workCtx, cancel := context.WithCancel(ctx)
	w := &userThreadingWorker{h: h, state: UserThreadingStatus{ThreadingState: storage.ThreadingState{InProgress: true}}, cancel: cancel, done: make(chan struct{})}
	if h.userIMAP != nil {
		if err := h.userIMAP.StartBackgroundService(workCtx, w.run); err != nil {
			cancel()
			return err
		}
	} else {
		go w.run(workCtx)
	}
	h.userThreadingWorker = w
	return nil
}

func (h *Handler) threadingWorker() *userThreadingWorker {
	if h.ownedMailbox != nil {
		return h.ownedMailbox.threadingWorker()
	}
	h.userThreadingMu.Lock()
	defer h.userThreadingMu.Unlock()
	return h.userThreadingWorker
}
func (h *Handler) getUserThreadingStatus() UserThreadingStatus {
	w := h.threadingWorker()
	if w == nil {
		return UserThreadingStatus{}
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.state
}

func (h *Handler) AwaitUserThreading(ctx context.Context) error {
	w := h.threadingWorker()
	if w == nil {
		return errors.New("startup threading has not started")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.firstError
}
func (h *Handler) WaitUserThreading() {
	w := h.threadingWorker()
	if w != nil {
		w.cancel()
		<-w.done
	}
}

func (w *userThreadingWorker) update(fn func(*UserThreadingStatus)) {
	w.mu.Lock()
	fn(&w.state)
	state := w.state
	w.mu.Unlock()
	if w.h.syncer != nil {
		w.h.syncer.Events().Publish(mail.Event{Type: mail.EventProcessingStatus, AdminOnly: true, Payload: map[string]any{"in_progress": state.InProgress, "processed": state.Processed, "total": state.Total, "last_error": state.LastError, "failed_users": state.FailedUsers}})
	}
}
func (w *userThreadingWorker) fail(err error, userFailure bool) {
	w.mu.Lock()
	if w.firstError == nil {
		w.firstError = err
		w.state.LastError = err.Error()
	}
	if userFailure {
		w.state.FailedUsers++
	}
	w.mu.Unlock()
	log.Printf("storage: owned startup threading: %v", err)
	w.update(func(*UserThreadingStatus) {})
}
func (w *userThreadingWorker) run(ctx context.Context) {
	defer close(w.done)
	defer w.cancel()
	defer w.update(func(s *UserThreadingStatus) { s.InProgress = false })
	cursor := ""
	for {
		owners, err := w.h.userStorage.ListStartupStoreOwners(ctx, cursor, 64)
		if err != nil {
			w.fail(err, false)
			return
		}
		if len(owners) == 0 {
			return
		}
		for _, owner := range owners {
			if err := ctx.Err(); err != nil {
				w.fail(err, false)
				return
			}
			w.mu.RLock()
			beforeProcessed, beforeTotal := w.state.Processed, w.state.Total
			w.mu.RUnlock()
			err := w.h.userStorage.EnsureUserThreadingForStartup(ctx, owner, func(state storage.ThreadingState) {
				w.update(func(s *UserThreadingStatus) {
					s.Processed = beforeProcessed + state.Processed
					s.Total = beforeTotal + state.Total
				})
			})
			if ctx.Err() != nil {
				w.fail(ctx.Err(), false)
				return
			}
			if err != nil {
				w.fail(fmt.Errorf("repair threading for user %s: %w", owner, err), true)
			}
			cursor = owner
		}
	}
}
