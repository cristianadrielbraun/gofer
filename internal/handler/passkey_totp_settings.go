package handler

import (
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"net/http"
)

func (h *Handler) handleSecurityPasskeyTOTP(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, i18n.T(r.Context(), "auth.security.update_unavailable")) {
		return
	}
	value := r.PostFormValue("required")
	if value != "" && value != "1" {
		http.Error(w, i18n.T(r.Context(), "auth.security.update_unavailable"), http.StatusBadRequest)
		return
	}
	session, err := h.auth.SetPasskeyTOTPRequired(r.Context(), auth.GetSessionToken(r), value == "1", r.UserAgent())
	switch {
	case errors.Is(err, auth.ErrRecentStepUpRequired):
		http.Redirect(w, r, "/settings/security?verify=1", http.StatusSeeOther)
	case errors.Is(err, auth.ErrPasskeyTOTPPolicyEnforced):
		h.renderSecurityManagementError(w, r, http.StatusConflict, i18n.T(r.Context(), "security.passkeys.extra_totp_locked"))
	case errors.Is(err, auth.ErrSecuritySessionInvalid):
		auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	case err != nil:
		log.Printf("set passkey extra TOTP requirement: %v", err)
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "auth.security.update_unavailable"))
	case session == nil:
		// Requiring the extra factor invalidated a passkey-only session.
		auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	default:
		auth.SetSessionCookie(w, session.Token, h.auth.Config().SecureCookies)
		http.Redirect(w, r, "/settings/security", http.StatusSeeOther)
	}
}
