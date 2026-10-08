package handler

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type UserMailRetentionOptions struct{ Interval, UserTimeout time.Duration }
type userRetentionWorker struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// One joined sweep holds at most one short local lease and one bounded central
// owner page. Store capacity/writer waits expire per owner, so a busy store does
// not indefinitely block housekeeping for everybody else.
func (h *Handler) StartUserMailRetention(ctx context.Context, options UserMailRetentionOptions) error {
	if h.ownedMailbox != nil {
		return h.ownedMailbox.StartUserMailRetention(ctx, options)
	}
	if ctx == nil || h.userStorage == nil || h.userIMAP == nil {
		return errors.New("owned retention requires routing and runtime lifecycle")
	}
	if options.Interval < 0 || options.UserTimeout < 0 {
		return errors.New("retention intervals must be nonnegative")
	}
	if options.Interval == 0 {
		options.Interval = mailRetentionInterval
	}
	if options.UserTimeout == 0 {
		options.UserTimeout = 30 * time.Second
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h.retentionMu.Lock()
	defer h.retentionMu.Unlock()
	if h.userRetention != nil {
		return errors.New("owned retention already started")
	}
	work, cancel := context.WithCancel(ctx)
	w := &userRetentionWorker{ctx: work, cancel: cancel, done: make(chan struct{})}
	err := h.userIMAP.StartBackgroundService(work, func(ctx context.Context) {
		defer close(w.done)
		ticker := time.NewTicker(options.Interval)
		defer ticker.Stop()
		for {
			h.runUserMailRetentionAt(ctx, time.Now().UTC(), options.UserTimeout)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
	if err != nil {
		cancel()
		return err
	}
	h.userRetention = w
	return nil
}

func (h *Handler) WaitUserMailRetention() {
	if h.ownedMailbox != nil {
		h.ownedMailbox.WaitUserMailRetention()
		return
	}
	h.retentionMu.RLock()
	w := h.userRetention
	h.retentionMu.RUnlock()
	if w != nil {
		w.cancel()
		<-w.done
	}
}

func (h *Handler) runUserMailRetentionAt(ctx context.Context, now time.Time, timeout time.Duration) {
	total := storage.DurableJobPruneResult{}
	lastError := ""
	defer func() { h.recordMailRetention(now.UTC(), total, lastError) }()
	after := ""
	for {
		if ctx.Err() != nil {
			lastError = "retention cleanup interrupted"
			return
		}
		owners, err := h.userStorage.ListStartupStoreOwners(ctx, after, 64)
		if err != nil {
			lastError = "retention cleanup failed"
			if ctx.Err() == nil {
				log.Printf("owned mail-retention discovery: %v", err)
			}
			return
		}
		if len(owners) == 0 {
			return
		}
		for _, owner := range owners {
			work, cancel := context.WithTimeout(ctx, timeout)
			for range mailRetentionMaxBatchesRun {
				pruned, err := h.userStorage.PruneRetainedUserMailJobs(work, owner, now, storage.RetentionBatchSize)
				total.OutgoingSends += pruned.OutgoingSends
				total.MessageMutations += pruned.MessageMutations
				total.IMAPDraftOperations += pruned.IMAPDraftOperations
				total.LabelMutations += pruned.LabelMutations
				if err != nil {
					// Concurrent removal closes this owner's maintenance admission.
					if !errors.Is(err, storage.ErrUserStoreOwner) {
						lastError = "retention cleanup failed"
						if ctx.Err() == nil {
							log.Printf("owned mail-retention: %v", err)
						}
					}
					break
				}
				if pruned.Total() == 0 {
					break
				}
			}
			cancel()
			after = owner
			if ctx.Err() != nil {
				lastError = "retention cleanup interrupted"
				return
			}
		}
	}
}
