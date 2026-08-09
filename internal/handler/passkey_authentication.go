package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

const (
	loginPasskeyStartPath           = "/login/passkey/start"
	loginPasskeyFinishPath          = "/login/passkey/finish"
	securityPasskeyStepUpStartPath  = "/settings/security/passkeys/step-up/start"
	securityPasskeyStepUpFinishPath = "/settings/security/passkeys/step-up/finish"
	passkeyLoginStartMaximumBytes   = 8 << 10
	passkeyAssertionMaximumBytes    = 1 << 20
	passkeyLoginFailureMessage      = "That passkey could not be verified. Try again or use your password."
)

func (h *Handler) handleLoginPasskeyStart(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, passkeyLoginStartMaximumBytes)
	if err := r.ParseForm(); err != nil {
		writePasskeyJSONError(w, http.StatusBadRequest, passkeyLoginFailureMessage)
		return
	}
	options, err := h.auth.StartPasskeyLogin(
		r.Context(), boundedLoginIdentifier(r.PostFormValue("identifier")), h.auth.Config().BaseURL,
		directLoginSource(r.RemoteAddr),
	)
	if err != nil {
		var throttleError *auth.LoginThrottleError
		switch {
		case errors.As(err, &throttleError):
			setPasskeyRetryAfter(w, throttleError)
			writePasskeyJSONError(w, http.StatusTooManyRequests, passkeyLoginFailureMessage)
		case errors.Is(err, auth.ErrPasskeyAuthenticationUnavailable):
			writePasskeyJSONError(w, http.StatusUnprocessableEntity, passkeyLoginFailureMessage)
		default:
			log.Printf("start passkey login: %v", err)
			writePasskeyJSONError(w, http.StatusInternalServerError, "Unable to start passkey sign-in right now.")
		}
		return
	}
	if options == nil || options.Challenge == nil || options.Challenge.Token == "" || len(options.RequestJSON) == 0 {
		log.Printf("start passkey login returned incomplete options")
		writePasskeyJSONError(w, http.StatusInternalServerError, "Unable to start passkey sign-in right now.")
		return
	}
	auth.SetPasskeyLoginChallengeCookie(
		w, options.Challenge.Token, h.auth.Config().SecureCookies,
		options.Challenge.ExpiresAt.Sub(options.Challenge.CreatedAt),
	)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(options.RequestJSON)
}

func (h *Handler) handleLoginPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, passkeyAssertionMaximumBytes)
	responseJSON, err := io.ReadAll(r.Body)
	if err != nil {
		writePasskeyJSONError(w, http.StatusBadRequest, passkeyLoginFailureMessage)
		return
	}
	session, err := h.auth.FinishPasskeyLogin(
		r.Context(), auth.GetPasskeyLoginChallengeToken(r), h.auth.Config().BaseURL,
		responseJSON, directLoginSource(r.RemoteAddr), r.UserAgent(),
	)
	if err != nil {
		var throttleError *auth.LoginThrottleError
		var validationError *auth.PasskeyAuthenticationValidationError
		switch {
		case errors.As(err, &throttleError):
			auth.ClearPasskeyLoginChallengeCookie(w, h.auth.Config().SecureCookies)
			setPasskeyRetryAfter(w, throttleError)
			writePasskeyJSONError(w, http.StatusTooManyRequests, passkeyLoginFailureMessage)
		case errors.Is(err, auth.ErrPasskeyCloneWarning):
			auth.ClearPasskeyLoginChallengeCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONError(w, http.StatusUnauthorized, "This passkey can no longer be used safely. Sign in another way and replace it.")
		case errors.As(err, &validationError):
			auth.ClearPasskeyLoginChallengeCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONError(w, http.StatusUnauthorized, passkeyLoginFailureMessage)
		case errors.Is(err, auth.ErrPasskeyAuthenticationInvalid):
			auth.ClearPasskeyLoginChallengeCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONError(w, http.StatusGone, "That passkey request expired or was already used. Start again or use your password.")
		default:
			log.Printf("finish passkey login: %v", err)
			writePasskeyJSONError(w, http.StatusInternalServerError, "Unable to complete passkey sign-in right now.")
		}
		return
	}
	if session == nil || session.Token == "" {
		log.Printf("passkey login returned no session")
		writePasskeyJSONError(w, http.StatusInternalServerError, "Unable to complete passkey sign-in right now.")
		return
	}
	auth.ClearPasskeyLoginChallengeCookie(w, h.auth.Config().SecureCookies)
	auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
	auth.SetSessionCookie(w, session.Token, h.auth.Config().SecureCookies)
	returnTo := auth.GetReturnTo(r)
	auth.ClearReturnToCookie(w, h.auth.Config().SecureCookies)
	if returnTo == "" {
		returnTo = "/"
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"redirect": returnTo})
}

