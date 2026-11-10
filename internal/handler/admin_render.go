package handler

import (
	"bytes"
	"context"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// renderAdminPage writes one page of the admin console: the whole console,
// or only its main area when htmx asks for it. The page renders into a
// buffer first, so a failure answers with an error instead of half a page.
func (h *Handler) renderAdminPage(w http.ResponseWriter, r *http.Request, status int, page views.AdminShellPage) {
	ctx := r.Context()
	var output bytes.Buffer
	var err error
	if r.Header.Get("HX-Request") == "true" {
		err = views.AdminMain(page.Content).Render(ctx, &output)
	} else {
		page.UISettings = h.adminUISettings(ctx)
		err = views.AdminShell(page).Render(ctx, &output)
	}
	if err != nil {
		log.Printf("render admin page %q: %v", page.Section, err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_render_admin_page"), http.StatusInternalServerError)
		return
	}
	setAdminPageHeaders(w)
	w.WriteHeader(status)
	_, _ = output.WriteTo(w)
}

// setAdminPageHeaders keeps admin pages out of caches, search engines and
// the referrer of links followed from them.
func setAdminPageHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

// adminUISettings are the display preferences of the person viewing the
// console, for its theme. Without one, the console uses the defaults.
func (h *Handler) adminUISettings(ctx context.Context) map[string]string {
	user := auth.GetCurrentUser(ctx)
	if h.db == nil || user == nil {
		return nil
	}
	return h.db.GetUISettings(ctx, user.ID)
}
