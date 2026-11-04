package handler

import (
	"encoding/base64"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const (
	securityStepUpPath           = "/settings/security/step-up"
	securityTOTPStartPath        = "/settings/security/totp/start"
	securityTOTPConfirmPath      = "/settings/security/totp/confirm"
	securityTOTPDisablePath      = "/settings/security/totp/disable"
	securityRecoveryStartPath    = "/settings/security/recovery/start"
	securityRecoveryCompletePath = "/settings/security/recovery/complete"
	securityRecoveryRevokePath   = "/settings/security/recovery/revoke"
	securityManagementCancelPath = "/settings/security/management/cancel"
	securityManagementFormBytes  = 8 << 10
)

func (h *Handler) handleSecurityStepUp(w http.ResponseWriter, r *http.Request) {
	jsonResponse := securityVerificationJSONRequested(r)
	if !h.parseSecurityManagementForm(w, r, i18n.T(r.Context(), "auth.security.verification_form")) {
		return
	}
	err := h.auth.VerifySecurityTOTPStepUp(
		r.Context(), auth.GetSessionToken(r), r.PostFormValue("code"),
		loginSource(r), r.UserAgent(),
	)
	if err != nil {
		var throttleError *auth.LoginThrottleError
		var validationError *auth.TOTPManagementValidationError
		switch {
		case errors.As(err, &throttleError):
			retrySeconds := int64((throttleError.RetryAfter + time.Second - 1) / time.Second)
			if retrySeconds < 1 {
				retrySeconds = 1
			}
			w.Header().Set("Retry-After", strconv.FormatInt(retrySeconds, 10))
			if jsonResponse {
				writeSecurityVerificationJSONError(w, http.StatusTooManyRequests, i18n.T(r.Context(), "auth.mfa.code_invalid_used"))
			} else {
				h.renderSecurityManagementError(w, r, http.StatusTooManyRequests, i18n.T(r.Context(), "auth.mfa.code_invalid_used"))
			}
		case errors.As(err, &validationError):
			if jsonResponse {
				writeSecurityVerificationJSONError(w, http.StatusUnprocessableEntity, i18n.T(r.Context(), "auth.mfa.code_invalid_used"))
			} else {
				h.renderSecurityManagementError(w, r, http.StatusUnprocessableEntity, i18n.T(r.Context(), "auth.mfa.code_invalid_used"))
			}
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			if jsonResponse {
				writeSecurityVerificationJSONError(w, http.StatusUnauthorized, i18n.T(r.Context(), "auth.session_expired"))
			} else {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			}
		default:
			log.Printf("verify security settings step-up: %v", err)
			if jsonResponse {
				writeSecurityVerificationJSONError(w, http.StatusInternalServerError, i18n.T(r.Context(), "auth.security.verify_unavailable"))
			} else {
				h.renderSecurityManagementError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "auth.security.verify_unavailable"))
			}
		}
		return
	}
	redirect := "/settings/security?verified=1"
	if returnTo := adminSecurityVerificationReturnTo(r.PostFormValue("return_to")); returnTo != "" {
		redirect = returnTo
	}
	if jsonResponse {
		writeSecurityVerificationJSON(w, http.StatusOK, map[string]string{"redirect": redirect})
		return
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (h *Handler) handleSecurityTOTPStart(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, i18n.T(r.Context(), "auth.security.totp_request")) {
		return
	}
	state, err := h.auth.StartTOTPManagement(r.Context(), auth.GetSessionToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.handleSecurityManagementOperationError(w, r, err, "start TOTP management")
		return
	}
	if state == nil || state.Challenge == nil || state.Challenge.Token == "" {
		log.Printf("start TOTP management returned no challenge")
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "auth.security.totp_update_unavailable"))
		return
	}
	auth.SetSecurityChallengeCookie(
		w, state.Challenge.Token, h.auth.Config().SecureCookies,
		state.Challenge.ExpiresAt.Sub(state.Challenge.CreatedAt),
	)
	redirect := "/settings/security?totp_enrollment=1"
	if state.IsReplacement {
		redirect = "/settings/security?totp_replacement=1"
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (h *Handler) handleSecurityTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, i18n.T(r.Context(), "auth.security.totp_form")) {
		return
	}
	state, _ := h.auth.GetTOTPManagement(
		r.Context(), auth.GetSecurityChallengeToken(r), auth.GetSessionToken(r), h.auth.Config().BaseURL,
	)
	session, err := h.auth.ConfirmTOTPManagement(
		r.Context(), auth.GetSecurityChallengeToken(r), auth.GetSessionToken(r),
		h.auth.Config().BaseURL, r.PostFormValue("code"), loginSource(r), r.UserAgent(),
	)
	if err != nil {
		var throttleError *auth.LoginThrottleError
		var validationError *auth.TOTPManagementValidationError
		switch {
		case errors.As(err, &throttleError):
			retrySeconds := int64((throttleError.RetryAfter + time.Second - 1) / time.Second)
			if retrySeconds < 1 {
				retrySeconds = 1
			}
			w.Header().Set("Retry-After", strconv.FormatInt(retrySeconds, 10))
			h.renderSecurityManagementError(w, r, http.StatusTooManyRequests, i18n.T(r.Context(), "auth.mfa.code_invalid_used"))
		case errors.As(err, &validationError) && !validationError.Terminal:
			h.renderSecurityManagementError(w, r, http.StatusUnprocessableEntity, i18n.T(r.Context(), "auth.mfa.code_invalid_used"))
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/settings/security?verify=1", http.StatusSeeOther)
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		case errors.Is(err, auth.ErrSecurityChallengeInvalid), errors.As(err, &validationError):
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/settings/security?challenge_expired=1", http.StatusSeeOther)
		default:
			log.Printf("confirm TOTP management: %v", err)
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "auth.security.totp_update_unavailable"))
		}
		return
	}
	if session == nil || session.Token == "" {
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "auth.security.totp_update_unavailable"))
		return
	}
	auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
	auth.SetSessionCookie(w, session.Token, h.auth.Config().SecureCookies)
	redirect := "/settings/security?totp_enrolled=1"
	if state != nil && state.IsReplacement {
		redirect = "/settings/security?totp_replaced=1"
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (h *Handler) handleSecurityTOTPDisable(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, i18n.T(r.Context(), "auth.security.totp_request")) {
		return
	}
	session, err := h.auth.DisableTOTP(r.Context(), auth.GetSessionToken(r), r.UserAgent())
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrLastAuthenticator):
			h.renderSecurityManagementError(w, r, http.StatusConflict, i18n.T(r.Context(), "auth.security.totp_add_another"))
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			http.Redirect(w, r, "/settings/security?verify=1", http.StatusSeeOther)
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			log.Printf("disable TOTP authenticator: %v", err)
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "auth.security.totp_disable_unavailable"))
		}
		return
	}
	if session == nil || session.Token == "" {
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "auth.security.totp_disable_unavailable"))
		return
	}
	auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
	auth.SetSessionCookie(w, session.Token, h.auth.Config().SecureCookies)
	http.Redirect(w, r, "/settings/security?totp_disabled=1", http.StatusSeeOther)
}

