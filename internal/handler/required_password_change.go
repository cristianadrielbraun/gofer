package handler

import (
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) requiredPasswordChangeSession(w http.ResponseWriter, r *http.Request) bool {
	user, session := auth.GetCurrentUser(r.Context()), auth.GetCurrentSession(r.Context())
	if h.auth == nil || !h.auth.IsEnabled() || user == nil || user.UserType != auth.UserTypeWebmail || session == nil {
		http.NotFound(w, r)
		return false
	}
	if !session.PasswordChangeRequired {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return false
	}
	return true
}

func (h *Handler) handleRequiredPasswordChange(w http.ResponseWriter, r *http.Request) {
	if !h.requiredPasswordChangeSession(w, r) {
		return
	}
	h.renderRequiredPasswordChange(w, r, http.StatusOK, "")
}

func (h *Handler) renderRequiredPasswordChange(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := views.RequiredPasswordChangePage(message,
		auth.CSRFToken(r.Context(), http.MethodPost, auth.RequiredPasswordChangePath),
		auth.CSRFToken(r.Context(), http.MethodPost, "/auth/logout")).Render(r.Context(), w); err != nil {
		log.Printf("render required password change: %v", err)
	}
}

func (h *Handler) handleRequiredPasswordChangeSubmit(w http.ResponseWriter, r *http.Request) {
	if !h.requiredPasswordChangeSession(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, passwordChangeFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderRequiredPasswordChange(w, r, http.StatusBadRequest, i18n.T(r.Context(), "auth.password.form_unreadable"))
		return
	}
	if r.PostFormValue("new_password") != r.PostFormValue("confirm_password") {
		h.renderRequiredPasswordChange(w, r, http.StatusBadRequest, i18n.T(r.Context(), "auth.password.mismatch"))
		return
	}
	result, err := h.auth.ChangePassword(r.Context(), auth.PasswordChangeOptions{
		SessionToken: auth.GetSessionToken(r), CurrentPassword: r.PostFormValue("current_password"),
		NewPassword: r.PostFormValue("new_password"), UserAgent: r.UserAgent(),
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrCurrentPasswordInvalid), errors.Is(err, auth.ErrPasswordUnchanged),
			errors.Is(err, auth.ErrPasswordInvalid), errors.Is(err, auth.ErrPasswordTooShort),
			errors.Is(err, auth.ErrPasswordTooLong), errors.Is(err, auth.ErrPasswordCommon):
			h.renderRequiredPasswordChange(w, r, http.StatusBadRequest, authText(r.Context(), err.Error()))
		case errors.Is(err, auth.ErrSessionNotActive), errors.Is(err, auth.ErrRecentStepUpRequired):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			log.Printf("complete required password change: %v", err)
			h.renderRequiredPasswordChange(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "auth.password.change_unavailable"))
		}
		return
	}
	auth.SetSessionCookie(w, result.Session.Token, h.auth.Config().SecureCookies)
	auth.ClearReturnToCookie(w, h.auth.Config().SecureCookies)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) handleRequireUserPasswordChange(w http.ResponseWriter, r *http.Request) {
	user, session := auth.GetCurrentUser(r.Context()), auth.GetCurrentSession(r.Context())
	if user == nil || session == nil || h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	err := h.auth.RequirePasswordChange(r.Context(), auth.RequirePasswordChangeOptions{UserID: r.PathValue("userID"), CreatedBy: user.ID, ActorSessionID: session.ID})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			h.renderAdminUsers(w, r, http.StatusForbidden, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.verify.require_password"))
		case errors.Is(err, auth.ErrAdministratorRequired):
			http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		case errors.Is(err, auth.ErrEnrollmentTokenTargetInvalid):
			http.NotFound(w, r)
		default:
			log.Printf("require webmail password change: %v", err)
			h.renderAdminUsers(w, r, http.StatusInternalServerError, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.require_password_unavailable"))
		}
		return
	}
	redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.password_change_required"))
}
