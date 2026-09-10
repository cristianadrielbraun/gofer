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
	loginFormMaximumBytes    = 8 << 10
	loginMFAFormMaximumBytes = 4 << 10
	loginFailureMessage      = "Unable to sign in with those credentials."
	loginMFAFailureMessage   = "That authenticator code is invalid or has already been used."
	loginServiceMessage      = "Unable to sign in right now. Please try again."
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
	h.handlePasswordLoginSubmit(w, r, auth.UserTypeWebmail)
}

func (h *Handler) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.NotFound(w, r)
		return
	}
	message := ""
	if r.URL.Query().Get("error") != "" {
		message = "Admin sign-in could not be completed. Please try again."
	}
	h.renderAdminLoginPage(w, r, http.StatusOK, message, "")
}

func (h *Handler) handleAdminLoginSubmit(w http.ResponseWriter, r *http.Request) {
	h.handlePasswordLoginSubmit(w, r, auth.UserTypeManagement)
}

func (h *Handler) handlePasswordLoginSubmit(w http.ResponseWriter, r *http.Request, requiredType auth.UserType) {
	if !h.auth.IsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, loginFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderTypedLoginPage(w, r, requiredType, http.StatusBadRequest, loginFailureMessage, "")
		return
	}
	identifier := boundedLoginIdentifier(r.FormValue("identifier"))
	result, err := h.auth.AuthenticatePassword(r.Context(), auth.PasswordLoginOptions{
		Identifier:       identifier,
		Password:         r.FormValue("password"),
		RequiredUserType: requiredType,
		Source:           directLoginSource(r.RemoteAddr),
		UserAgent:        r.UserAgent(),
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
			h.renderTypedLoginPage(w, r, requiredType, http.StatusTooManyRequests, loginFailureMessage, identifier)
		case errors.Is(err, auth.ErrInvalidCredentials):
			h.renderTypedLoginPage(w, r, requiredType, http.StatusUnauthorized, loginFailureMessage, identifier)
		default:
			log.Printf("local password login failed: %v", err)
			h.renderTypedLoginPage(w, r, requiredType, http.StatusInternalServerError, loginServiceMessage, identifier)
		}
		return
	}

	if result == nil || (result.Session == nil && result.PreAuthChallenge == nil) || (result.Session != nil && result.PreAuthChallenge != nil) {
		log.Printf("local password login returned an invalid completion result")
		h.renderTypedLoginPage(w, r, requiredType, http.StatusInternalServerError, loginServiceMessage, identifier)
		return
	}
	if result.PreAuthChallenge != nil {
		challenge := result.PreAuthChallenge
		auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
		auth.ClearPasskeyLoginChallengeCookie(w, h.auth.Config().SecureCookies)
		if requiredType == auth.UserTypeManagement && !isManagementReturnTarget(auth.GetReturnTo(r)) {
			auth.SetReturnToCookie(w, "/admin", h.auth.Config().SecureCookies)
		}
		auth.SetPreAuthCookie(
			w, challenge.Token, h.auth.Config().SecureCookies,
			challenge.ExpiresAt.Sub(challenge.CreatedAt),
		)
		http.Redirect(w, r, primaryAuthenticationContinuationPath(result), http.StatusSeeOther)
		return
	}

	auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
	auth.ClearPasskeyLoginChallengeCookie(w, h.auth.Config().SecureCookies)
	auth.SetSessionCookie(w, result.Session.Token, h.auth.Config().SecureCookies)
	returnTo := h.loginReturnTo(r, requiredType)
	auth.ClearReturnToCookie(w, h.auth.Config().SecureCookies)
	http.Redirect(w, r, returnTo, http.StatusSeeOther)
}

func (h *Handler) handleLoginMFA(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if r.URL.RawQuery != "" {
		http.Redirect(w, r, "/login/mfa", http.StatusSeeOther)
		return
	}
	token := auth.GetPreAuthToken(r)
	if _, enrollmentErr := h.auth.GetMFAEnrollmentState(r.Context(), token, h.auth.Config().BaseURL); enrollmentErr == nil {
		http.Redirect(w, r, "/login/mfa/enroll", http.StatusSeeOther)
		return
	} else if !errors.Is(enrollmentErr, auth.ErrMFAEnrollmentInvalid) {
		log.Printf("read required MFA enrollment continuation: %v", enrollmentErr)
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		http.Error(w, "failed to load MFA enrollment", http.StatusInternalServerError)
		return
	}
	challenge, err := h.auth.GetActiveMFAChallenge(r.Context(), token, h.auth.Config().BaseURL)
	if err != nil {
		log.Printf("read MFA continuation: %v", err)
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		http.Error(w, "failed to load additional verification", http.StatusInternalServerError)
		return
	}
	if challenge == nil {
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		loginPath := "/login"
		if isManagementReturnTarget(auth.GetReturnTo(r)) {
			loginPath = "/admin/login"
		}
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
		return
	}
	h.renderLoginMFAContinuationPage(w, r, http.StatusOK, "", h.challengeIsManagement(r, challenge))
}

