package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// RegisterUserStorageRoutes mounts the converted routes on a separate mux for
// integration testing. It is deliberately not a complete application router.
// Production startup still calls RegisterRoutes until migration/worker routing
// is complete. Authentication middleware must wrap this mux as usual.
func (h *Handler) RegisterUserStorageRoutes(ctx context.Context, mux *http.ServeMux, routing *storage.AccountRouting) error {
	if routing == nil || routing.System() != h.db {
		return errors.New("user routing must use the handler's central database")
	}
	if h.auth == nil || !h.auth.IsEnabled() || h.auth.Config().AuthenticationMode() != auth.ModeManaged {
		return errors.New("user storage routes require managed authentication")
	}
	// Share immutable services, not Handler mutexes or mutable worker state.
	routed := &Handler{db: h.db, auth: h.auth, syncer: h.syncer, userStorage: routing,
		vapidPublicKey: h.vapidPublicKey, userBackfillQueue: make(chan userContactBackfillJob, 32),
		userBackfills: make(map[string]struct{})}
	private := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if err := routing.ValidateUser(r.Context(), routed.userID(r.Context())); err != nil {
				http.Error(w, "user storage unavailable", http.StatusForbidden)
				return
			}
			handler(w, r)
		})
	}
	private("GET /api/settings/ui", routed.handleGetUISettings)
	private("PATCH /api/settings/ui", routed.handleSaveUISettings)
	private("GET /api/push/vapid-public-key", routed.handlePushVAPIDPublicKey)
	// Browser endpoints need one owner across the entire installation. Their
	// registration stays central; notification preferences are stored per user.
	private("POST /api/push/subscription", routed.handleSavePushSubscription)
	private("DELETE /api/push/subscription", routed.handleDeletePushSubscription)
	private("GET /api/contacts/search", routed.handleContactSearch)
	private("GET /api/contacts/export", routed.handleExportContacts)
	private("GET /api/contacts/{id}/export", routed.handleExportContact)
	go routed.runUserContactBackfills(ctx)
	return nil
}

func (h *Handler) withUserDB(ctx context.Context, userID string, fn func(*storage.DB) error) error {
	if h.userStorage == nil {
		return fn(h.db)
	}
	return h.userStorage.WithUser(ctx, userID, fn)
}

type userContactBackfillJob struct{ userID, sourceKey string }

func (h *Handler) ensureUserContactsBackfilled(ctx context.Context) {
	userID := h.userID(ctx)
	var job userContactBackfillJob
	err := h.withUserDB(ctx, userID, func(db *storage.DB) error {
		settings := db.GetContactSettings(ctx, userID)
		if !settings.AutoCreateObserved || (!settings.ObserveSenders && !settings.ObserveRecipients) {
			return nil
		}
		key := ""
		if settings.ObserveSenders {
			key = "senders"
		}
		if settings.ObserveRecipients {
			if key != "" {
				key += ","
			}
			key += "recipients"
		}
		done, err := db.GetSetting(ctx, userID, "contacts_observed_backfilled_v1")
		if err != nil {
			return err
		}
		if done != key {
			job = userContactBackfillJob{userID, key}
		}
		return nil
	})
	if err != nil {
		log.Printf("contacts: load user backfill settings: %v", err)
		return
	}
	if job.userID == "" {
		return
	}
	h.contactBackfillMu.Lock()
	defer h.contactBackfillMu.Unlock()
	if _, exists := h.userBackfills[userID]; exists {
		return
	}
	select {
	case h.userBackfillQueue <- job:
		h.userBackfills[userID] = struct{}{}
	default: // A later search retries; pending users never open/pin database files.
	}
}

func (h *Handler) runUserContactBackfills(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-h.userBackfillQueue:
			workCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			state := models.ContactBackfillState{InProgress: true, StartedAt: time.Now().UTC()}
			h.publishContactBackfill(job.userID, state)
			err := h.withUserDB(workCtx, job.userID, func(db *storage.DB) error {
				if err := db.BackfillObservedContactsWithProgress(workCtx, job.userID, func(processed int) {
					state.Processed = processed
					h.publishContactBackfill(job.userID, state)
				}); err != nil {
					return err
				}
				return db.SetSetting(workCtx, job.userID, "contacts_observed_backfilled_v1", job.sourceKey)
			})
			cancel()
			state.InProgress, state.FinishedAt = false, time.Now().UTC()
			if err != nil {
				state.LastError = err.Error()
				log.Printf("contacts: user backfill failed: %v", err)
			}
			h.publishContactBackfill(job.userID, state)
			h.contactBackfillMu.Lock()
			delete(h.userBackfills, job.userID)
			h.contactBackfillMu.Unlock()
		}
	}
}
