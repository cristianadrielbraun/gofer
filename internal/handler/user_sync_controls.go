package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleUserSaveSyncSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	minutes, err := strconv.Atoi(r.FormValue("sync_interval_minutes"))
	if err != nil || minutes < 1 || minutes > 1440 {
		http.Error(w, "interval must be between 1 and 1440 minutes", http.StatusBadRequest)
		return
	}
	updates := make(map[string][]string)
	for _, id := range r.PostForm["account_ids"] {
		id = strings.TrimSpace(id)
		if id == "" {
			http.Error(w, "invalid account", http.StatusBadRequest)
			return
		}
		updates[id] = nil
	}
	for _, entry := range r.PostForm["idle_folders"] {
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			http.Error(w, "invalid folder selection", http.StatusBadRequest)
			return
		}
		id, folder := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		updates[id] = append(updates[id], folder)
	}
	if len(updates) > 256 {
		http.Error(w, "too many accounts", http.StatusBadRequest)
		return
	}
	ctx, owner := r.Context(), h.userID(r.Context())
	for id := range updates {
		err := h.userAccounts.WithAccountForUser(ctx, owner, id, func(local *config.AccountStore, _ *storage.DB) error {
			cfg, err := local.GetConfig(ctx, id)
			if err != nil {
				return err
			}
			if cfg.Provider != "imap" || cfg.AuthMethod != "plain" {
				return storage.ErrAccountRoute
			}
			return nil
		})
		if err != nil {
			userAccountError(w, r, err)
			return
		}
	}
	if err := h.withUserDB(ctx, owner, func(db *storage.DB) error { return db.SaveUserSyncSettings(ctx, owner, minutes, updates) }); err != nil {
		if errors.Is(err, storage.ErrSyncSettingsInvalid) {
			http.Error(w, "invalid sync settings", http.StatusBadRequest)
		} else {
			userAccountError(w, r, err)
		}
		return
	}
	if err := h.userIMAP.RefreshUserSettings(ctx, owner); err != nil {
		http.Error(w, "settings saved; worker refresh failed, retry saving", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (h *Handler) handleUserEmailService(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if r.FormValue("service") != "email" {
		http.Error(w, "This service is not available yet.", http.StatusNotImplemented)
		return
	}
	if value := r.FormValue("enabled"); value != "true" && value != "false" {
		http.Error(w, "invalid enabled value", http.StatusBadRequest)
		return
	}
	owner, id := h.userID(r.Context()), r.PathValue("id")
	var account *models.Account
	err := h.userAccounts.WithAccountForUser(r.Context(), owner, id, func(local *config.AccountStore, _ *storage.DB) error {
		if r.FormValue("enabled") == "true" {
			cfg, err := local.GetConfig(r.Context(), id)
			if err != nil {
				return err
			}
			if cfg.Provider != "imap" || cfg.AuthMethod != "plain" {
				return storage.ErrAccountRoute
			}
		}
		if err := local.SetEmailSyncEnabled(r.Context(), owner, id, r.FormValue("enabled") == "true"); err != nil {
			return err
		}
		var err error
		account, err = local.GetAccountByIDForUser(r.Context(), owner, id)
		return err
	})
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if err := h.userIMAP.RestartAccount(r.Context(), id); err != nil {
		http.Error(w, "email setting saved; worker restart failed, retry", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	_ = views.SettingsAccountCard(*account).Render(r.Context(), w)
}

func (h *Handler) handleUserManualSync(w http.ResponseWriter, r *http.Request) {
	owner := h.userID(r.Context())
	var ids []string
	err := h.withUserDB(r.Context(), owner, func(db *storage.DB) error {
		var err error
		ids, err = db.GetEmailSyncAccountIDs(r.Context(), owner)
		return err
	})
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if id := r.PathValue("id"); id != "" {
		found := false
		for _, candidate := range ids {
			if candidate == id {
				found = true
				break
			}
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		ids = []string{id}
	}
	if len(ids) == 0 {
		htmlStatus(w, http.StatusBadRequest, "Connect an email account before syncing mail.")
		return
	}
	if len(ids) > 256 {
		htmlStatus(w, http.StatusBadRequest, "Sync up to 256 accounts in one request.")
		return
	}
	id, started, err := h.userIMAP.StartManualSync(r.Context(), owner, ids)
	if err != nil {
		if errors.Is(err, mail.ErrUserIMAPManualCapacity) {
			w.Header().Set("Retry-After", "2")
			htmlStatus(w, http.StatusServiceUnavailable, "Mail sync is busy; retry shortly.")
		} else {
			userAccountError(w, r, err)
		}
		return
	}
	w.Header().Set("X-Gofer-Mail-Sync-Run-ID", id)
	if !started {
		w.Header().Set("X-Gofer-Mail-Sync-Running", "true")
		htmlStatus(w, http.StatusOK, "Mail sync is already running.")
		return
	}
	w.Header().Set("X-Gofer-Mail-Sync-Accounts", strconv.Itoa(len(ids)))
	w.Header().Set("X-Gofer-Mail-Sync-Mode", "sync")
	message := fmt.Sprintf("Mail sync started for %d accounts.", len(ids))
	if len(ids) == 1 {
		message = "Mail sync started for 1 account."
	}
	htmlStatus(w, http.StatusOK, message)
}

func (h *Handler) handleUserCancelSync(w http.ResponseWriter, r *http.Request) {
	active, err := h.userIMAP.CancelManualSync(r.Context(), h.userID(r.Context()))
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if active {
		htmlStatus(w, http.StatusOK, "Mail sync cancellation requested.")
	} else {
		htmlStatus(w, http.StatusOK, "No mail sync is running.")
	}
}
