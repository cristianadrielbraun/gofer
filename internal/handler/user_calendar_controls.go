package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleUserSaveCalendarSources(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil || len(r.PostForm["source_id"]) > 256 {
		http.Error(w, "invalid calendar source selection", http.StatusBadRequest)
		return
	}
	owner, id := h.userID(r.Context()), strings.TrimSpace(r.PathValue("id"))
	err := h.userIMAP.RunUserServiceWork(r.Context(), owner, func(ctx context.Context) error {
		if err := h.userAccounts.SetCalendarSourcesSelected(ctx, owner, id, r.PostForm["source_id"]); err != nil {
			return err
		}
		h.wakeUserCalendarAccount(ctx, owner, id)
		return nil
	})
	if err != nil {
		if errors.Is(err, storage.ErrCalendarSourceSelection) {
			http.Error(w, "invalid calendar source selection", http.StatusBadRequest)
		} else {
			userAccountError(w, r, err)
		}
		return
	}
	var data *models.EditAccountData
	err = h.userAccounts.WithAccountForUser(r.Context(), owner, id, func(local *config.AccountStore, _ *storage.DB) error {
		var err error
		data, err = local.GetEditData(r.Context(), id)
		return err
	})
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.CalendarSyncSettingsResult(*data, "Calendar sources saved.", false).Render(r.Context(), w); err != nil {
		log.Printf("owned calendar: render selection: %v", err)
	}
}

func (h *Handler) handleUserCalendarService(w http.ResponseWriter, r *http.Request, owner, id string) {
	err := h.userIMAP.RunUserServiceWork(r.Context(), owner, func(ctx context.Context) error {
		if err := h.userAccounts.SetCalendarServiceEnabled(ctx, owner, id, r.FormValue("enabled") == "true"); err != nil {
			return err
		}
		h.wakeUserCalendarAccount(ctx, owner, id)
		return nil
	})
	if err != nil {
		if errors.Is(err, storage.ErrCalendarSourcesNotConfigured) {
			http.Error(w, "Discover at least one calendar before enabling Calendar.", http.StatusConflict)
			return
		}
		userAccountError(w, r, err)
		return
	}
	var account *models.Account
	err = h.userAccounts.WithAccountForUser(r.Context(), owner, id, func(local *config.AccountStore, _ *storage.DB) error {
		var err error
		account, err = local.GetAccountByIDForUser(r.Context(), owner, id)
		if err == nil && account == nil {
			return storage.ErrAccountRoute
		}
		return err
	})
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.SettingsAccountCard(*account).Render(r.Context(), w)
}

// The local commit is authoritative; a failed central wake cannot undo it.
// Startup reconciliation must rediscover selected sources if a hint was lost.
func (h *Handler) wakeUserCalendarAccount(ctx context.Context, owner, id string) {
	if err := h.WakeUserCalendarAccount(ctx, owner, id); err != nil {
		log.Printf("owned calendar: wake committed source setting: %v", err)
	}
}
