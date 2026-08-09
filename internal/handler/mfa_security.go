package handler

import (
	"encoding/base64"
	"errors"
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
	securityTOTPFailureMessage   = "That authenticator code is invalid or has already been used."
)

func (h *Handler) handleSecurityStepUp(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, "Unable to read the verification form. Please try again.") {
		return
	}
	err := h.auth.VerifySecurityTOTPStepUp(
		r.Context(), auth.GetSessionToken(r), r.PostFormValue("code"),
		directLoginSource(r.RemoteAddr), r.UserAgent(),
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
			h.renderSecurityManagementError(w, r, http.StatusTooManyRequests, securityTOTPFailureMessage)
		case errors.As(err, &validationError):
			h.renderSecurityManagementError(w, r, http.StatusUnprocessableEntity, securityTOTPFailureMessage)
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			log.Printf("verify security settings step-up: %v", err)
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to verify this session right now. Please try again.")
		}
		return
	}
	http.Redirect(w, r, "/settings/security?verified=1", http.StatusSeeOther)
}

func (h *Handler) handleSecurityTOTPStart(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, "Unable to read the authenticator request. Please try again.") {
		return
	}
	state, err := h.auth.StartTOTPReplacement(r.Context(), auth.GetSessionToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.handleSecurityManagementOperationError(w, r, err, "start TOTP replacement")
		return
	}
	if state == nil || state.Challenge == nil || state.Challenge.Token == "" {
		log.Printf("start TOTP replacement returned no challenge")
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to replace the authenticator right now. Please try again.")
		return
	}
	auth.SetSecurityChallengeCookie(
		w, state.Challenge.Token, h.auth.Config().SecureCookies,
		state.Challenge.ExpiresAt.Sub(state.Challenge.CreatedAt),
	)
	http.Redirect(w, r, "/settings/security?totp_replacement=1", http.StatusSeeOther)
}

func (h *Handler) handleSecurityTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, "Unable to read the authenticator form. Please try again.") {
		return
	}
	session, err := h.auth.ConfirmTOTPReplacement(
		r.Context(), auth.GetSecurityChallengeToken(r), auth.GetSessionToken(r),
		h.auth.Config().BaseURL, r.PostFormValue("code"), directLoginSource(r.RemoteAddr), r.UserAgent(),
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
			h.renderSecurityManagementError(w, r, http.StatusTooManyRequests, securityTOTPFailureMessage)
		case errors.As(err, &validationError) && !validationError.Terminal:
			h.renderSecurityManagementError(w, r, http.StatusUnprocessableEntity, securityTOTPFailureMessage)
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
			log.Printf("confirm TOTP replacement: %v", err)
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to replace the authenticator right now. Please try again.")
		}
		return
	}
	if session == nil || session.Token == "" {
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to replace the authenticator right now. Please try again.")
		return
	}
	auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
	auth.SetSessionCookie(w, session.Token, h.auth.Config().SecureCookies)
	http.Redirect(w, r, "/settings/security?totp_replaced=1", http.StatusSeeOther)
}

func (h *Handler) handleSecurityTOTPDisable(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, "Unable to read the authenticator request. Please try again.") {
		return
	}
	session, err := h.auth.DisableTOTP(r.Context(), auth.GetSessionToken(r), r.UserAgent())
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrLastAuthenticator):
			h.renderSecurityManagementError(w, r, http.StatusConflict, "Add another strong authenticator before disabling this one.")
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			http.Redirect(w, r, "/settings/security?verify=1", http.StatusSeeOther)
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			log.Printf("disable TOTP authenticator: %v", err)
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to disable the authenticator right now. Please try again.")
		}
		return
	}
	if session == nil || session.Token == "" {
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to disable the authenticator right now. Please try again.")
		return
	}
	auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
	auth.SetSessionCookie(w, session.Token, h.auth.Config().SecureCookies)
	http.Redirect(w, r, "/settings/security?totp_disabled=1", http.StatusSeeOther)
}

func (h *Handler) handleSecurityRecoveryStart(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, "Unable to read the recovery-code request. Please try again.") {
		return
	}
	batch, err := h.auth.StartRecoveryCodeReplacement(r.Context(), auth.GetSessionToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.handleSecurityManagementOperationError(w, r, err, "start recovery-code replacement")
		return
	}
	if batch == nil || batch.Challenge == nil || batch.Challenge.Token == "" {
		log.Printf("start recovery-code replacement returned no challenge")
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to generate recovery codes right now. Please try again.")
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
	if !h.parseSecurityManagementForm(w, r, "Unable to read the recovery-code form. Please try again.") {
		return
	}
	err := h.auth.CompleteRecoveryCodeReplacement(
		r.Context(), auth.GetSecurityChallengeToken(r), auth.GetSessionToken(r), h.auth.Config().BaseURL,
		r.PostFormValue("batch_id"), r.UserAgent(), r.PostFormValue("saved") == "yes",
	)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecoveryManagementRequired):
			h.renderSecurityManagementError(w, r, http.StatusUnprocessableEntity, "Confirm that you saved the new recovery codes before replacing the old batch.")
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
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to replace recovery codes right now. The previous codes remain active.")
		}
		return
	}
	auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
	http.Redirect(w, r, "/settings/security?recovery_replaced=1", http.StatusSeeOther)
}

func (h *Handler) handleSecurityRecoveryRevoke(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, "Unable to read the recovery-code request. Please try again.") {
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
	if !h.parseSecurityManagementForm(w, r, "Unable to read the cancellation request. Please try again.") {
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
		h.renderSecurityManagementError(w, r, http.StatusConflict, "An active authenticator is required for this action.")
	default:
		log.Printf("%s: %v", operation, err)
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to update security settings right now. Please try again.")
	}
}

func (h *Handler) renderSecurityManagementError(w http.ResponseWriter, r *http.Request, status int, message string) {
	h.renderPasswordSecurityTab(w, r, status, &views.PasswordSecurityData{
		Message: message, MessageIsError: true,
	})
}

func totpManagementViewData(state *auth.TOTPManagementState) *views.TOTPReplacementData {
	if state == nil || state.Enrollment == nil {
		return nil
	}
	return &views.TOTPReplacementData{
		QRCodeDataURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(state.Enrollment.QRPNG),
		ManualKey:     state.Enrollment.ManualKey, Algorithm: state.Enrollment.Algorithm,
		Digits: state.Enrollment.Digits, Period: state.Enrollment.Period,
	}
}
