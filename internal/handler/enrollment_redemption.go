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
	enrollmentRedemptionPath               = "/account/redeem"
	enrollmentGoogleRedemptionPath         = "/account/redeem/google"
	enrollmentRedemptionCompletePath       = "/account/redeem/complete"
	enrollmentRedemptionFormMaximumBytes   = 12 << 10
	enrollmentRedemptionFailureMessage     = "That invitation or reset token is invalid or no longer active."
	enrollmentRedemptionServiceMessage     = "Unable to set the password right now. Please try again."
	enrollmentRedemptionMFARequiredMessage = "This account must enroll a strong authenticator before it can be activated. Contact an administrator to complete MFA enrollment."
)

func (h *Handler) handleEnrollmentRedemption(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	message := ""
	if r.URL.Query().Get("google_failed") == "1" {
		message = "Unable to complete Google sign-in with that invitation. The invitation was not consumed; please try again."
	}
	h.renderEnrollmentRedemptionPage(w, r, http.StatusOK, message)
}

func (h *Handler) handleEnrollmentGoogleRedemption(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() || !h.auth.HasGoogleLogin() {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, enrollmentRedemptionFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderEnrollmentRedemptionPage(w, r, http.StatusBadRequest, enrollmentRedemptionFailureMessage)
		return
	}
	clearLegacyOAuthStateCookie(w, h.auth.Config().SecureCookies)
	if previousToken := auth.GetPreAuthToken(r); previousToken != "" {
		err := h.auth.TerminateGoogleAuthorizationChallenge(r.Context(), previousToken)
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		if err != nil && !errors.Is(err, auth.ErrPreAuthChallengeInvalid) {
			log.Printf("replace Google enrollment challenge: %v", err)
			h.renderEnrollmentRedemptionPage(w, r, http.StatusInternalServerError, enrollmentRedemptionServiceMessage)
			return
		}
	}

	start, err := h.auth.BeginGoogleEnrollment(r.Context(), r.PostFormValue("token"))
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrEnrollmentTokenInvalid):
			h.renderEnrollmentRedemptionPage(w, r, http.StatusBadRequest, enrollmentRedemptionFailureMessage)
		case errors.Is(err, auth.ErrInstanceMFAEnrollmentNeeded):
			h.renderEnrollmentRedemptionPage(w, r, http.StatusConflict, enrollmentRedemptionMFARequiredMessage)
		default:
			log.Printf("start Google invitation enrollment: %v", err)
			h.renderEnrollmentRedemptionPage(w, r, http.StatusInternalServerError, enrollmentRedemptionServiceMessage)
		}
		return
	}
	if start == nil || start.Challenge == nil || start.Challenge.Token == "" {
		log.Printf("start Google invitation enrollment returned incomplete authorization")
		h.renderEnrollmentRedemptionPage(w, r, http.StatusInternalServerError, enrollmentRedemptionServiceMessage)
		return
	}
	auth.SetPreAuthCookie(
		w, start.Challenge.Token, h.auth.Config().SecureCookies,
		start.Challenge.ExpiresAt.Sub(start.Challenge.CreatedAt),
	)
	http.Redirect(w, r, start.AuthorizationURL, http.StatusTemporaryRedirect)
}

func (h *Handler) handleEnrollmentRedemptionSubmit(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, enrollmentRedemptionFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderEnrollmentRedemptionPage(w, r, http.StatusBadRequest, enrollmentRedemptionFailureMessage)
		return
	}
	newPassword := r.PostFormValue("new_password")
	if newPassword != r.PostFormValue("confirm_password") {
		h.renderEnrollmentRedemptionPage(w, r, http.StatusBadRequest, "The new password fields do not match.")
		return
	}

	result, err := h.auth.RedeemEnrollmentToken(r.Context(), auth.RedeemEnrollmentTokenOptions{
		Token:       r.PostFormValue("token"),
		NewPassword: newPassword,
		UserAgent:   r.UserAgent(),
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrEnrollmentTokenInvalid):
			h.renderEnrollmentRedemptionPage(w, r, http.StatusBadRequest, enrollmentRedemptionFailureMessage)
		case errors.Is(err, auth.ErrPasswordInvalid),
			errors.Is(err, auth.ErrPasswordTooShort),
			errors.Is(err, auth.ErrPasswordTooLong),
			errors.Is(err, auth.ErrPasswordCommon):
			h.renderEnrollmentRedemptionPage(w, r, http.StatusBadRequest, err.Error())
		case errors.Is(err, auth.ErrInstanceMFAEnrollmentNeeded):
			h.renderEnrollmentRedemptionPage(w, r, http.StatusConflict, enrollmentRedemptionMFARequiredMessage)
		default:
			log.Printf("redeem enrollment token: %v", err)
			h.renderEnrollmentRedemptionPage(w, r, http.StatusInternalServerError, enrollmentRedemptionServiceMessage)
		}
		return
	}

	auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
	auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	destination := enrollmentRedemptionCompletePath
	if result != nil && result.UserType == auth.UserTypeManagement {
		destination += "?management=1"
	}
	http.Redirect(w, r, destination, http.StatusSeeOther)
}

func (h *Handler) handleEnrollmentRedemptionComplete(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	var page bytes.Buffer
	component := views.EnrollmentRedemptionCompletePage()
	if r.URL.Query().Get("management") == "1" {
		component = views.ManagementEnrollmentCompletePage()
	}
	if err := component.Render(r.Context(), &page); err != nil {
		log.Printf("render enrollment redemption completion: %v", err)
		http.Error(w, "failed to render password confirmation", http.StatusInternalServerError)
		return
	}
	writeEnrollmentRedemptionPage(w, http.StatusOK, &page)
}

func (h *Handler) renderEnrollmentRedemptionPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	var page bytes.Buffer
	if err := views.EnrollmentRedemptionPage(views.EnrollmentRedemptionData{
		Message: message, GoogleLoginAvailable: h.auth.HasGoogleLogin(),
	}).Render(r.Context(), &page); err != nil {
		log.Printf("render enrollment redemption page: %v", err)
		http.Error(w, "failed to render password form", http.StatusInternalServerError)
		return
	}
	writeEnrollmentRedemptionPage(w, status, &page)
}

func writeEnrollmentRedemptionPage(w http.ResponseWriter, status int, page *bytes.Buffer) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.WriteHeader(status)
	_, _ = page.WriteTo(w)
}
