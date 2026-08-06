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
	enrollmentRedemptionPath             = "/account/redeem"
	enrollmentRedemptionCompletePath     = "/account/redeem/complete"
	enrollmentRedemptionFormMaximumBytes = 12 << 10
	enrollmentRedemptionFailureMessage   = "That invitation or reset token is invalid or no longer active."
	enrollmentRedemptionServiceMessage   = "Unable to set the password right now. Please try again."
)

func (h *Handler) handleEnrollmentRedemption(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.renderEnrollmentRedemptionPage(w, r, http.StatusOK, "")
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

	_, err := h.auth.RedeemEnrollmentToken(r.Context(), auth.RedeemEnrollmentTokenOptions{
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
	http.Redirect(w, r, enrollmentRedemptionCompletePath, http.StatusSeeOther)
}

func (h *Handler) handleEnrollmentRedemptionComplete(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	var page bytes.Buffer
	if err := views.EnrollmentRedemptionCompletePage().Render(r.Context(), &page); err != nil {
		log.Printf("render enrollment redemption completion: %v", err)
		http.Error(w, "failed to render password confirmation", http.StatusInternalServerError)
		return
	}
	writeEnrollmentRedemptionPage(w, http.StatusOK, &page)
}

func (h *Handler) renderEnrollmentRedemptionPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	var page bytes.Buffer
	if err := views.EnrollmentRedemptionPage(views.EnrollmentRedemptionData{Message: message}).Render(r.Context(), &page); err != nil {
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
