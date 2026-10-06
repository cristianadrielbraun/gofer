package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// RegisterUserStorageRoutes mounts the converted routes on a separate mux for
// integration testing. It is deliberately not a complete application router.
// Production startup still calls RegisterRoutes until migration/worker routing
// is complete. Authentication middleware must wrap this mux as usual.
func (h *Handler) RegisterUserStorageRoutes(ctx context.Context, mux *http.ServeMux, routing *storage.AccountRouting, options ...UserStorageOptions) error {
	if routing == nil || routing.System() != h.db {
		return errors.New("user routing must use the handler's central database")
	}
	if h.auth == nil || !h.auth.IsEnabled() || h.auth.Config().AuthenticationMode() != auth.ModeManaged {
		return errors.New("user storage routes require managed authentication")
	}
	if len(options) > 1 {
		return errors.New("only one user storage options value is supported")
	}
	var option UserStorageOptions
	if len(options) == 1 {
		option = options[0]
	}
	if option.Accounts != nil {
		if option.Accounts.Routing() != routing {
			return errors.New("account repository must use the same routing coordinator")
		}
		if option.Hooks.Created == nil || option.Hooks.Updated == nil || option.Hooks.Cleanup == nil {
			return errors.New("routed account management requires create, update and cleanup lifecycle hooks")
		}
	} else if option.Hooks.Created != nil || option.Hooks.Updated != nil || option.Hooks.Cleanup != nil {
		return errors.New("account lifecycle hooks require a scoped account repository")
	}
	// Share immutable services, not Handler mutexes or mutable worker state.
	routed := &Handler{db: h.db, auth: h.auth, syncer: h.syncer, userStorage: routing,
		userAccounts: option.Accounts, userAccountHooks: option.Hooks, userStorageContext: ctx, userDeletions: make(map[string]*userAccountDeletionJob),
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
	private("GET /{$}", routed.handleIndex)
	private("GET /folder/{id}", routed.handleFolderPartial)
	private("GET /folder/{id}/full", routed.handleFolderFull)
	private("GET /folder/{id}/{email}", routed.handleFolderWithEmail)
	private("GET /mail/folder/{id}/items", routed.handleMailItems)
	private("GET /mail/thread/{threadId}/subitems", routed.handleThreadSubItems)
	private("GET /email/{id}", routed.handleEmailPartial)
	private("GET /email/{id}/body", routed.handleUserEmailBody)
	private("GET /search", routed.handleSearch)
	private("GET /api/folders/unread", routed.handleFolderUnreadCounts)
	private("GET /api/sidebar/mail", routed.handleMailSidebar)
	private("GET /api/sidebar/accounts/{id}", routed.handleSidebarAccount)
	private("GET /api/accounts", routed.handleUserAccounts)
	private("GET /settings/accounts", routed.handleUserAccountSettings)
	private("GET /api/accounts/{id}/deletion-status", routed.handleUserAccountDeletionStatus)
	if option.Accounts != nil {
		private("POST /api/accounts", routed.handleUserCreateAccount)
		private("GET /api/accounts/{id}/edit", routed.handleUserEditAccount)
		private("POST /api/accounts/{id}/edit", routed.handleUserUpdateAccount)
		private("POST /api/accounts/{id}/color", routed.handleUserAccountColor)
		private("DELETE /api/accounts/{id}", routed.handleUserDeleteAccount)
	}
	go routed.runUserContactBackfills(ctx)
	return nil
}

// UserAccountHooks bridge persistence to converted provider workers and external
// cleanup. They run without a database lease. Cleanup must stop/join account
// workers and remove blobs and central OAuth credentials, and be idempotent.
type UserAccountHooks struct {
	Created func(context.Context, string) error
	Updated func(context.Context, string) error
	Cleanup func(context.Context, string) error
}

type UserStorageOptions struct {
	Accounts *config.UserAccountStore
	Hooks    UserAccountHooks
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
