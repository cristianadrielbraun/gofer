package handler

import (
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleAdminRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/avatars/", http.StatusFound)
}

func (h *Handler) handleAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	activeTab, ok := adminAvatarTab(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	uiSettings := h.db.GetUISettings(ctx, h.userID(ctx))
	avatarStatus, err := h.avatarStatus(ctx)
	if err != nil {
		http.Error(w, "failed to get admin status", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		views.AdminPartial(avatarStatus, activeTab).Render(ctx, w)
		return
	}

	views.AdminLayout(uiSettings, avatarStatus, activeTab).Render(ctx, w)
}

func adminAvatarTab(r *http.Request) (string, bool) {
	tab := r.PathValue("tab")
	if tab == "" {
		return "overview", true
	}
	switch tab {
	case "overview", "senders", "providers", "events":
		return tab, true
	default:
		return "", false
	}
}
