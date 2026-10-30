package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// handleCalendarSourceSync starts syncing a calendar the Calendar sidebar
// offers, one the account has but doesn't sync, or with DELETE stops syncing
// one: it joins or leaves the calendars its account syncs, as ticking or
// unticking it in Calendar sync does. Nothing changes on the provider.
func (h *Handler) handleCalendarSourceSync(w http.ResponseWriter, r *http.Request) {
	start := r.Method != http.MethodDelete
	sourceID, owner := strings.TrimSpace(r.PathValue("id")), h.userID(r.Context())
	if sourceID == "" {
		http.Error(w, "calendar required", http.StatusBadRequest)
		return
	}
	var err error
	if h.userStorage == nil {
		var accountID string
		var ids []string
		if accountID, ids, err = calendarSourceSelection(r.Context(), h.db, owner, sourceID, start); err == nil {
			if err = h.db.SetCalendarSourceSelection(r.Context(), owner, accountID, ids); err == nil && start {
				err = h.db.ScheduleCalendarSync(r.Context(), owner, sourceID, time.Now())
			}
		}
	} else {
		err = h.userIMAP.RunUserServiceWork(r.Context(), owner, func(ctx context.Context) error {
			var accountID string
			var ids []string
			if err := h.userAccounts.WithUser(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
				var err error
				accountID, ids, err = calendarSourceSelection(ctx, db, owner, sourceID, start)
				return err
			}); err != nil {
				return err
			}
			if err := h.userAccounts.SetCalendarSourcesSelected(ctx, owner, accountID, ids); err != nil {
				return err
			}
			h.wakeUserCalendarAccount(ctx, owner, accountID)
			return nil
		})
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.NotFound(w, r)
	case errors.Is(err, storage.ErrCalendarSourceSelection):
		http.Error(w, "This calendar is no longer offered. Refresh the page.", http.StatusConflict)
	case err != nil && h.userStorage != nil:
		userAccountError(w, r, err)
	case err != nil:
		http.Error(w, "Could not change whether this calendar syncs.", http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// calendarSourceSelection is the account of one of the owner's calendars and
// the calendars that account is to sync: with it added when start, for an
// unsynced calendar, or without it otherwise, for a synced one.
func calendarSourceSelection(ctx context.Context, db *storage.DB, owner, sourceID string, start bool) (string, []string, error) {
	selected, err := db.ListSelectedCalendarSources(ctx, owner)
	if err != nil {
		return "", nil, err
	}
	candidates := selected
	if start {
		if candidates, err = db.ListUnselectedCalendarSources(ctx, owner); err != nil {
			return "", nil, err
		}
	}
	accountID := ""
	for _, source := range candidates {
		if source.ID == sourceID {
			accountID = source.AccountID
		}
	}
	if accountID == "" {
		return "", nil, sql.ErrNoRows
	}
	var ids []string
	if start {
		ids = append(ids, sourceID)
	}
	for _, source := range selected {
		if source.AccountID == accountID && source.ID != sourceID {
			ids = append(ids, source.ID)
		}
	}
	return accountID, ids, nil
}
