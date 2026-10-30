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
	calendar, contacts, err := h.db.OutlookPushServices(ctx, accountID)
	if err != nil {
		calendar, contacts = false, false
	}
	for _, service := range []mail.OutlookPushService{mail.OutlookPushCalendar, mail.OutlookPushContacts} {
		state := models.OutlookPushServiceState{Service: string(service), Synced: (service == mail.OutlookPushCalendar && calendar) || (service == mail.OutlookPushContacts && contacts)}
		var serviceStatus mail.OutlookPushStatus
		if h.userIMAP != nil {
			serviceStatus = h.userIMAP.OutlookServicePushStatus(accountID, service)
		} else if h.syncer != nil {
			serviceStatus = h.syncer.OutlookServicePushStatus(accountID, service)
		}
		if enabled && serviceStatus.Available && state.Synced {
			state.Subscribed, state.Error = serviceStatus.Subscribed, serviceStatus.Error
		}
		settings.Services = append(settings.Services, state)
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
	h.renderOutlookPush(w, r, owner, id, false, nil, nil)
}

// handleOutlookPushCheck makes new subscriptions for the account now: its
// mailbox's, and its calendar's and contacts' when it syncs them. Graph
// accepts each only after calling Gofer's notification URL, so they show
// whether Microsoft can reach this server for each.
func (h *Handler) handleOutlookPushCheck(w http.ResponseWriter, r *http.Request) {
	ctx, owner, id := r.Context(), h.userID(r.Context()), strings.TrimSpace(r.PathValue("id"))
	if err := h.withOutlookAccount(ctx, owner, id, func(*storage.DB) error { return nil }); err != nil {
		userAccountError(w, r, err)
		return
	}
	var err, servicesErr error
	var services map[mail.OutlookPushService]error
	if h.userIMAP != nil {
		err = h.userIMAP.CheckOutlookPush(ctx, owner, id)
		services, servicesErr = h.userIMAP.CheckOutlookPushServices(ctx, owner, id)
	} else if h.syncer != nil {
		err = h.syncer.CheckOutlookPush(ctx, id)
		services, servicesErr = h.syncer.CheckOutlookPushServices(ctx, id)
	} else {
		err = mail.ErrOutlookPushUnavailable
	}
	if errors.Is(err, storage.ErrAccountRoute) || errors.Is(servicesErr, storage.ErrAccountRoute) {
		http.NotFound(w, r)
		return
	}
	if servicesErr != nil && ctx.Err() == nil {
		log.Printf("outlook push account=%s: check calendar and contacts: %v", id, servicesErr)
	}
	h.renderOutlookPush(w, r, owner, id, true, err, services)
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
// Check now when checked is set: checkErr for the mailbox, and services for
// the calendar and contacts it checked.
func (h *Handler) renderOutlookPush(w http.ResponseWriter, r *http.Request, owner, id string, checked bool, checkErr error, services map[mail.OutlookPushService]error) {
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
	for i := range settings.Services {
		serviceErr, tested := services[mail.OutlookPushService(settings.Services[i].Service)]
		if !tested {
			continue
		}
		settings.Services[i].Checked = true
		if serviceErr != nil {
			settings.Services[i].CheckError = mail.OutlookPushErrorMessage(serviceErr)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.OutlookPushControl(*settings).Render(r.Context(), w); err != nil {
		log.Printf("render outlook push control: %v", err)
	}
}
