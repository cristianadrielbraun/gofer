package handler

import (
	"context"
	"log"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

const (
	authenticationEventRetentionInterval      = 6 * time.Hour
	authenticationEventRetentionMaxBatchesRun = 100
)

// StartAuthenticationEventRetentionWorker prunes old sign-in events and
// administration audit events at startup, periodically, and after an
// administrator changes a retention window.
func (h *Handler) StartAuthenticationEventRetentionWorker(ctx context.Context) {
	h.authEventRetentionWorkers.Add(1)
	go func() {
		defer h.authEventRetentionWorkers.Done()
		h.runAuthenticationEventRetention(ctx)
		ticker := time.NewTicker(authenticationEventRetentionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.runAuthenticationEventRetention(ctx)
			case <-h.authEventRetentionWake:
				h.runAuthenticationEventRetention(ctx)
			}
		}
	}()
}

func (h *Handler) wakeAuthenticationEventRetention() {
	if h.authEventRetentionWake == nil {
		return
	}
	select {
	case h.authEventRetentionWake <- struct{}{}:
	default:
	}
}

func (h *Handler) runAuthenticationEventRetention(ctx context.Context) {
	h.runAuthenticationEventRetentionAt(ctx, time.Now().UTC())
}

func (h *Handler) runAuthenticationEventRetentionAt(ctx context.Context, now time.Time) {
	if h.auth == nil {
		return
	}
	h.pruneAuthenticationEvents(ctx, now)
	h.pruneAuditEvents(ctx, now)
}

func (h *Handler) pruneAuthenticationEvents(ctx context.Context, now time.Time) {
	for batch := 0; batch < authenticationEventRetentionMaxBatchesRun; batch++ {
		result, err := h.auth.PruneAuthenticationEvents(
			ctx, now, auth.AuthenticationEventPruneBatchSize,
		)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("auth-event-retention: prune events: %v", err)
			}
			return
		}
		if result.Deleted == 0 {
			return
		}
	}
}

// pruneAuditEvents removes administration events past their retention in
// bounded batches, then notes in the log how many went.
func (h *Handler) pruneAuditEvents(ctx context.Context, now time.Time) {
	var total int64
	var cutoff time.Time
	for batch := 0; batch < authenticationEventRetentionMaxBatchesRun; batch++ {
		deleted, before, err := h.auth.PruneAuditEvents(ctx, now, auth.AuditPruneBatchSize)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("audit-retention: prune events: %v", err)
			}
			break
		}
		total += deleted
		cutoff = before
		if deleted < auth.AuditPruneBatchSize {
			break
		}
	}
	if total == 0 {
		return
	}
	log.Printf("audit-retention: removed %d administration events older than %s", total, cutoff.Format(time.RFC3339))
	if err := h.auth.RecordAuditPruning(ctx, total, cutoff); err != nil && ctx.Err() == nil {
		log.Printf("audit-retention: record pruning: %v", err)
	}
}

// WaitAuthenticationEventRetention joins workers after lifecycle cancellation.
func (h *Handler) WaitAuthenticationEventRetention() { h.authEventRetentionWorkers.Wait() }
