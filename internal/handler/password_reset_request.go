package handler

import (
	"bytes"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const (
	passwordResetRequestPath             = "/account/recover"
	passwordResetRequestFormMaximumBytes = 4 << 10
)

func (h *Handler) handlePasswordResetRequest(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil || !h.auth.IsEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.renderPasswordResetRequestPage(w, r, http.StatusOK, views.PasswordResetRequestData{})
}

func (h *Handler) handlePasswordResetRequestSubmit(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil || !h.auth.IsEnabled() {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, passwordResetRequestFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderPasswordResetRequestPage(w, r, http.StatusBadRequest, views.PasswordResetRequestData{Message: i18n.T(r.Context(), "auth.reset.unavailable")})
		return
	}
	if err := h.auth.RequestPasswordReset(r.Context(), auth.PasswordResetRequestOptions{
		Identifier: boundedLoginIdentifier(r.PostFormValue("identifier")),
		Source:     loginSource(r),
		UserAgent:  r.UserAgent(),
	}); err != nil {
		log.Printf("request password reset: %v", err)
		h.renderPasswordResetRequestPage(w, r, http.StatusInternalServerError, views.PasswordResetRequestData{Message: i18n.T(r.Context(), "auth.reset.unavailable")})
		return
	}
	h.renderPasswordResetRequestPage(w, r, http.StatusAccepted, views.PasswordResetRequestData{Submitted: true})
}

func (h *Handler) renderPasswordResetRequestPage(w http.ResponseWriter, r *http.Request, status int, data views.PasswordResetRequestData) {
	var page bytes.Buffer
	if err := views.PasswordResetRequestPage(data).Render(r.Context(), &page); err != nil {
		log.Printf("render password reset request: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_render_password_reset_request"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.WriteHeader(status)
	_, _ = page.WriteTo(w)
}
