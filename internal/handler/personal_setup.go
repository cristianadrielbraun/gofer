package handler

import (
	"bytes"
	"errors"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handlePersonalSetup(w http.ResponseWriter, r *http.Request) {
	state, err := h.auth.SetupState(r.Context())
	if err != nil {
		h.writeSetupServiceFailure(w)
		return
	}
	if state.Initialized {
		h.writeSetupNotFound(w)
		return
	}
	if r.URL.RawQuery != "" {
		h.redirectSetupOwnerWithoutQuery(w, r)
		return
	}
	access, err := h.auth.GetActiveSetupAccess(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.writeSetupServiceFailure(w)
		return
	}
	if access == nil {
		http.Redirect(w, r, setupPath, http.StatusSeeOther)
		return
	}
	data := views.PersonalSetupData{}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, setupFormMaximumBytes)
		if err := r.ParseForm(); err != nil {
			h.writeSetupServiceFailure(w)
			return
		}
		data.Name = r.PostFormValue("name")
		data.Username = r.PostFormValue("username")
		result, err := h.auth.CompletePersonalSetup(r.Context(), auth.PersonalSetupInput{Token: auth.GetPreAuthToken(r), Origin: h.auth.Config().BaseURL, UserAgent: r.UserAgent(), Name: data.Name, Username: data.Username, Password: r.PostFormValue("password"), PasswordConfirmation: r.PostFormValue("password_confirmation")})
		if err == nil {
			auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
			auth.SetSessionCookie(w, result.Session.Token, h.auth.Config().SecureCookies)
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, "/settings/security", http.StatusSeeOther)
			return
		}
		var profileErr *auth.SetupOwnerValidationError
		var passwordErr *auth.SetupPasswordValidationError
		status = http.StatusUnprocessableEntity
		switch {
		case errors.As(err, &profileErr):
			data.Errors = profileErr.Fields
		case errors.As(err, &passwordErr):
			data.Errors = passwordErr.Fields
		case errors.Is(err, auth.ErrPersonalSetupBlocked):
			status = http.StatusConflict
			data.Errors = map[string]string{"form": "This database is not an unprotected personal profile. Use its original authentication mode."}
		case errors.Is(err, auth.ErrSetupAccessInvalid), errors.Is(err, auth.ErrSetupAlreadyInitialized):
			http.Redirect(w, r, setupPath, http.StatusSeeOther)
			return
		default:
			log.Printf("complete personal setup: %v", err)
			h.writeSetupServiceFailure(w)
			return
		}
	}
	var page bytes.Buffer
	if err := views.PersonalSetupPage(data).Render(r.Context(), &page); err != nil {
		h.writeSetupServiceFailure(w)
		return
	}
	writeSetupPage(w, status, &page)
}
