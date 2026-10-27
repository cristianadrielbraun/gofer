package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// outlookPushSettings describes an Outlook account's choice between
// Microsoft's change notifications and scheduled checks for the sync page.
func (h *Handler) outlookPushSettings(ctx context.Context, accountID string) *models.OutlookPushSettings {
	enabled, err := h.db.ProviderPushEnabled(ctx, accountID)
	if err != nil {
		enabled = true
	}
	var status mail.OutlookPushStatus
	if h.userIMAP != nil {
		status = h.userIMAP.OutlookPushStatus(accountID)
	} else if h.syncer != nil {
		status = h.syncer.OutlookPushStatus(accountID)
	}
	settings := &models.OutlookPushSettings{
		AccountID: accountID,
		Enabled:   enabled,
		Available: status.Available,
		Managed:   h.userIMAP != nil,
	}
	if enabled && status.Available {
		settings.Subscribed, settings.Expires = status.Subscribed, status.Expires
		settings.Error, settings.RetryAt = status.Error, status.RetryAt
	}
	return settings
}

// handleOutlookPushSetting records an Outlook account's choice and applies
// it right away: a new choice of notifications subscribes the account, and
// scheduled checks delete its subscription.
func (h *Handler) handleOutlookPushSetting(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	var enabled bool
	switch r.PostFormValue("enabled") {
	case "true":
		enabled = true
	case "false":
	default:
		http.Error(w, "invalid choice", http.StatusBadRequest)
		return
	}
	ctx, owner, id := r.Context(), h.userID(r.Context()), strings.TrimSpace(r.PathValue("id"))
	err := h.withOutlookAccount(ctx, owner, id, func(db *storage.DB) error {
		return db.SetProviderPushEnabled(ctx, owner, id, enabled)
	})
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if h.userIMAP != nil {
		// The account's sync keeps or deletes the subscription.
		if err := h.userIMAP.QueueAccount(ctx, id); err != nil {
			log.Printf("outlook push account=%s: queue sync: %v", id, err)
		}
	} else if h.syncer != nil {
		h.syncer.OutlookPushChanged()
	}
	h.renderOutlookPush(w, r, owner, id, false, nil)
}

// handleOutlookPushCheck makes a new subscription for the account now. Graph
// accepts it only after calling Gofer's notification URL, so it shows
// whether Microsoft can reach this server.
func (h *Handler) handleOutlookPushCheck(w http.ResponseWriter, r *http.Request) {
	ctx, owner, id := r.Context(), h.userID(r.Context()), strings.TrimSpace(r.PathValue("id"))
	if err := h.withOutlookAccount(ctx, owner, id, func(*storage.DB) error { return nil }); err != nil {
		userAccountError(w, r, err)
		return
	}
	var err error
	if h.userIMAP != nil {
		err = h.userIMAP.CheckOutlookPush(ctx, owner, id)
	} else if h.syncer != nil {
		err = h.syncer.CheckOutlookPush(ctx, id)
	} else {
		err = mail.ErrOutlookPushUnavailable
	}
	if errors.Is(err, storage.ErrAccountRoute) {
		http.NotFound(w, r)
		return
	}
	h.renderOutlookPush(w, r, owner, id, true, err)
}

// withOutlookAccount runs fn with the database holding the owner's Outlook
// account, and refuses other accounts.
func (h *Handler) withOutlookAccount(ctx context.Context, owner, id string, fn func(*storage.DB) error) error {
	if id == "" {
		return storage.ErrAccountRoute
	}
	if h.userAccounts != nil {
		return h.userAccounts.WithAccountForUser(ctx, owner, id, func(local *config.AccountStore, db *storage.DB) error {
			cfg, err := local.GetSettingsConfig(ctx, id)
			if err != nil {
				return err
			}
			if cfg.Provider != providers.ProviderOutlook {
				return storage.ErrAccountRoute
			}
			return fn(db)
		})
	}
	user, err := h.db.GetAccountUserID(ctx, id)
	if err != nil {
		return err
	}
	provider, err := h.db.GetAccountProvider(ctx, id)
	if err != nil {
		return err
	}
	if user != owner || provider != providers.ProviderOutlook {
		return storage.ErrAccountRoute
	}
	return fn(h.db)
}

// renderOutlookPush answers with the account's control, and the result of
// Check now when checked is set.
func (h *Handler) renderOutlookPush(w http.ResponseWriter, r *http.Request, owner, id string, checked bool, checkErr error) {
	var settings *models.OutlookPushSettings
	err := h.withOutlookAccount(r.Context(), owner, id, func(db *storage.DB) error {
		local := &Handler{db: db, userIMAP: h.userIMAP, syncer: h.syncer}
		settings = local.outlookPushSettings(r.Context(), id)
		return nil
	})
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	settings.Checked = checked
	if checkErr != nil {
		settings.CheckError = mail.OutlookPushErrorMessage(checkErr)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.OutlookPushControl(*settings).Render(r.Context(), w); err != nil {
		log.Printf("render outlook push control: %v", err)
	}
}
