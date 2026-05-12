package handler

import (
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accounts, _ := h.db.GetAccounts(ctx, h.userID(ctx))
	uiSettings := h.db.GetUISettings(ctx, h.userID(ctx))
	avatarStatus, err := h.avatarStatus(ctx)
	if err != nil {
		http.Error(w, "failed to get admin status", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		views.AdminPartial(avatarStatus).Render(ctx, w)
		return
	}

	views.AdminLayout(accounts, uiSettings, avatarStatus).Render(ctx, w)
}
