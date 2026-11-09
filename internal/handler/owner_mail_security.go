package handler

import (
	"context"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// In personal and open mode Gofer has a single owner, who manages the mail
// security exceptions from Settings, Security; personal mode has no admin area
// at all. The admin handlers serve both places; this marks which page a
// request came from, so results and verification return there.
const ownerMailSecurityBase = "/settings/security/mail"

type ownerMailSecurityKey struct{}

func mailSecuritySurfaceIsOwner(ctx context.Context) bool {
	personal, _ := ctx.Value(ownerMailSecurityKey{}).(bool)
	return personal
}

// ownerManagesMailSecurity reports whether the person using Gofer approves
// mail security exceptions themselves: in personal mode, and in open mode,
// where sign-in is off. In managed mode only administrators do.
func (h *Handler) ownerManagesMailSecurity() bool {
	return h.auth == nil || !h.auth.IsEnabled() || h.auth.IsPersonal()
}

func (h *Handler) ownerMailSecurity(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.ownerManagesMailSecurity() {
			http.NotFound(w, r)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ownerMailSecurityKey{}, true)))
	}
}

func (h *Handler) registerOwnerMailSecurityRoutes(register func(string, http.HandlerFunc)) {
	register("POST "+ownerMailSecurityBase+"/http-discovery", h.ownerMailSecurity(h.handleAddHTTPDiscoveryException))
	register("POST "+ownerMailSecurityBase+"/plaintext", h.ownerMailSecurity(h.handleAddPlaintextTransportException))
	register("POST "+ownerMailSecurityBase+"/private-target", h.ownerMailSecurity(h.handleAddPrivateTargetException))
	register("POST "+ownerMailSecurityBase+"/exceptions/{id}/delete", h.ownerMailSecurity(h.handleDeleteMailSecurityException))
	register("POST "+ownerMailSecurityBase+"/proxies", h.ownerMailSecurity(h.handleSaveOutboundProxy))
	register("POST "+ownerMailSecurityBase+"/proxies/{id}/delete", h.ownerMailSecurity(h.handleDeleteOutboundProxy))
}

// ownerMailSecurityData loads the owner's exceptions for the security
// settings page. It is nil in managed mode.
func (h *Handler) ownerMailSecurityData(r *http.Request) (*models.MailSecurityAdminData, error) {
	if !h.ownerManagesMailSecurity() {
		return nil, nil
	}
	ctx := r.Context()
	exceptions, err := h.mailSecurityExceptions(ctx)
	if err != nil {
		return nil, err
	}
	data := &models.MailSecurityAdminData{
		ActionBase: ownerMailSecurityBase,
		Exceptions: exceptions,
		Notice:     strings.TrimSpace(r.URL.Query().Get("mail_notice")),
		Error:      strings.TrimSpace(r.URL.Query().Get("mail_error")),
		CSRFTokens: map[string]string{},
	}
	for _, action := range []string{"/http-discovery", "/plaintext", "/private-target"} {
		data.CSRFTokens[ownerMailSecurityBase+action] = auth.CSRFToken(ctx, http.MethodPost, ownerMailSecurityBase+action)
	}
	for _, item := range exceptions {
		action := ownerMailSecurityBase + "/exceptions/" + item.ID + "/delete"
		data.CSRFTokens[action] = auth.CSRFToken(ctx, http.MethodPost, action)
	}
	if err := h.addOutboundProxyData(ctx, data, ownerMailSecurityBase); err != nil {
		return nil, err
	}
	return data, nil
}

// renderOpenSecurityTab shows the Security tab when Gofer runs without
// sign-in. With no sign-in methods or sessions to manage, it holds only the
// owner's Mail security card.
func (h *Handler) renderOpenSecurityTab(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data, err := h.ownerMailSecurityData(r)
	if err != nil || data == nil {
		log.Printf("load open mode mail security exceptions: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_load_security_settings"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Header.Get("HX-Request") == "true" {
		_ = views.OpenSecurityPartial(*data).Render(ctx, w)
		return
	}
	uiSettings, err := h.securityUISettings(ctx, h.userID(ctx))
	if err != nil {
		log.Printf("load security display preferences: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.user_settings_unavailable"), http.StatusServiceUnavailable)
		return
	}
	_ = views.OpenSecurityLayout(uiSettings, *data).Render(ctx, w)
}
