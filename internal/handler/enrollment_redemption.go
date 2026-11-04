package handler

import (
	"bytes"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"net/http"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const (
	invitationEnrollmentPath             = "/account/enroll"
	invitationGoogleEnrollmentPath       = "/account/enroll/google"
	invitationEnrollmentCompletePath     = "/account/enroll/complete"
	credentialRedemptionPath             = "/account/redeem"
	credentialRedemptionCompletePath     = "/account/redeem/complete"
	enrollmentRedemptionFormMaximumBytes = 12 << 10
)

func (h *Handler) handleInvitationEnrollment(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	message := ""
	if r.URL.Query().Get("google_failed") == "1" {
		message = i18n.T(r.Context(), "auth.invitation.google_failed")
	}
	h.renderInvitationEnrollmentPage(w, r, http.StatusOK, message)
}

func (h *Handler) handleInvitationGoogleEnrollment(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() || !h.auth.HasGoogleLogin() {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, enrollmentRedemptionFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderInvitationEnrollmentPage(w, r, http.StatusBadRequest, i18n.T(r.Context(), "auth.invitation.invalid"))
		return
	}
	clearLegacyOAuthStateCookie(w, h.auth.Config().SecureCookies)
	if previousToken := auth.GetPreAuthToken(r); previousToken != "" {
		err := h.auth.TerminateGoogleAuthorizationChallenge(r.Context(), previousToken)
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		if err != nil && !errors.Is(err, auth.ErrPreAuthChallengeInvalid) {
			log.Printf("replace Google enrollment challenge: %v", err)
			h.renderInvitationEnrollmentPage(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "auth.enrollment.unavailable"))
			return
		}
	}

	start, err := h.auth.BeginGoogleEnrollment(r.Context(), r.PostFormValue("token"))
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrEnrollmentTokenInvalid):
			h.renderInvitationEnrollmentPage(w, r, http.StatusBadRequest, i18n.T(r.Context(), "auth.invitation.invalid"))
		case errors.Is(err, auth.ErrInstanceMFAEnrollmentNeeded):
			h.renderInvitationEnrollmentPage(w, r, http.StatusConflict, i18n.T(r.Context(), "auth.enrollment.mfa_required"))
		default:
			log.Printf("start Google invitation enrollment: %v", err)
			h.renderInvitationEnrollmentPage(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "auth.enrollment.unavailable"))
		}
		return
	}
	if start == nil || start.Challenge == nil || start.Challenge.Token == "" {
		log.Printf("start Google invitation enrollment returned incomplete authorization")
		h.renderInvitationEnrollmentPage(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "auth.enrollment.unavailable"))
		return
	}
	auth.SetPreAuthCookie(
		w, start.Challenge.Token, h.auth.Config().SecureCookies,
		start.Challenge.ExpiresAt.Sub(start.Challenge.CreatedAt),
	)
	http.Redirect(w, r, start.AuthorizationURL, http.StatusSeeOther)
}

func (h *Handler) handleInvitationEnrollmentSubmit(w http.ResponseWriter, r *http.Request) {
	h.handlePasswordTokenSubmit(
		w, r,
		auth.EnrollmentTokenPurposeEnrollment,
		invitationEnrollmentCompletePath,
		i18n.T(r.Context(), "auth.invitation.invalid"),
		h.renderInvitationEnrollmentPage,
	)
}

func (h *Handler) handleCredentialRedemption(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.renderCredentialRedemptionPage(w, r, http.StatusOK, "")
}

func (h *Handler) handleCredentialRedemptionSubmit(w http.ResponseWriter, r *http.Request) {
	h.handlePasswordTokenSubmit(
		w, r,
		auth.EnrollmentTokenPurposeCredentialReset,
		credentialRedemptionCompletePath,
		i18n.T(r.Context(), "auth.reset.token_invalid"),
		h.renderCredentialRedemptionPage,
	)
}

type passwordTokenPageRenderer func(http.ResponseWriter, *http.Request, int, string)

func (h *Handler) handlePasswordTokenSubmit(
	w http.ResponseWriter,
	r *http.Request,
	purpose auth.EnrollmentTokenPurpose,
	completePath string,
	failureMessage string,
	render passwordTokenPageRenderer,
) {
	if !h.auth.IsEnabled() {
		http.Error(w, i18n.T(r.Context(), "server.error.not_found"), http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, enrollmentRedemptionFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		render(w, r, http.StatusBadRequest, failureMessage)
		return
	}
	newPassword := r.PostFormValue("new_password")
	if newPassword != r.PostFormValue("confirm_password") {
		render(w, r, http.StatusBadRequest, i18n.T(r.Context(), "auth.password.mismatch"))
		return
	}

	_, err := h.auth.RedeemEnrollmentToken(r.Context(), auth.RedeemEnrollmentTokenOptions{
		Token:       r.PostFormValue("token"),
		NewPassword: newPassword,
		UserAgent:   r.UserAgent(),
		Purpose:     purpose,
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrEnrollmentTokenInvalid):
			render(w, r, http.StatusBadRequest, failureMessage)
		case errors.Is(err, auth.ErrPasswordInvalid),
			errors.Is(err, auth.ErrPasswordTooShort),
			errors.Is(err, auth.ErrPasswordTooLong),
			errors.Is(err, auth.ErrPasswordCommon):
			render(w, r, http.StatusBadRequest, authText(r.Context(), err.Error()))
		case errors.Is(err, auth.ErrInstanceMFAEnrollmentNeeded):
			render(w, r, http.StatusConflict, i18n.T(r.Context(), "auth.enrollment.mfa_required"))
		default:
			log.Printf("redeem %s token: %v", purpose, err)
			render(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "auth.enrollment.unavailable"))
		}
		return
	}

	auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
	auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, completePath, http.StatusSeeOther)
}

func (h *Handler) handleInvitationEnrollmentComplete(w http.ResponseWriter, r *http.Request) {
	h.renderPasswordTokenCompletion(w, r, views.InvitationEnrollmentCompletePage())
}

func (h *Handler) handleCredentialRedemptionComplete(w http.ResponseWriter, r *http.Request) {
	h.renderPasswordTokenCompletion(w, r, views.CredentialRedemptionCompletePage())
}

func (h *Handler) renderPasswordTokenCompletion(w http.ResponseWriter, r *http.Request, component templ.Component) {
	if !h.auth.IsEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	var page bytes.Buffer
	if err := component.Render(r.Context(), &page); err != nil {
		log.Printf("render password token completion: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_render_password_confirmation"), http.StatusInternalServerError)
		return
	}
	writeEnrollmentRedemptionPage(w, http.StatusOK, &page)
}

func (h *Handler) renderInvitationEnrollmentPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	var page bytes.Buffer
	if err := views.InvitationEnrollmentPage(views.InvitationEnrollmentData{
		Message: message, GoogleLoginAvailable: h.auth.HasGoogleLogin(),
	}).Render(r.Context(), &page); err != nil {
		log.Printf("render invitation enrollment page: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_render_invitation_form"), http.StatusInternalServerError)
		return
	}
	writeEnrollmentRedemptionPage(w, status, &page)
}

func (h *Handler) renderCredentialRedemptionPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	var page bytes.Buffer
	if err := views.CredentialRedemptionPage(views.CredentialRedemptionData{Message: message}).Render(r.Context(), &page); err != nil {
		log.Printf("render credential redemption page: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_render_password_reset_form"), http.StatusInternalServerError)
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