func (h *Handler) handleSecurityPasskeyStepUpStart(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, "Unable to read the passkey verification request. Please try again.") {
		return
	}
	options, err := h.auth.StartPasskeyStepUp(
		r.Context(), auth.GetSessionToken(r), h.auth.Config().BaseURL, directLoginSource(r.RemoteAddr),
	)
	if err != nil {
		var throttleError *auth.LoginThrottleError
		switch {
		case errors.As(err, &throttleError):
			setPasskeyRetryAfter(w, throttleError)
			writePasskeyJSONError(w, http.StatusTooManyRequests, passkeyLoginFailureMessage)
		case errors.Is(err, auth.ErrPasskeyAuthenticationUnavailable):
			writePasskeyJSONError(w, http.StatusConflict, "No usable passkey is registered for this account. Use your authenticator code.")
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONError(w, http.StatusUnauthorized, "Your session expired. Sign in again.")
		default:
			log.Printf("start passkey security step-up: %v", err)
			writePasskeyJSONError(w, http.StatusInternalServerError, "Unable to start passkey verification right now.")
		}
		return
	}
	if options == nil || options.Challenge == nil || options.Challenge.Token == "" || len(options.RequestJSON) == 0 {
		log.Printf("start passkey security step-up returned incomplete options")
		writePasskeyJSONError(w, http.StatusInternalServerError, "Unable to start passkey verification right now.")
		return
	}
	auth.SetSecurityChallengeCookie(
		w, options.Challenge.Token, h.auth.Config().SecureCookies,
		options.Challenge.ExpiresAt.Sub(options.Challenge.CreatedAt),
	)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(options.RequestJSON)
}

func (h *Handler) handleSecurityPasskeyStepUpFinish(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, passkeyAssertionMaximumBytes)
	responseJSON, err := io.ReadAll(r.Body)
	if err != nil {
		writePasskeyJSONError(w, http.StatusBadRequest, passkeyLoginFailureMessage)
		return
	}
	err = h.auth.FinishPasskeyStepUp(
		r.Context(), auth.GetSecurityChallengeToken(r), auth.GetSessionToken(r), h.auth.Config().BaseURL,
		responseJSON, directLoginSource(r.RemoteAddr), r.UserAgent(),
	)
	if err != nil {
		var throttleError *auth.LoginThrottleError
		var validationError *auth.PasskeyAuthenticationValidationError
		switch {
		case errors.As(err, &throttleError):
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			setPasskeyRetryAfter(w, throttleError)
			writePasskeyJSONError(w, http.StatusTooManyRequests, passkeyLoginFailureMessage)
		case errors.Is(err, auth.ErrPasskeyCloneWarning):
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONError(w, http.StatusUnauthorized, "This passkey can no longer be used safely. Use your authenticator code and replace it.")
		case errors.As(err, &validationError):
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONError(w, http.StatusUnauthorized, passkeyLoginFailureMessage)
		case errors.Is(err, auth.ErrPasskeyAuthenticationInvalid):
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONError(w, http.StatusGone, "That passkey request expired or was already used. Start again.")
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONError(w, http.StatusUnauthorized, "Your session expired. Sign in again.")
		default:
			log.Printf("finish passkey security step-up: %v", err)
			writePasskeyJSONError(w, http.StatusInternalServerError, "Unable to verify this session right now.")
		}
		return
	}
	auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"redirect": "/settings/security?verified=1"})
}

func setPasskeyRetryAfter(w http.ResponseWriter, throttleError *auth.LoginThrottleError) {
	retrySeconds := int64((throttleError.RetryAfter + time.Second - 1) / time.Second)
	if retrySeconds < 1 {
		retrySeconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(retrySeconds, 10))
}
