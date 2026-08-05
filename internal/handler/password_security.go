package handler

import (
	"errors"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const (
	passwordChangePath             = "/settings/security/password"
	passwordChangeFormMaximumBytes = 8 << 10
)

func (h *Handler) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, passwordChangeFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderPasswordChangeError(w, r, http.StatusBadRequest, "Unable to read the password form. Please try again.")
		return
	}
	newPassword := r.PostFormValue("new_password")
	if newPassword != r.PostFormValue("confirm_password") {
		h.renderPasswordChangeError(w, r, http.StatusBadRequest, "The new password fields do not match.")
		return
	}

	result, err := h.auth.ChangePassword(r.Context(), auth.PasswordChangeOptions{
		SessionToken:    auth.GetSessionToken(r),
		CurrentPassword: r.PostFormValue("current_password"),
		NewPassword:     newPassword,
		UserAgent:       r.UserAgent(),
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrCurrentPasswordInvalid),
			errors.Is(err, auth.ErrPasswordUnchanged),
			errors.Is(err, auth.ErrPasswordInvalid),
			errors.Is(err, auth.ErrPasswordTooShort),
			errors.Is(err, auth.ErrPasswordTooLong),
			errors.Is(err, auth.ErrPasswordCommon):
			h.renderPasswordChangeError(w, r, http.StatusBadRequest, err.Error())
		case errors.Is(err, auth.ErrSessionNotActive):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			log.Printf("change local password: %v", err)
			h.renderPasswordChangeError(w, r, http.StatusInternalServerError, "Unable to change the password right now. Please try again.")
		}
		return
	}
	if result == nil || result.Session == nil {
		log.Printf("change local password returned no rotated session")
		h.renderPasswordChangeError(w, r, http.StatusInternalServerError, "Unable to change the password right now. Please try again.")
		return
	}

	auth.SetSessionCookie(w, result.Session.Token, h.auth.Config().SecureCookies)
	http.Redirect(w, r, "/settings/security?password_changed=1", http.StatusSeeOther)
}

func (h *Handler) renderPasswordChangeError(w http.ResponseWriter, r *http.Request, status int, message string) {
	h.renderPasswordSecurityTab(w, r, status, &views.PasswordSecurityData{
		HasPassword:    true,
		Message:        message,
		MessageIsError: true,
		CSRFToken:      auth.CSRFToken(r.Context(), http.MethodPost, passwordChangePath),
	})
}
