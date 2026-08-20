package handler

import (
	"bytes"
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
	recoveryLoginFormMaximumBytes = 4 << 10
	recoveryCodeFailureMessage    = "That recovery code is invalid or has already been used."
	recoveryRepairServiceMessage  = "Unable to continue account recovery right now. Please try again."
)

func (h *Handler) handleRecoveryCodeLogin(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.URL.RawQuery != "" {
		http.Redirect(w, r, "/login/mfa/recovery", http.StatusSeeOther)
		return
	}
	active, err := h.hasLoginChallenge(r, auth.ChallengePurposeMFA)
	if err != nil {
		log.Printf("read recovery-code login challenge: %v", err)
		http.Error(w, "failed to load account recovery", http.StatusInternalServerError)
		return
	}
	if !active {
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	h.renderRecoveryCodeLoginPage(w, r, http.StatusOK, "")
}

func (h *Handler) handleRecoveryCodeLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.URL.RawQuery != "" {
		http.Redirect(w, r, "/login/mfa/recovery", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, recoveryLoginFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderRecoveryCodeLoginPage(w, r, http.StatusBadRequest, recoveryCodeFailureMessage)
		return
	}
	challenge, err := h.auth.StartRecoveryCodeRepair(r.Context(), auth.RecoveryCodeLoginOptions{
		Token: auth.GetPreAuthToken(r), Code: r.PostFormValue("code"),
		Origin: h.auth.Config().BaseURL, Source: directLoginSource(r.RemoteAddr),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		var throttleError *auth.LoginThrottleError
		var validationError *auth.RecoveryCodeLoginValidationError
		switch {
		case errors.As(err, &throttleError):
			retrySeconds := int64((throttleError.RetryAfter + time.Second - 1) / time.Second)
			if retrySeconds < 1 {
				retrySeconds = 1
			}
			w.Header().Set("Retry-After", strconv.FormatInt(retrySeconds, 10))
			h.renderRecoveryCodeLoginPage(w, r, http.StatusTooManyRequests, recoveryCodeFailureMessage)
		case errors.As(err, &validationError) && !validationError.Terminal:
			h.renderRecoveryCodeLoginPage(w, r, http.StatusUnauthorized, recoveryCodeFailureMessage)
		case errors.Is(err, auth.ErrRecoveryCodeLoginChallengeInvalid), errors.As(err, &validationError):
			auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login?error=recovery", http.StatusSeeOther)
		default:
			log.Printf("start recovery-code factor repair: %v", err)
			h.renderRecoveryCodeLoginPage(w, r, http.StatusInternalServerError, recoveryRepairServiceMessage)
		}
		return
	}
	if challenge == nil || challenge.Token == "" {
		log.Printf("recovery-code factor repair returned no challenge")
		h.renderRecoveryCodeLoginPage(w, r, http.StatusInternalServerError, recoveryRepairServiceMessage)
		return
	}
	auth.SetPreAuthCookie(
		w, challenge.Token, h.auth.Config().SecureCookies,
		challenge.ExpiresAt.Sub(challenge.CreatedAt),
	)
	http.Redirect(w, r, "/login/recovery/mfa", http.StatusSeeOther)
}

func (h *Handler) handleRecoveryRepairMFA(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.URL.RawQuery != "" {
		http.Redirect(w, r, "/login/recovery/mfa", http.StatusSeeOther)
		return
	}
	state, err := h.auth.GetRecoveryRepairState(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.handleRecoveryRepairAccessError(w, r, err, "load recovery-repair authenticator")
		return
	}
	if state.TOTPConfirmed {
		http.Redirect(w, r, "/login/recovery/codes", http.StatusSeeOther)
		return
	}
	h.renderRecoveryRepairMFAPage(w, r, http.StatusOK, recoveryRepairMFAViewData(state, ""))
}

func (h *Handler) handleRecoveryRepairMFASubmit(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.URL.RawQuery != "" {
		http.Redirect(w, r, "/login/recovery/mfa", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, recoveryLoginFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderCurrentRecoveryRepairMFA(w, r, http.StatusBadRequest, "That authenticator code is invalid.")
		return
	}
	var state *auth.RecoveryRepairState
	var err error
	switch r.PostFormValue("action") {
	case "confirm":
		state, err = h.auth.ConfirmRecoveryRepairTOTP(
			r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL, r.PostFormValue("code"),
		)
	case "restart":
		state, err = h.auth.RestartRecoveryRepairTOTP(
			r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL,
		)
	default:
		h.renderCurrentRecoveryRepairMFA(w, r, http.StatusUnprocessableEntity, "Choose a valid authenticator-repair action.")
		return
	}
	if err != nil {
		var validationError *auth.RecoveryRepairTOTPValidationError
		switch {
		case errors.As(err, &validationError) && !validationError.Terminal:
			h.renderCurrentRecoveryRepairMFA(w, r, http.StatusUnprocessableEntity, "That authenticator code is invalid or has already been used.")
		case errors.Is(err, auth.ErrRecoveryRepairInvalid), errors.Is(err, auth.ErrRecoveryRepairStateChanged), errors.As(err, &validationError):
			auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login?error=recovery", http.StatusSeeOther)
		default:
			log.Printf("update recovery-repair authenticator: %v", err)
			h.renderCurrentRecoveryRepairMFA(w, r, http.StatusInternalServerError, recoveryRepairServiceMessage)
		}
		return
	}
	if state == nil {
		h.renderCurrentRecoveryRepairMFA(w, r, http.StatusInternalServerError, recoveryRepairServiceMessage)
		return
	}
	if state.TOTPConfirmed {
		http.Redirect(w, r, "/login/recovery/codes", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login/recovery/mfa", http.StatusSeeOther)
}

func (h *Handler) handleRecoveryRepairCodes(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.URL.RawQuery != "" {
		http.Redirect(w, r, "/login/recovery/codes", http.StatusSeeOther)
		return
	}
	state, err := h.auth.GetRecoveryRepairState(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.handleRecoveryRepairAccessError(w, r, err, "load recovery-repair code batch")
		return
	}
	if !state.TOTPConfirmed {
		http.Redirect(w, r, "/login/recovery/mfa", http.StatusSeeOther)
		return
	}
	h.renderRecoveryRepairCodesPage(w, r, http.StatusOK, recoveryRepairCodesViewData(state, nil, ""))
}

func (h *Handler) handleRecoveryRepairCodesSubmit(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.URL.RawQuery != "" {
		http.Redirect(w, r, "/login/recovery/codes", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, recoveryLoginFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderCurrentRecoveryRepairCodes(w, r, http.StatusBadRequest, "Unable to read that recovery-code request.")
		return
	}
	switch r.PostFormValue("action") {
	case "generate", "regenerate":
		batch, err := h.auth.GenerateRecoveryRepairCodes(
			r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL,
			r.PostFormValue("action") == "regenerate",
		)
		if err != nil {
			if errors.Is(err, auth.ErrRecoveryRepairBatchExists) {
				http.Redirect(w, r, "/login/recovery/codes", http.StatusSeeOther)
				return
			}
			handleErr := h.handleRecoveryRepairOperationError(w, r, err, "generate recovery-repair codes")
			if !handleErr {
				h.renderCurrentRecoveryRepairCodes(w, r, http.StatusInternalServerError, recoveryRepairServiceMessage)
			}
			return
		}
		h.renderRecoveryRepairCodesPage(w, r, http.StatusOK, views.RecoveryRepairCodesData{
			BatchID: batch.RecoveryBatchID, Codes: batch.Codes, Generated: true,
		})
	case "complete":
		session, err := h.auth.CompleteRecoveryRepair(r.Context(), auth.CompleteRecoveryRepairOptions{
			Token: auth.GetPreAuthToken(r), Origin: h.auth.Config().BaseURL,
			BatchID: r.PostFormValue("batch_id"), Saved: r.PostFormValue("saved") == "yes",
			Source: directLoginSource(r.RemoteAddr), UserAgent: r.UserAgent(),
		})
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrRecoveryRepairBatchRequired):
				h.renderCurrentRecoveryRepairCodes(w, r, http.StatusUnprocessableEntity, "Confirm that you saved the new recovery codes before continuing.")
			case errors.Is(err, auth.ErrRecoveryRepairStateChanged):
				http.Redirect(w, r, "/login/recovery/codes", http.StatusSeeOther)
			default:
				if !h.handleRecoveryRepairOperationError(w, r, err, "complete recovery repair") {
					h.renderCurrentRecoveryRepairCodes(w, r, http.StatusInternalServerError, recoveryRepairServiceMessage)
				}
			}
			return
		}
		if session == nil {
			h.renderCurrentRecoveryRepairCodes(w, r, http.StatusInternalServerError, recoveryRepairServiceMessage)
			return
		}
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		auth.SetSessionCookie(w, session.Token, h.auth.Config().SecureCookies)
		returnTo := auth.GetReturnTo(r)
		auth.ClearReturnToCookie(w, h.auth.Config().SecureCookies)
		if returnTo == "" {
			returnTo = "/"
			if user, loadErr := h.auth.GetUserByID(r.Context(), session.UserID); loadErr == nil && user != nil && user.IsManagement() {
				returnTo = "/admin"
			}
		}
		http.Redirect(w, r, returnTo, http.StatusSeeOther)
	default:
		h.renderCurrentRecoveryRepairCodes(w, r, http.StatusUnprocessableEntity, "Choose a valid recovery-code action.")
	}
}

func (h *Handler) hasLoginChallenge(r *http.Request, purpose auth.ChallengePurpose) (bool, error) {
	if purpose == auth.ChallengePurposeMFA {
		challenge, err := h.auth.GetActiveMFAChallenge(
			r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL,
		)
		return challenge != nil, err
	}
	challenge, err := h.auth.GetActivePreAuthChallenge(
		r.Context(), auth.GetPreAuthToken(r), purpose, h.auth.Config().BaseURL,
	)
	if err != nil {
		return false, err
	}
	return challenge != nil, nil
}

func (h *Handler) handleRecoveryRepairAccessError(w http.ResponseWriter, r *http.Request, err error, operation string) {
	if errors.Is(err, auth.ErrRecoveryRepairInvalid) || errors.Is(err, auth.ErrRecoveryRepairStateChanged) {
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		http.Redirect(w, r, "/login?error=recovery", http.StatusSeeOther)
		return
	}
	log.Printf("%s: %v", operation, err)
	auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
	http.Error(w, "failed to load account recovery", http.StatusInternalServerError)
}

func (h *Handler) handleRecoveryRepairOperationError(w http.ResponseWriter, r *http.Request, err error, operation string) bool {
	if errors.Is(err, auth.ErrRecoveryRepairInvalid) || errors.Is(err, auth.ErrRecoveryRepairStateChanged) {
		h.handleRecoveryRepairAccessError(w, r, err, operation)
		return true
	}
	if errors.Is(err, auth.ErrRecoveryRepairTOTPRequired) {
		http.Redirect(w, r, "/login/recovery/mfa", http.StatusSeeOther)
		return true
	}
	log.Printf("%s: %v", operation, err)
	return false
}

func (h *Handler) renderCurrentRecoveryRepairMFA(w http.ResponseWriter, r *http.Request, status int, message string) {
	state, err := h.auth.GetRecoveryRepairState(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.handleRecoveryRepairAccessError(w, r, err, "reload recovery-repair authenticator")
		return
	}
	h.renderRecoveryRepairMFAPage(w, r, status, recoveryRepairMFAViewData(state, message))
}

func (h *Handler) renderCurrentRecoveryRepairCodes(w http.ResponseWriter, r *http.Request, status int, message string) {
	state, err := h.auth.GetRecoveryRepairState(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.handleRecoveryRepairAccessError(w, r, err, "reload recovery-repair codes")
		return
	}
	h.renderRecoveryRepairCodesPage(w, r, status, recoveryRepairCodesViewData(state, nil, message))
}

func (h *Handler) renderRecoveryCodeLoginPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	var page bytes.Buffer
	if err := views.RecoveryCodeLoginPage(message).Render(r.Context(), &page); err != nil {
		log.Printf("render recovery-code login page: %v", err)
		http.Error(w, "failed to render account recovery", http.StatusInternalServerError)
		return
	}
	writeNoStoreHTML(w, status, &page)
}

func (h *Handler) renderRecoveryRepairMFAPage(w http.ResponseWriter, r *http.Request, status int, data views.RecoveryRepairMFAData) {
	var page bytes.Buffer
	if err := views.RecoveryRepairMFAPage(data).Render(r.Context(), &page); err != nil {
		log.Printf("render recovery-repair authenticator page: %v", err)
		http.Error(w, "failed to render authenticator repair", http.StatusInternalServerError)
		return
	}
	writeNoStoreHTML(w, status, &page)
}

func (h *Handler) renderRecoveryRepairCodesPage(w http.ResponseWriter, r *http.Request, status int, data views.RecoveryRepairCodesData) {
	var page bytes.Buffer
	if err := views.RecoveryRepairCodesPage(data).Render(r.Context(), &page); err != nil {
		log.Printf("render recovery-repair code page: %v", err)
		http.Error(w, "failed to render recovery codes", http.StatusInternalServerError)
		return
	}
	writeNoStoreHTML(w, status, &page)
}

func recoveryRepairMFAViewData(state *auth.RecoveryRepairState, message string) views.RecoveryRepairMFAData {
	data := views.RecoveryRepairMFAData{ErrorMessage: message}
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

func recoveryRepairCodesViewData(state *auth.RecoveryRepairState, codes []string, message string) views.RecoveryRepairCodesData {
	if state == nil {
		return views.RecoveryRepairCodesData{ErrorMessage: message}
	}
	return views.RecoveryRepairCodesData{
		BatchID: state.RecoveryBatchID, Codes: codes,
		Generated: state.RecoveryGenerated, ErrorMessage: message,
	}
}

func writeNoStoreHTML(w http.ResponseWriter, status int, page *bytes.Buffer) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.WriteHeader(status)
	_, _ = page.WriteTo(w)
}
