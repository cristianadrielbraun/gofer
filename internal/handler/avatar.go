package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	mail "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

const (
	avatarBackfillBatchSize  = 100
	avatarBackfillFetchDelay = 100 * time.Millisecond
	avatarMissingTTL         = 24 * time.Hour
	avatarErrorRetryAfter    = 6 * time.Hour
)

func (h *Handler) StartAvatarBackfill(ctx context.Context) {
	go func() {
		h.runAvatarBackfill(ctx)

		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.runAvatarBackfill(ctx)
			}
		}
	}()
}

func (h *Handler) runAvatarBackfill(ctx context.Context) {
	log.Printf("avatar: backfill worker started")
	startedAt := time.Now()
	h.setAvatarBackfillState(models.AvatarBackfillState{InProgress: true, StartedAt: startedAt})

	if _, err := h.db.EnsureSenderAvatarCandidates(ctx); err != nil {
		log.Printf("avatar: candidate scan failed: %v", err)
		h.setAvatarBackfillState(models.AvatarBackfillState{InProgress: false, LastError: err.Error(), StartedAt: startedAt, FinishedAt: time.Now()})
		return
	}

	stats, err := h.db.GetSenderAvatarStats(ctx)
	if err != nil {
		log.Printf("avatar: status count failed: %v", err)
		h.setAvatarBackfillState(models.AvatarBackfillState{InProgress: false, LastError: err.Error(), StartedAt: startedAt, FinishedAt: time.Now()})
		return
	}

	state := models.AvatarBackfillState{InProgress: true, Total: stats.Due, StartedAt: startedAt}
	h.setAvatarBackfillState(state)

	for {
		if err := ctx.Err(); err != nil {
			state.InProgress = false
			state.LastError = err.Error()
			state.FinishedAt = time.Now()
			h.setAvatarBackfillState(state)
			return
		}

		candidates, err := h.db.GetDueSenderAvatarCandidates(ctx, avatarBackfillBatchSize)
		if err != nil {
			state.InProgress = false
			state.LastError = err.Error()
			state.FinishedAt = time.Now()
			h.setAvatarBackfillState(state)
			log.Printf("avatar: candidate load failed: %v", err)
			return
		}
		if len(candidates) == 0 {
			break
		}

		for _, candidate := range candidates {
			_, found, err := h.fetchAndPersistAvatar(ctx, candidate.EmailHash, candidate.Email)
			state.Processed++
			if err != nil {
				state.Errors++
				state.LastError = err.Error()
			} else if found {
				state.Found++
			} else {
				state.Missing++
			}
			h.setAvatarBackfillState(state)

			select {
			case <-ctx.Done():
				state.InProgress = false
				state.LastError = ctx.Err().Error()
				state.FinishedAt = time.Now()
				h.setAvatarBackfillState(state)
				return
			case <-time.After(avatarBackfillFetchDelay):
			}
		}
	}

	state.InProgress = false
	state.FinishedAt = time.Now()
	h.setAvatarBackfillState(state)
	log.Printf("avatar: backfill worker finished processed=%d found=%d missing=%d errors=%d", state.Processed, state.Found, state.Missing, state.Errors)
}

func (h *Handler) fetchAndPersistAvatar(ctx context.Context, hash, email string) (avatarresolver.Image, bool, error) {
	image, found, err := h.avatar.ResolveGravatar(ctx, hash)
	if err != nil {
		_ = h.db.SaveSenderAvatarError(ctx, hash, email, err.Error(), time.Now().Add(avatarErrorRetryAfter))
		return avatarresolver.Image{}, false, err
	}
	if found {
		expiresAt := image.ExpiresAt
		if expiresAt.IsZero() {
			expiresAt = time.Now().Add(7 * 24 * time.Hour)
		}
		if err := h.db.SaveSenderAvatarFound(ctx, hash, email, image.ContentType, image.Data, expiresAt); err != nil {
			return avatarresolver.Image{}, false, err
		}
		if dataURL, err := h.db.SenderAvatarDataURL(ctx, hash); err == nil && dataURL != "" {
			h.syncer.Events().Publish(mail.Event{Type: mail.EventAvatarUpdated, AvatarHash: hash, AvatarDataURL: dataURL})
		}
		return image, true, nil
	}

	if err := h.db.SaveSenderAvatarMissing(ctx, hash, email, time.Now().Add(avatarMissingTTL)); err != nil {
		return avatarresolver.Image{}, false, err
	}
	return avatarresolver.Image{}, false, nil
}

func (h *Handler) handleAvatarStatus(w http.ResponseWriter, r *http.Request) {
	status, err := h.avatarStatus(r.Context())
	if err != nil {
		http.Error(w, "failed to get avatar status", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (h *Handler) avatarStatus(ctx context.Context) (models.AvatarStatus, error) {
	stats, err := h.db.GetSenderAvatarStats(ctx)
	if err != nil {
		return models.AvatarStatus{}, err
	}
	return models.AvatarStatus{
		Backfill: h.getAvatarBackfillState(),
		Cache: models.AvatarCacheStats{
			Total:   stats.Total,
			Pending: stats.Pending,
			Found:   stats.Found,
			Missing: stats.Missing,
			Error:   stats.Error,
			Due:     stats.Due,
		},
	}, nil
}

func (h *Handler) setAvatarBackfillState(state models.AvatarBackfillState) {
	h.avatarBackfillMu.Lock()
	h.avatarBackfillState = state
	h.avatarBackfillMu.Unlock()
}

func (h *Handler) getAvatarBackfillState() models.AvatarBackfillState {
	h.avatarBackfillMu.RLock()
	defer h.avatarBackfillMu.RUnlock()
	return h.avatarBackfillState
}

func serveAvatarImage(w http.ResponseWriter, contentType string, data []byte, expiresAt time.Time) {
	if contentType == "" {
		contentType = "image/jpeg"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
	if !expiresAt.IsZero() {
		w.Header().Set("Expires", expiresAt.UTC().Format(http.TimeFormat))
	}
	_, _ = w.Write(data)
}
