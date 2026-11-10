package handler

import (
	"log"
	"net/http"
	"strings"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// Settings → Diagnostics shows the owner of an open or personal instance,
// who has no admin console, what the console's diagnostics show: avatar
// checks, contacts and labels. The pages and their data are the console's,
// at the owner's own addresses. Managed instances answer 404: their
// administrators use the console.

// ownerDiagnosticsAPI is where the owner's diagnostics read their data, as
// the console's read /api/admin.
const ownerDiagnosticsAPI = "/api/diagnostics"

func (h *Handler) registerOwnerDiagnosticsRoutes(mux *http.ServeMux) {
	owner := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, h.ownerDiagnostics(handler))
	}
	toAvatars := redirectAdminPage(views.OwnerAvatarDiagnosticsPath)
	owner("GET "+views.OwnerDiagnosticsPath, toAvatars)
	owner("GET "+views.OwnerDiagnosticsPath+"/{$}", toAvatars)
	owner("GET "+strings.TrimSuffix(views.OwnerAvatarDiagnosticsPath, "/"), toAvatars)
	owner("GET "+views.OwnerAvatarDiagnosticsPath+"{$}", h.handleOwnerAvatarDiagnostics)
	owner("GET "+views.OwnerAvatarDiagnosticsPath+"{tab}", h.handleOwnerAvatarDiagnostics)
	owner("GET "+views.OwnerDiagnosticsPath+"/contacts", h.handleOwnerContactDiagnostics)
	owner("GET "+views.OwnerDiagnosticsPath+"/labels", h.handleOwnerLabelDiagnostics)
	owner("POST "+views.OwnerAvatarDiagnosticsPath+"recheck", h.handleForceAvatarBackfill)
	owner("POST "+views.OwnerAvatarDiagnosticsPath+"cancel", h.handleCancelAvatarBackfill)
	owner("POST "+views.OwnerDiagnosticsPath+"/contacts/backfill", h.handleForceContactBackfill)
	owner("GET "+ownerDiagnosticsAPI+"/avatars/status", h.handleAvatarStatus)
	owner("GET "+ownerDiagnosticsAPI+"/avatars/attempts", h.handleAvatarAttempts)
	owner("GET "+ownerDiagnosticsAPI+"/avatars/senders", h.handleAvatarSenders)
	owner("GET "+ownerDiagnosticsAPI+"/avatars/{hash}", h.handleAdminAvatarImage)
	owner("POST "+ownerDiagnosticsAPI+"/avatars/senders/{hash}/recheck", h.handleRecheckAvatarSender)
	owner("GET "+ownerDiagnosticsAPI+"/contacts/status", h.handleContactAdminStatus)
	owner("GET "+ownerDiagnosticsAPI+"/labels/status", h.handleLabelAdminStatus)
}

// ownerInstance reports whether the instance belongs to its one owner: open
// and personal mode.
func (h *Handler) ownerInstance() bool {
	return h.auth == nil || !h.auth.IsEnabled() || h.auth.IsPersonal()
}

// ownerDiagnostics lets only the owner of an open or personal instance in.
func (h *Handler) ownerDiagnostics(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.ownerInstance() || auth.GetCurrentUser(r.Context()) == nil {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

// ownerDiagnosticsRequest reports whether r came from Settings →
// Diagnostics rather than the console, for answers that lead back.
func ownerDiagnosticsRequest(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, views.OwnerDiagnosticsPath+"/") || strings.HasPrefix(r.URL.Path, ownerDiagnosticsAPI+"/")
}

// diagnosticsAPIBase is where r's diagnostics data lives, for addresses
// handed out in it, such as avatar images.
func diagnosticsAPIBase(r *http.Request) string {
	if ownerDiagnosticsRequest(r) {
		return ownerDiagnosticsAPI
	}
	return "/api/admin"
}

// ownerDiagnosticsScope is everyone's data, which on an owner's instance is
// the owner's, shown at the owner's addresses.
func ownerDiagnosticsScope() models.AdminWebmailScope {
	return models.AdminWebmailScope{Owner: true}
}

func (h *Handler) handleOwnerAvatarDiagnostics(w http.ResponseWriter, r *http.Request) {
	tab, ok := adminAvatarTab(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	status, err := h.avatarStatus(r.Context(), ownerDiagnosticsScope())
	if err != nil {
		log.Printf("owner diagnostics: avatar status: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_get_admin_status"), http.StatusInternalServerError)
		return
	}
	status.Scope = ownerDiagnosticsScope()
	h.renderOwnerDiagnostics(w, r, "avatars", views.AdminPage(status, tab))
}

func (h *Handler) handleOwnerContactDiagnostics(w http.ResponseWriter, r *http.Request) {
	status, err := h.contactAdminStatus(r.Context(), ownerDiagnosticsScope())
	if err != nil {
		log.Printf("owner diagnostics: contact status: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_get_contact_admin_status"), http.StatusInternalServerError)
		return
	}
	status.Scope = ownerDiagnosticsScope()
	h.renderOwnerDiagnostics(w, r, "contacts", views.AdminContactsPage(status))
}

func (h *Handler) handleOwnerLabelDiagnostics(w http.ResponseWriter, r *http.Request) {
	status, err := h.labelAdminStatus(r.Context(), ownerDiagnosticsScope())
	if err != nil {
		log.Printf("owner diagnostics: label status: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_get_label_admin_status"), http.StatusInternalServerError)
		return
	}
	status.Scope = ownerDiagnosticsScope()
	h.renderOwnerDiagnostics(w, r, "labels", views.AdminLabelsPage(status))
}

// renderOwnerDiagnostics answers with a whole Settings page, or with its
// content for a page that's already open.
func (h *Handler) renderOwnerDiagnostics(w http.ResponseWriter, r *http.Request, page string, content templ.Component) {
	ctx := r.Context()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Header.Get("HX-Request") == "true" {
		_ = views.OwnerDiagnosticsPartial(page, content).Render(ctx, w)
		return
	}
	uiSettings, err := h.securityUISettings(ctx, h.userID(ctx))
	if err != nil {
		log.Printf("owner diagnostics: load display preferences: %v", err)
		http.Error(w, i18n.T(ctx, "server.error.user_settings_unavailable"), http.StatusServiceUnavailable)
		return
	}
	_ = views.OwnerDiagnosticsLayout(uiSettings, page, content).Render(ctx, w)
}
