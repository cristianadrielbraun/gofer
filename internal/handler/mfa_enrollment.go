package handler

import (
	"bytes"
	"encoding/base64"
	"errors"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleMFAEnrollment(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.URL.RawQuery != "" {
		http.Redirect(w, r, "/login/mfa/enroll", http.StatusSeeOther)
		return
	}
	state, err := h.auth.GetMFAEnrollmentState(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.handleMFAEnrollmentAccessError(w, r, err, "load required MFA enrollment")
		return
	}
	if state.TOTPConfirmed {
		http.Redirect(w, r, "/login/mfa/enroll/codes", http.StatusSeeOther)
		return
	}
	h.renderMFAEnrollmentPage(w, r, http.StatusOK, mfaEnrollmentViewData(state, ""))
}

func (h *Handler) handleMFAEnrollmentSubmit(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.URL.RawQuery != "" {
		http.Redirect(w, r, "/login/mfa/enroll", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, recoveryLoginFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderCurrentMFAEnrollment(w, r, http.StatusBadRequest, "That authenticator code is invalid.")
		return
	}
	var state *auth.MFAEnrollmentState
	var err error
	switch r.PostFormValue("action") {
	case "confirm":
		state, err = h.auth.ConfirmMFAEnrollmentTOTP(
			r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL, r.PostFormValue("code"),
		)
	case "restart":
		state, err = h.auth.RestartMFAEnrollment(
			r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL,
		)
	default:
		h.renderCurrentMFAEnrollment(w, r, http.StatusUnprocessableEntity, "Choose a valid authenticator-enrollment action.")
		return
	}
	if err != nil {
		var validationError *auth.MFAEnrollmentTOTPValidationError
		switch {
		case errors.As(err, &validationError) && !validationError.Terminal:
			h.renderCurrentMFAEnrollment(w, r, http.StatusUnprocessableEntity, "That authenticator code is invalid or has already been used.")
		case errors.Is(err, auth.ErrMFAEnrollmentInvalid), errors.Is(err, auth.ErrMFAEnrollmentStateChanged), errors.As(err, &validationError):
			h.handleMFAEnrollmentAccessError(w, r, err, "update required MFA enrollment")
		default:
			log.Printf("update required MFA enrollment: %v", err)
			h.renderCurrentMFAEnrollment(w, r, http.StatusInternalServerError, loginServiceMessage)
		}
		return
	}
	if state == nil {
		h.renderCurrentMFAEnrollment(w, r, http.StatusInternalServerError, loginServiceMessage)
		return
	}
	if state.TOTPConfirmed {
		http.Redirect(w, r, "/login/mfa/enroll/codes", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login/mfa/enroll", http.StatusSeeOther)
}

func (h *Handler) handleMFAEnrollmentCodes(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.URL.RawQuery != "" {
		http.Redirect(w, r, "/login/mfa/enroll/codes", http.StatusSeeOther)
		return
	}
	state, err := h.auth.GetMFAEnrollmentState(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.handleMFAEnrollmentAccessError(w, r, err, "load required MFA recovery codes")
		return
	}
	if !state.TOTPConfirmed {
		http.Redirect(w, r, "/login/mfa/enroll", http.StatusSeeOther)
		return
	}
	h.renderMFAEnrollmentCodesPage(w, r, http.StatusOK, mfaEnrollmentCodesViewData(state, nil, ""))
}

func (h *Handler) handleMFAEnrollmentCodesSubmit(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.URL.RawQuery != "" {
		http.Redirect(w, r, "/login/mfa/enroll/codes", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, recoveryLoginFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderCurrentMFAEnrollmentCodes(w, r, http.StatusBadRequest, "Unable to read that recovery-code request.")
		return
	}
	switch r.PostFormValue("action") {
	case "generate", "regenerate":
		batch, err := h.auth.GenerateMFAEnrollmentRecoveryCodes(
			r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL,
			r.PostFormValue("action") == "regenerate",
		)
		if err != nil {
			if errors.Is(err, auth.ErrMFAEnrollmentBatchExists) {
				http.Redirect(w, r, "/login/mfa/enroll/codes", http.StatusSeeOther)
				return
			}
			if !h.handleMFAEnrollmentOperationError(w, r, err, "generate required MFA recovery codes") {
				h.renderCurrentMFAEnrollmentCodes(w, r, http.StatusInternalServerError, loginServiceMessage)
			}
			return
		}
		h.renderMFAEnrollmentCodesPage(w, r, http.StatusOK, views.MFAEnrollmentCodesData{
			BatchID: batch.RecoveryBatchID, Codes: batch.Codes, Generated: true,
		})
	case "complete":
		session, err := h.auth.CompleteMFAEnrollment(r.Context(), auth.CompleteMFAEnrollmentOptions{
			Token: auth.GetPreAuthToken(r), Origin: h.auth.Config().BaseURL,
			BatchID: r.PostFormValue("batch_id"), Saved: r.PostFormValue("saved") == "yes",
			Source: directLoginSource(r.RemoteAddr), UserAgent: r.UserAgent(),
		})
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrMFAEnrollmentBatchRequired):
				h.renderCurrentMFAEnrollmentCodes(w, r, http.StatusUnprocessableEntity, "Confirm that you saved the recovery codes before continuing.")
			case errors.Is(err, auth.ErrMFAEnrollmentStateChanged):
				http.Redirect(w, r, "/login/mfa/enroll/codes", http.StatusSeeOther)
			default:
				if !h.handleMFAEnrollmentOperationError(w, r, err, "complete required MFA enrollment") {
					h.renderCurrentMFAEnrollmentCodes(w, r, http.StatusInternalServerError, loginServiceMessage)
				}
			}
			return
		}
		if session == nil {
			h.renderCurrentMFAEnrollmentCodes(w, r, http.StatusInternalServerError, loginServiceMessage)
			return
		}
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		auth.SetSessionCookie(w, session.Token, h.auth.Config().SecureCookies)
		userType := auth.UserTypeWebmail
		if user, loadErr := h.auth.GetUserByID(r.Context(), session.UserID); loadErr == nil && user != nil {
			userType = user.UserType
		}
		returnTo := h.loginReturnTo(r, userType)
		auth.ClearReturnToCookie(w, h.auth.Config().SecureCookies)
		http.Redirect(w, r, returnTo, http.StatusSeeOther)
	default:
		h.renderCurrentMFAEnrollmentCodes(w, r, http.StatusUnprocessableEntity, "Choose a valid recovery-code action.")
	}
}

func (h *Handler) handleMFAEnrollmentAccessError(w http.ResponseWriter, r *http.Request, err error, operation string) {
	if errors.Is(err, auth.ErrMFAEnrollmentInvalid) || errors.Is(err, auth.ErrMFAEnrollmentStateChanged) {
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		loginPath := "/login?error=mfa_enrollment"
		if isManagementReturnTarget(auth.GetReturnTo(r)) {
			loginPath = "/admin/login?error=mfa_enrollment"
		}
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
		return
	}
	log.Printf("%s: %v", operation, err)
	auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
	http.Error(w, "failed to load MFA enrollment", http.StatusInternalServerError)
}

func (h *Handler) handleMFAEnrollmentOperationError(w http.ResponseWriter, r *http.Request, err error, operation string) bool {
	if errors.Is(err, auth.ErrMFAEnrollmentInvalid) || errors.Is(err, auth.ErrMFAEnrollmentStateChanged) {
		h.handleMFAEnrollmentAccessError(w, r, err, operation)
		return true
	}
	if errors.Is(err, auth.ErrMFAEnrollmentTOTPRequired) {
		http.Redirect(w, r, "/login/mfa/enroll", http.StatusSeeOther)
		return true
	}
	log.Printf("%s: %v", operation, err)
	return false
}

func (h *Handler) renderCurrentMFAEnrollment(w http.ResponseWriter, r *http.Request, status int, message string) {
	state, err := h.auth.GetMFAEnrollmentState(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.handleMFAEnrollmentAccessError(w, r, err, "reload required MFA enrollment")
		return
	}
	h.renderMFAEnrollmentPage(w, r, status, mfaEnrollmentViewData(state, message))
}

func (h *Handler) renderCurrentMFAEnrollmentCodes(w http.ResponseWriter, r *http.Request, status int, message string) {
	state, err := h.auth.GetMFAEnrollmentState(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.handleMFAEnrollmentAccessError(w, r, err, "reload required MFA recovery codes")
		return
	}
	h.renderMFAEnrollmentCodesPage(w, r, status, mfaEnrollmentCodesViewData(state, nil, message))
}

func (h *Handler) renderMFAEnrollmentPage(w http.ResponseWriter, r *http.Request, status int, data views.MFAEnrollmentData) {
	var page bytes.Buffer
	if err := views.MFAEnrollmentPage(data).Render(r.Context(), &page); err != nil {
		log.Printf("render required MFA enrollment page: %v", err)
		http.Error(w, "failed to render MFA enrollment", http.StatusInternalServerError)
		return
	}
	writeNoStoreHTML(w, status, &page)
}

func (h *Handler) renderMFAEnrollmentCodesPage(w http.ResponseWriter, r *http.Request, status int, data views.MFAEnrollmentCodesData) {
	var page bytes.Buffer
	if err := views.MFAEnrollmentCodesPage(data).Render(r.Context(), &page); err != nil {
		log.Printf("render required MFA recovery-code page: %v", err)
		http.Error(w, "failed to render recovery codes", http.StatusInternalServerError)
		return
	}
	writeNoStoreHTML(w, status, &page)
}

func mfaEnrollmentViewData(state *auth.MFAEnrollmentState, message string) views.MFAEnrollmentData {
	data := views.MFAEnrollmentData{ErrorMessage: message}
	if state == nil || state.Enrollment == nil {
		return data
	}
	data.QRCodeDataURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(state.Enrollment.QRPNG)
	data.ManualKey = state.Enrollment.ManualKey
	data.Algorithm = state.Enrollment.Algorithm
	data.Digits = state.Enrollment.Digits
	data.Period = state.Enrollment.Period
	return data
}

func mfaEnrollmentCodesViewData(state *auth.MFAEnrollmentState, codes []string, message string) views.MFAEnrollmentCodesData {
	if state == nil {
		return views.MFAEnrollmentCodesData{ErrorMessage: message}
	}
	return views.MFAEnrollmentCodesData{
		BatchID: state.RecoveryBatchID, Codes: codes,
		Generated: state.RecoveryGenerated, ErrorMessage: message,
	}
}