func (h *Handler) handleSecurityRecoveryStart(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, i18n.T(r.Context(), "auth.security.recovery_request")) {
		return
	}
	batch, err := h.auth.StartRecoveryCodeReplacement(r.Context(), auth.GetSessionToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.handleSecurityManagementOperationError(w, r, err, "start recovery-code replacement")
		return
	}
	if batch == nil || batch.Challenge == nil || batch.Challenge.Token == "" {
		log.Printf("start recovery-code replacement returned no challenge")
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "auth.security.recovery_generate_unavailable"))
		return
	}
	auth.SetSecurityChallengeCookie(
		w, batch.Challenge.Token, h.auth.Config().SecureCookies,
		batch.Challenge.ExpiresAt.Sub(batch.Challenge.CreatedAt),
	)
	h.renderPasswordSecurityTab(w, r, http.StatusOK, &views.PasswordSecurityData{
		RecoveryReplacementPending: true,
		RecoveryBatchID:            batch.BatchID,
		RecoveryCodes:              batch.Codes,
	})
}

func (h *Handler) handleSecurityRecoveryComplete(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, i18n.T(r.Context(), "auth.security.recovery_form")) {
		return
	}
	err := h.auth.CompleteRecoveryCodeReplacement(
		r.Context(), auth.GetSecurityChallengeToken(r), auth.GetSessionToken(r), h.auth.Config().BaseURL,
		r.PostFormValue("batch_id"), r.UserAgent(), r.PostFormValue("saved") == "yes",
	)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecoveryManagementRequired):
			h.renderSecurityManagementError(w, r, http.StatusUnprocessableEntity, i18n.T(r.Context(), "auth.security.recovery_confirm_replace"))
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/settings/security?verify=1", http.StatusSeeOther)
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		case errors.Is(err, auth.ErrSecurityChallengeInvalid):
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/settings/security?challenge_expired=1", http.StatusSeeOther)
		default:
			log.Printf("complete recovery-code replacement: %v", err)
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "auth.security.recovery_replace_unavailable"))
		}
		return
	}
	auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
	http.Redirect(w, r, "/settings/security?recovery_replaced=1", http.StatusSeeOther)
}

