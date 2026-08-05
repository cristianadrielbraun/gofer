package handler

import (
	"bytes"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const (
	loginFormMaximumBytes = 8 << 10
	loginFailureMessage   = "Unable to sign in with those credentials."
	loginServiceMessage   = "Unable to sign in right now. Please try again."
)

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	message := ""
	if r.URL.Query().Get("error") != "" {
		message = "Sign-in could not be completed. Please try again."
	}
	h.renderLoginPage(w, r, http.StatusOK, message, "")
}

func (h *Handler) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, loginFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderLoginPage(w, r, http.StatusBadRequest, loginFailureMessage, "")
		return
	}
	identifier := boundedLoginIdentifier(r.FormValue("identifier"))
	result, err := h.auth.AuthenticatePassword(r.Context(), auth.PasswordLoginOptions{
		Identifier: identifier,
		Password:   r.FormValue("password"),
		Source:     directLoginSource(r.RemoteAddr),
		UserAgent:  r.UserAgent(),
	})
	if err != nil {
		var throttleError *auth.LoginThrottleError
		switch {
		case errors.As(err, &throttleError):
			retrySeconds := int64((throttleError.RetryAfter + time.Second - 1) / time.Second)
			if retrySeconds < 1 {
				retrySeconds = 1
			}
			w.Header().Set("Retry-After", strconv.FormatInt(retrySeconds, 10))
			h.renderLoginPage(w, r, http.StatusTooManyRequests, loginFailureMessage, identifier)
		case errors.Is(err, auth.ErrInvalidCredentials):
			h.renderLoginPage(w, r, http.StatusUnauthorized, loginFailureMessage, identifier)
		default:
			log.Printf("local password login failed: %v", err)
			h.renderLoginPage(w, r, http.StatusInternalServerError, loginServiceMessage, identifier)
		}
		return
	}

	if result == nil || (result.Session == nil && result.PreAuthChallenge == nil) || (result.Session != nil && result.PreAuthChallenge != nil) {
		log.Printf("local password login returned an invalid completion result")
		h.renderLoginPage(w, r, http.StatusInternalServerError, loginServiceMessage, identifier)
		return
	}
	if result.PreAuthChallenge != nil {
		challenge := result.PreAuthChallenge
		auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
		auth.SetPreAuthCookie(
			w, challenge.Token, h.auth.Config().SecureCookies,
			challenge.ExpiresAt.Sub(challenge.CreatedAt),
		)
		http.Redirect(w, r, "/login/mfa", http.StatusSeeOther)
		return
	}

	auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
	auth.SetSessionCookie(w, result.Session.Token, h.auth.Config().SecureCookies)
	returnTo := auth.GetReturnTo(r)
	auth.ClearReturnToCookie(w, h.auth.Config().SecureCookies)
	if returnTo == "" {
		returnTo = "/"
	}
	http.Redirect(w, r, returnTo, http.StatusSeeOther)
}

func (h *Handler) handleLoginMFA(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	token := auth.GetPreAuthToken(r)
	challenge, err := h.auth.GetActivePreAuthChallenge(
		r.Context(), token, auth.ChallengePurposeMFA, h.auth.Config().BaseURL,
	)
	if err != nil {
		log.Printf("read password MFA continuation: %v", err)
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		http.Error(w, "failed to load additional verification", http.StatusInternalServerError)
		return
	}
	if challenge == nil {
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	h.renderLoginMFAContinuationPage(w, r)
}

func (h *Handler) renderLoginPage(w http.ResponseWriter, r *http.Request, status int, message, identifier string) {
	var page bytes.Buffer
	if err := views.LoginPage(h.auth.HasGoogleOAuth(), message, identifier).Render(r.Context(), &page); err != nil {
		log.Printf("render login page: %v", err)
		http.Error(w, "failed to render sign-in page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = page.WriteTo(w)
}

func (h *Handler) renderLoginMFAContinuationPage(w http.ResponseWriter, r *http.Request) {
	var page bytes.Buffer
	if err := views.LoginMFAContinuationPage().Render(r.Context(), &page); err != nil {
		log.Printf("render password MFA continuation page: %v", err)
		http.Error(w, "failed to render additional verification page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = page.WriteTo(w)
}

func directLoginSource(remoteAddress string) string {
	remoteAddress = strings.TrimSpace(remoteAddress)
	if host, _, err := net.SplitHostPort(remoteAddress); err == nil {
		return strings.Trim(host, "[]")
	}
	return remoteAddress
}

func boundedLoginIdentifier(value string) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	for len(value) > 254 {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}
