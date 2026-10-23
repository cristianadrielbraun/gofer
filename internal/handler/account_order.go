package handler

import (
	"errors"
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
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	ids := r.PostForm["account_id"]
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}
	ctx, owner := r.Context(), h.userID(r.Context())
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		return db.SetAccountOrder(ctx, owner, ids)
	})
	if errors.Is(err, storage.ErrAccountOrderChanged) {
		http.Error(w, "Your accounts changed. Reload the page and try again.", http.StatusConflict)
		return
	}
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