func (h *Handler) handleSecurityRecoveryRevoke(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, i18n.T(r.Context(), "auth.security.recovery_request")) {
		return
	}
	_, err := h.auth.RevokeRecoveryCodes(r.Context(), auth.GetSessionToken(r), r.UserAgent())
	if err != nil {
		h.handleSecurityManagementOperationError(w, r, err, "revoke recovery codes")
		return
	}
	auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
	http.Redirect(w, r, "/settings/security?recovery_revoked=1", http.StatusSeeOther)
}

func (h *Handler) handleSecurityManagementCancel(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, i18n.T(r.Context(), "auth.security.cancel_request")) {
		return
	}
	token := auth.GetSecurityChallengeToken(r)
	if token != "" {
		if err := h.auth.CancelSecurityManagement(
			r.Context(), token, auth.GetSessionToken(r), h.auth.Config().BaseURL,
		); err != nil && !errors.Is(err, auth.ErrSecurityChallengeInvalid) {
			log.Printf("cancel security management challenge: %v", err)
		}
	}
	auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
	http.Redirect(w, r, "/settings/security", http.StatusSeeOther)
}

func (h *Handler) parseSecurityManagementForm(w http.ResponseWriter, r *http.Request, message string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, securityManagementFormBytes)
	if err := r.ParseForm(); err != nil {
		h.renderSecurityManagementError(w, r, http.StatusBadRequest, message)
		return false
	}
	return true
}

func (h *Handler) handleSecurityManagementOperationError(w http.ResponseWriter, r *http.Request, err error, operation string) {
	switch {
	case errors.Is(err, auth.ErrRecentStepUpRequired):
		http.Redirect(w, r, "/settings/security?verify=1", http.StatusSeeOther)
	case errors.Is(err, auth.ErrSecuritySessionInvalid):
		auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	case errors.Is(err, auth.ErrTOTPManagementUnavailable):
		h.renderSecurityManagementError(w, r, http.StatusConflict, i18n.T(r.Context(), "auth.security.totp_required"))
	default:
		log.Printf("%s: %v", operation, err)
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "auth.security.update_unavailable"))
	}
}

func (h *Handler) renderSecurityManagementError(w http.ResponseWriter, r *http.Request, status int, message string) {
	h.renderPasswordSecurityTab(w, r, status, &views.PasswordSecurityData{
		Message: message, MessageIsError: true,
	})
}

func totpManagementViewData(state *auth.TOTPManagementState) *views.TOTPManagementData {
	if state == nil || state.Enrollment == nil {
		return nil
	}
	return &views.TOTPManagementData{
		QRCodeDataURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(state.Enrollment.QRPNG),
		ManualKey:     state.Enrollment.ManualKey, Algorithm: state.Enrollment.Algorithm,
		Digits: state.Enrollment.Digits, Period: state.Enrollment.Period, IsReplacement: state.IsReplacement,
	}
}
