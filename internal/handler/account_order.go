package handler

import (
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// handleAccountOrder puts the user's accounts in the order given, one
// account_id value per account. It refuses a list that doesn't name each of
// their accounts once, so the page can reload the accounts and try again.
func (h *Handler) handleAccountOrder(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form"), http.StatusBadRequest)
		return
	}
	items := r.PostForm["item"]
	for i := range items {
		items[i] = strings.TrimSpace(items[i])
	}
	ids := r.PostForm["account_id"]
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}
	_, hasGroup := r.PostForm["group_id"]
	groupID := strings.TrimSpace(r.PostFormValue("group_id"))
	ctx, owner := r.Context(), h.userID(r.Context())
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		if len(items) > 0 || hasGroup {
			return db.SetAccountContainerOrder(ctx, owner, groupID, items)
		}
		return db.SetAccountOrder(ctx, owner, ids)
	})
	if errors.Is(err, storage.ErrAccountOrderChanged) {
		http.Error(w, i18n.T(r.Context(), "server.error.your_accounts_changed_reload_the_page_and"), http.StatusConflict)
		return
	}
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
