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

// StartAuthenticationEventRetentionWorker prunes old audit rows at startup,
// periodically, and after an administrator changes the configured window.
func (h *Handler) StartAuthenticationEventRetentionWorker(ctx context.Context) {
	go func() {
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
