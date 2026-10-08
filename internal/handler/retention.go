package handler

import (
	"context"
	"log"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const (
	mailRetentionInterval      = 6 * time.Hour
	mailRetentionMaxBatchesRun = 10
)

// StartMailRetentionWorker keeps the durable queue bounded for the lifetime
// of the application. It is deliberately separate from HTTP handlers so a
// quiet admin panel or a restarted browser cannot stop maintenance.
func (h *Handler) StartMailRetentionWorker(ctx context.Context) {
	if h.ownedMailbox != nil || h.userStorage != nil {
		if err := h.StartUserMailRetention(ctx, UserMailRetentionOptions{}); err != nil {
			log.Printf("mail-retention: start owned cleanup: %v", err)
		}
		return
	}
	go func() {
		h.runMailRetention(ctx)
		ticker := time.NewTicker(mailRetentionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.runMailRetention(ctx)
			}
		}
	}()
}

func (h *Handler) runMailRetention(ctx context.Context) {
	h.runMailRetentionAt(ctx, time.Now().UTC())
}

func (h *Handler) runMailRetentionAt(ctx context.Context, now time.Time) {
	if h.ownedMailbox != nil {
		h.ownedMailbox.runMailRetentionAt(ctx, now)
		return
	}
	if h.userStorage != nil {
		h.runUserMailRetentionAt(ctx, now, 30*time.Second)
		return
	}
	now = now.UTC()
	total := storage.DurableJobPruneResult{}
	lastError := ""
	for batch := 0; batch < mailRetentionMaxBatchesRun; batch++ {
		pruned, err := h.db.PruneDurableMailJobs(ctx, now, storage.RetentionBatchSize)
		if err != nil {
			lastError = "retention cleanup failed"
			if ctx.Err() == nil {
				log.Printf("mail-retention: prune durable jobs: %v", err)
			}
			break
		}
		total.OutgoingSends += pruned.OutgoingSends
		total.MessageMutations += pruned.MessageMutations
		total.IMAPDraftOperations += pruned.IMAPDraftOperations
		total.LabelMutations += pruned.LabelMutations
		if pruned.Total() == 0 {
			break
		}
	}

	h.recordMailRetention(now, total, lastError)
}

func (h *Handler) recordMailRetention(now time.Time, total storage.DurableJobPruneResult, lastError string) {
	h.retentionMu.Lock()
	h.retentionState = models.MailRetentionDiagnostics{
		LastRunAt: now,
		LastPruned: models.MailRetentionPruneCounts{
			OutgoingSends:       total.OutgoingSends,
			MessageMutations:    total.MessageMutations,
			IMAPDraftOperations: total.IMAPDraftOperations,
			LabelMutations:      total.LabelMutations,
		},
		LastError: lastError,
	}
	h.retentionMu.Unlock()
}

func (h *Handler) mailRetentionDiagnostics() models.MailRetentionDiagnostics {
	if h.ownedMailbox != nil {
		return h.ownedMailbox.mailRetentionDiagnostics()
	}
	h.retentionMu.RLock()
	defer h.retentionMu.RUnlock()
	return h.retentionState
}
