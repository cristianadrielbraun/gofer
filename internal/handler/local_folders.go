package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// localFoldersNoDelete answers a request to delete the Local folders account,
// which every user keeps. It can be hidden instead.
const localFoldersNoDelete = "Local folders can't be deleted. You can hide them instead."

// localFoldersReady holds the users whose Local folders account is known to
// exist, so the mailbox doesn't look for it on every page.
var localFoldersReady sync.Map

// ensureLocalFolders makes the user's Local folders account the first time
// they open their mail.
func (h *Handler) ensureLocalFolders(ctx context.Context, userID string) error {
	if _, ok := localFoldersReady.Load(h.localFoldersKey(userID)); ok {
		return nil
	}
	var err error
	if h.userStorage != nil {
		err = h.userStorage.EnsureLocalFolders(ctx, userID)
	} else if h.db != nil {
		_, err = h.db.EnsureLocalFoldersAccount(ctx, userID)
	}
	if err == nil {
		localFoldersReady.Store(h.localFoldersKey(userID), struct{}{})
	}
	return err
}

// localFoldersKey tells apart users of different handlers, such as tests
// that each open their own database.
func (h *Handler) localFoldersKey(userID string) any {
	if h.userStorage != nil {
		return [2]any{h.userStorage, userID}
	}
	return [2]any{h.db, userID}
}

// localFoldersShownValue reads the Local folders "Show" switch: the form
// carries shown=true while it's on and nothing once it's off.
func localFoldersShownValue(r *http.Request) (bool, error) {
	if err := r.ParseForm(); err != nil {
		return false, err
	}
	return r.FormValue("shown") == "true", nil
}

// handleLocalFoldersShown hides or shows the user's Local folders.
func (h *Handler) handleLocalFoldersShown(w http.ResponseWriter, r *http.Request) {
	shown, err := localFoldersShownValue(r)
	if err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if err := h.accountStore.SetLocalFoldersHidden(r.Context(), h.userID(r.Context()), r.PathValue("id"), !shown); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "couldn't save the change", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUserLocalFoldersShown hides or shows Local folders kept in the
// user's own storage.
func (h *Handler) handleUserLocalFoldersShown(w http.ResponseWriter, r *http.Request) {
	shown, err := localFoldersShownValue(r)
	if err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	owner, id := h.userID(r.Context()), r.PathValue("id")
	if err := h.userAccounts.WithAccountForUser(r.Context(), owner, id, func(local *config.AccountStore, _ *storage.DB) error {
		return local.SetLocalFoldersHidden(r.Context(), owner, id, !shown)
	}); err != nil {
		userAccountError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