func (h *Handler) handleLoginMFASubmit(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.URL.RawQuery != "" {
		http.Redirect(w, r, "/login/mfa", http.StatusSeeOther)
		return
	}
	management := false
	if challenge, challengeErr := h.auth.GetActiveMFAChallenge(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL); challengeErr == nil && challenge != nil {
		management = h.challengeIsManagement(r, challenge)
	}
	r.Body = http.MaxBytesReader(w, r.Body, loginMFAFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderLoginMFAContinuationPage(w, r, http.StatusBadRequest, loginMFAFailureMessage, management)
		return
	}
	session, err := h.auth.CompleteTOTPLogin(r.Context(), auth.TOTPLoginOptions{
		Token:     auth.GetPreAuthToken(r),
		Code:      r.PostFormValue("code"),
		Origin:    h.auth.Config().BaseURL,
		Source:    directLoginSource(r.RemoteAddr),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		var throttleError *auth.LoginThrottleError
		var validationError *auth.TOTPLoginValidationError
		switch {
		case errors.As(err, &throttleError):
			retrySeconds := int64((throttleError.RetryAfter + time.Second - 1) / time.Second)
			if retrySeconds < 1 {
				retrySeconds = 1
			}
			w.Header().Set("Retry-After", strconv.FormatInt(retrySeconds, 10))
			h.renderLoginMFAContinuationPage(w, r, http.StatusTooManyRequests, loginMFAFailureMessage, management)
		case errors.As(err, &validationError) && !validationError.Terminal:
			h.renderLoginMFAContinuationPage(w, r, http.StatusUnauthorized, loginMFAFailureMessage, management)
		case errors.Is(err, auth.ErrTOTPLoginChallengeInvalid), errors.As(err, &validationError):
			auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
			loginPath := "/login?error=mfa"
			if management || isManagementReturnTarget(auth.GetReturnTo(r)) {
				loginPath = "/admin/login?error=mfa"
			}
			http.Redirect(w, r, loginPath, http.StatusSeeOther)
		default:
			log.Printf("complete TOTP login: %v", err)
			h.renderLoginMFAContinuationPage(w, r, http.StatusInternalServerError, loginServiceMessage, management)
		}
		return
	}
	if session == nil {
		log.Printf("TOTP login returned no session")
		h.renderLoginMFAContinuationPage(w, r, http.StatusInternalServerError, loginServiceMessage, management)
		return
	}

	auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
	auth.SetSessionCookie(w, session.Token, h.auth.Config().SecureCookies)
	userType := auth.UserTypeWebmail
	if user, userErr := h.auth.GetUserByID(r.Context(), session.UserID); userErr == nil && user != nil {
		userType = user.UserType
	}
	returnTo := h.loginReturnTo(r, userType)
	auth.ClearReturnToCookie(w, h.auth.Config().SecureCookies)
	http.Redirect(w, r, returnTo, http.StatusSeeOther)
}

func (h *Handler) renderTypedLoginPage(w http.ResponseWriter, r *http.Request, userType auth.UserType, status int, message, identifier string) {
	if userType == auth.UserTypeManagement {
		h.renderAdminLoginPage(w, r, status, message, identifier)
		return
	}
	h.renderLoginPage(w, r, status, message, identifier)
}

func (h *Handler) renderLoginPage(w http.ResponseWriter, r *http.Request, status int, message, identifier string) {
	var page bytes.Buffer
	if err := views.LoginPage(
		h.auth.HasGoogleLogin(), h.auth.HasMicrosoftLogin(), h.auth.OIDCLoginName(), message, identifier,
	).Render(r.Context(), &page); err != nil {
		log.Printf("render login page: %v", err)
		http.Error(w, "failed to render sign-in page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = page.WriteTo(w)
}

func (h *Handler) renderAdminLoginPage(w http.ResponseWriter, r *http.Request, status int, message, identifier string) {
	var page bytes.Buffer
	if err := views.ManagementLoginPage(message, identifier).Render(r.Context(), &page); err != nil {
		log.Printf("render admin sign-in page: %v", err)
		http.Error(w, "failed to render admin sign-in page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = page.WriteTo(w)
}

func (h *Handler) renderLoginMFAContinuationPage(w http.ResponseWriter, r *http.Request, status int, message string, management bool) {
	var page bytes.Buffer
	factors, err := h.auth.GetMFAContinuationFactors(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		loginPath := "/login"
		if management {
			loginPath = "/admin/login"
		}
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
		return
	}
	component := views.LoginMFAContinuationPage(message, views.LoginMFAFactors{Username: factors.Username, HasTOTP: factors.HasTOTP, HasPasskey: factors.HasPasskey})
	if management {
		component = views.ManagementMFAContinuationPage(message)
	}
	if err := component.Render(r.Context(), &page); err != nil {
		log.Printf("render password MFA continuation page: %v", err)
		http.Error(w, "failed to render additional verification page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = page.WriteTo(w)
}

func (h *Handler) challengeIsManagement(r *http.Request, challenge *auth.PreAuthChallenge) bool {
	if challenge == nil {
		return false
	}
	user, err := h.auth.GetUserByID(r.Context(), challenge.UserID)
	return err == nil && user != nil && user.IsManagement()
}

func (h *Handler) loginReturnTo(r *http.Request, userType auth.UserType) string {
	returnTo := auth.GetReturnTo(r)
	if userType == auth.UserTypeManagement {
		if isManagementReturnTarget(returnTo) {
			return returnTo
		}
		return "/admin"
	}
	if isManagementReturnTarget(returnTo) {
		returnTo = ""
	}
	if returnTo == "" {
		return "/"
	}
	return returnTo
}

func primaryAuthenticationContinuationPath(result *auth.PrimaryAuthenticationResult) string {
	if result != nil && result.MFAEnrollmentRequired {
		return "/login/mfa/enroll"
	}
	return "/login/mfa"
}

func isManagementReturnTarget(target string) bool {
	return target == "/admin" || strings.HasPrefix(target, "/admin/")
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
