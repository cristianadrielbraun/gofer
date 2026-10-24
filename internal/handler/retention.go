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
	// Files changed this recently stay, so ones still being published do.
	messageFileRetention = 24 * time.Hour
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
		total.FolderStates += pruned.FolderStates
		total.Messages += pruned.Messages
		total.Threads += pruned.Threads
		if pruned.Total() == 0 {
			break
		}
	}
	h.cleanupMessageFiles(ctx)

	h.recordMailRetention(now, total, lastError)
}

// cleanupMessageFiles removes downloaded files nothing refers to any more, as
// managed installations do for each user (mail.UserIMAP.CleanupUserFiles).
func (h *Handler) cleanupMessageFiles(ctx context.Context) {
	if h.blobStore == nil {
		return
	}
	work, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := h.db.WithBlobReferences(work, func(refs storage.BlobReferences) error {
		result, err := h.blobStore.CleanupMessageFiles(work, refs.Accounts, refs.Paths, refs.Messages, messageFileRetention)
		if result.Versions+result.Messages > 0 {
			log.Printf("mail-retention: removed files of %d messages and %d old versions", result.Messages, result.Versions)
		}
		return err
	})
	if err != nil && ctx.Err() == nil {
		log.Printf("mail-retention: clean up message files: %v", err)
	}
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
			FolderStates:        total.FolderStates,
			Messages:            total.Messages,
			Threads:             total.Threads,
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
