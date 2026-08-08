package handler

import (
	"bytes"
	"errors"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const (
	setupPath                = "/setup"
	setupOwnerPath           = "/setup/owner"
	setupFormMaximumBytes    = 4 << 10
	setupTokenFailureMessage = "That setup token is invalid or no longer active."
	setupTokenServiceMessage = "Unable to verify the setup token right now. Please try again."
)

func (h *Handler) handleSetup(w http.ResponseWriter, r *http.Request) {
	state, err := h.auth.SetupState(r.Context())
	if err != nil {
		log.Printf("read setup entry state: %v", err)
		h.renderSetupPage(w, r, http.StatusInternalServerError, setupTokenServiceMessage)
		return
	}
	if state.Initialized {
		h.writeSetupNotFound(w)
		return
	}
	if r.URL.RawQuery != "" {
		h.redirectSetupWithoutQuery(w, r)
		return
	}

	challenge, err := h.auth.GetActiveSetupAccess(
		r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL,
	)
	if err != nil {
		log.Printf("read setup access challenge: %v", err)
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		h.renderSetupPage(w, r, http.StatusInternalServerError, setupTokenServiceMessage)
		return
	}
	if challenge != nil {
		http.Redirect(w, r, setupOwnerPath, http.StatusSeeOther)
		return
	}
	h.renderSetupPage(w, r, http.StatusOK, "")
}

func (h *Handler) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	state, err := h.auth.SetupState(r.Context())
	if err != nil {
		log.Printf("read setup submission state: %v", err)
		h.renderSetupPage(w, r, http.StatusInternalServerError, setupTokenServiceMessage)
		return
	}
	if state.Initialized {
		h.writeSetupNotFound(w)
		return
	}
	if r.URL.RawQuery != "" {
		h.redirectSetupWithoutQuery(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, setupFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderSetupPage(w, r, http.StatusUnauthorized, setupTokenFailureMessage)
		return
	}
	challenge, err := h.auth.BeginSetup(r.Context(), auth.BeginSetupOptions{
		Token: r.PostFormValue("token"), Origin: h.auth.Config().BaseURL, UserAgent: r.UserAgent(),
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrSetupAlreadyInitialized):
			h.writeSetupNotFound(w)
		case errors.Is(err, auth.ErrSetupTokenInvalid):
			h.renderSetupPage(w, r, http.StatusUnauthorized, setupTokenFailureMessage)
		default:
			log.Printf("verify setup token: %v", err)
			h.renderSetupPage(w, r, http.StatusInternalServerError, setupTokenServiceMessage)
		}
		return
	}
	if challenge == nil {
		log.Printf("setup token verification returned no access challenge")
		h.renderSetupPage(w, r, http.StatusInternalServerError, setupTokenServiceMessage)
		return
	}

	auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
	auth.ClearReturnToCookie(w, h.auth.Config().SecureCookies)
	auth.SetPreAuthCookie(
		w, challenge.Token, h.auth.Config().SecureCookies,
		challenge.ExpiresAt.Sub(challenge.CreatedAt),
	)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, setupOwnerPath, http.StatusSeeOther)
}

func (h *Handler) handleSetupOwner(w http.ResponseWriter, r *http.Request) {
	state, err := h.auth.SetupState(r.Context())
	if err != nil {
		log.Printf("read protected setup state: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	if state.Initialized {
		h.writeSetupNotFound(w)
		return
	}
	challenge, err := h.auth.GetActiveSetupAccess(
		r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL,
	)
	if err != nil {
		log.Printf("read protected setup access: %v", err)
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		h.writeSetupServiceFailure(w)
		return
	}
	if challenge == nil {
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		http.Redirect(w, r, setupPath, http.StatusSeeOther)
		return
	}

	var page bytes.Buffer
	if err := views.SetupOwnerPage().Render(r.Context(), &page); err != nil {
		log.Printf("render protected setup owner page: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	writeSetupPage(w, http.StatusOK, &page)
}

func (h *Handler) renderSetupPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	var page bytes.Buffer
	if err := views.SetupTokenPage(views.SetupTokenData{Message: message}).Render(r.Context(), &page); err != nil {
		log.Printf("render setup token page: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	writeSetupPage(w, status, &page)
}

func (h *Handler) writeSetupNotFound(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Error(w, "not found", http.StatusNotFound)
}

func (h *Handler) writeSetupServiceFailure(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Error(w, setupTokenServiceMessage, http.StatusInternalServerError)
}

func (h *Handler) redirectSetupWithoutQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, setupPath, http.StatusSeeOther)
}

func writeSetupPage(w http.ResponseWriter, status int, page *bytes.Buffer) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.WriteHeader(status)
	_, _ = page.WriteTo(w)
}
