package handler

import (
	"bytes"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const (
	setupPath                = "/setup"
	setupOwnerPath           = "/setup/owner"
	setupPasswordPath        = "/setup/password"
	setupMFAPath             = "/setup/mfa"
	setupRecoveryPath        = "/setup/recovery"
	setupFormMaximumBytes    = 8 << 10
	setupTokenFailureMessage = "That setup token is invalid or no longer active."
	setupTokenServiceMessage = "Unable to verify the setup token right now. Please try again."
)

func (h *Handler) handleSetup(w http.ResponseWriter, r *http.Request) {
	state, err := h.auth.SetupState(r.Context())
	if err != nil {
		log.Printf("read setup entry state: %v", err)
		h.renderSetupPage(w, r, http.StatusInternalServerError, setupTokenServiceMessage)
		return
	}
	if state.Initialized {
		h.writeSetupNotFound(w)
		return
	}
	if r.URL.RawQuery != "" {
		h.redirectSetupWithoutQuery(w, r)
		return
	}

	challenge, err := h.auth.GetActiveSetupAccess(
		r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL,
	)
	if err != nil {
		log.Printf("read setup access challenge: %v", err)
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		h.renderSetupPage(w, r, http.StatusInternalServerError, setupTokenServiceMessage)
		return
	}
	if challenge != nil {
		http.Redirect(w, r, setupOwnerPath, http.StatusSeeOther)
		return
	}
	h.renderSetupPage(w, r, http.StatusOK, "")
}

func (h *Handler) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	state, err := h.auth.SetupState(r.Context())
	if err != nil {
		log.Printf("read setup submission state: %v", err)
		h.renderSetupPage(w, r, http.StatusInternalServerError, setupTokenServiceMessage)
		return
	}
	if state.Initialized {
		h.writeSetupNotFound(w)
		return
	}
	if r.URL.RawQuery != "" {
		h.redirectSetupWithoutQuery(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, setupFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderSetupPage(w, r, http.StatusUnauthorized, setupTokenFailureMessage)
		return
	}
	challenge, err := h.auth.BeginSetup(r.Context(), auth.BeginSetupOptions{
		Token: r.PostFormValue("token"), Origin: h.auth.Config().BaseURL, UserAgent: r.UserAgent(),
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrSetupAlreadyInitialized):
			h.writeSetupNotFound(w)
		case errors.Is(err, auth.ErrSetupTokenInvalid):
			h.renderSetupPage(w, r, http.StatusUnauthorized, setupTokenFailureMessage)
		default:
			log.Printf("verify setup token: %v", err)
			h.renderSetupPage(w, r, http.StatusInternalServerError, setupTokenServiceMessage)
		}
		return
	}
	if challenge == nil {
		log.Printf("setup token verification returned no access challenge")
		h.renderSetupPage(w, r, http.StatusInternalServerError, setupTokenServiceMessage)
		return
	}

	auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
	auth.ClearReturnToCookie(w, h.auth.Config().SecureCookies)
	auth.SetPreAuthCookie(
		w, challenge.Token, h.auth.Config().SecureCookies,
		challenge.ExpiresAt.Sub(challenge.CreatedAt),
	)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, setupOwnerPath, http.StatusSeeOther)
}

func (h *Handler) handleSetupPassword(w http.ResponseWriter, r *http.Request) {
	state, err := h.auth.SetupState(r.Context())
	if err != nil {
		log.Printf("read setup password state: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	if state.Initialized {
		h.writeSetupNotFound(w)
		return
	}
	if r.URL.RawQuery != "" {
		h.redirectSetupPasswordWithoutQuery(w, r)
		return
	}
	ownerState, err := h.auth.GetSetupOwnerState(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrSetupAccessInvalid):
			auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, setupPath, http.StatusSeeOther)
		case errors.Is(err, auth.ErrSetupOwnerBlocked):
			http.Redirect(w, r, setupOwnerPath, http.StatusSeeOther)
		default:
			log.Printf("read setup password owner draft: %v", err)
			h.writeSetupServiceFailure(w)
		}
		return
	}
	if ownerState.Draft == nil || ownerState.DraftStale {
		http.Redirect(w, r, setupOwnerPath, http.StatusSeeOther)
		return
	}
	h.renderSetupPasswordPage(w, r, http.StatusOK, views.SetupPasswordData{PasswordReady: ownerState.PasswordReady})
}

func (h *Handler) handleSetupPasswordSubmit(w http.ResponseWriter, r *http.Request) {
	state, err := h.auth.SetupState(r.Context())
	if err != nil {
		log.Printf("read setup password submission state: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	if state.Initialized {
		h.writeSetupNotFound(w)
		return
	}
	if r.URL.RawQuery != "" {
		h.redirectSetupPasswordWithoutQuery(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, setupFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderSubmittedSetupPassword(w, r, http.StatusUnprocessableEntity, views.SetupPasswordData{
			Errors: map[string]string{"form": "The submitted password form is too large or invalid."},
		})
		return
	}
	_, err = h.auth.SaveSetupPasswordDraft(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL, auth.SetupPasswordDraftInput{
		Password: r.PostFormValue("password"), PasswordConfirmation: r.PostFormValue("password_confirmation"),
	})
	if err != nil {
		var validationErr *auth.SetupPasswordValidationError
		switch {
		case errors.Is(err, auth.ErrSetupAccessInvalid):
			auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, setupPath, http.StatusSeeOther)
		case errors.Is(err, auth.ErrSetupOwnerDraftRequired), errors.Is(err, auth.ErrSetupOwnerBlocked):
			http.Redirect(w, r, setupOwnerPath, http.StatusSeeOther)
		case errors.As(err, &validationErr):
			h.renderSubmittedSetupPassword(w, r, http.StatusUnprocessableEntity, views.SetupPasswordData{Errors: validationErr.Fields})
		default:
			log.Printf("save setup password draft: %v", err)
			h.writeSetupServiceFailure(w)
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, setupPasswordPath, http.StatusSeeOther)
}

func (h *Handler) handleSetupMFA(w http.ResponseWriter, r *http.Request) {
	state, err := h.auth.SetupState(r.Context())
	if err != nil {
		log.Printf("read setup MFA state: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	if state.Initialized {
		h.writeSetupNotFound(w)
		return
	}
	if r.URL.RawQuery != "" {
		h.redirectSetupMFAWithoutQuery(w, r)
		return
	}
	enrollment, err := h.auth.GetSetupTOTPEnrollment(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.handleSetupMFAAccessError(w, r, err, "read setup MFA enrollment")
		return
	}
	h.renderSetupMFAPage(w, r, http.StatusOK, setupMFAViewData(enrollment, nil))
}

func (h *Handler) handleSetupMFASubmit(w http.ResponseWriter, r *http.Request) {
	state, err := h.auth.SetupState(r.Context())
	if err != nil {
		log.Printf("read setup MFA submission state: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	if state.Initialized {
		h.writeSetupNotFound(w)
		return
	}
	if r.URL.RawQuery != "" {
		h.redirectSetupMFAWithoutQuery(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, setupFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderSubmittedSetupMFA(w, r, http.StatusUnprocessableEntity, map[string]string{
			"form": "The submitted authenticator form is too large or invalid.",
		})
		return
	}
	action := r.PostFormValue("action")
	switch action {
	case "start", "restart":
		_, err = h.auth.StartSetupTOTP(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL, action == "restart")
	case "confirm":
		_, err = h.auth.ConfirmSetupTOTP(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL, r.PostFormValue("code"))
	default:
		h.renderSubmittedSetupMFA(w, r, http.StatusUnprocessableEntity, map[string]string{
			"form": "Choose a valid authenticator setup action.",
		})
		return
	}
	if err != nil {
		var validationErr *auth.SetupTOTPValidationError
		if errors.As(err, &validationErr) {
			h.renderSubmittedSetupMFA(w, r, http.StatusUnprocessableEntity, validationErr.Fields)
			return
		}
		h.handleSetupMFAAccessError(w, r, err, "update setup MFA enrollment")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, setupMFAPath, http.StatusSeeOther)
}

func (h *Handler) handleSetupMFAAccessError(w http.ResponseWriter, r *http.Request, err error, operation string) {
	switch {
	case errors.Is(err, auth.ErrSetupAccessInvalid):
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		http.Redirect(w, r, setupPath, http.StatusSeeOther)
	case errors.Is(err, auth.ErrSetupOwnerDraftRequired), errors.Is(err, auth.ErrSetupOwnerBlocked):
		http.Redirect(w, r, setupOwnerPath, http.StatusSeeOther)
	case errors.Is(err, auth.ErrSetupPasswordDraftRequired), errors.Is(err, auth.ErrSetupTOTPDraftRequired):
		http.Redirect(w, r, setupPasswordPath, http.StatusSeeOther)
	default:
		log.Printf("%s: %v", operation, err)
		h.writeSetupServiceFailure(w)
	}
}

func (h *Handler) handleSetupRecovery(w http.ResponseWriter, r *http.Request) {
	state, err := h.auth.SetupState(r.Context())
	if err != nil {
		log.Printf("read setup recovery state: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	if state.Initialized {
		h.writeSetupNotFound(w)
		return
	}
	if r.URL.RawQuery != "" {
		h.redirectSetupRecoveryWithoutQuery(w, r)
		return
	}
	recoveryState, err := h.auth.GetSetupRecoveryState(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.handleSetupRecoveryAccessError(w, r, err, "read setup recovery-code state")
		return
	}
	h.renderSetupRecoveryPage(w, r, http.StatusOK, setupRecoveryViewData(recoveryState, nil, nil))
}

func (h *Handler) handleSetupRecoverySubmit(w http.ResponseWriter, r *http.Request) {
	state, err := h.auth.SetupState(r.Context())
	if err != nil {
		log.Printf("read setup recovery submission state: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	if state.Initialized {
		h.writeSetupNotFound(w)
		return
	}
	if r.URL.RawQuery != "" {
		h.redirectSetupRecoveryWithoutQuery(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, setupFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderSubmittedSetupRecovery(w, r, http.StatusUnprocessableEntity, map[string]string{
			"form": "The submitted recovery-code form is too large or invalid.",
		})
		return
	}
	action := r.PostFormValue("action")
	switch action {
	case "generate", "regenerate":
		batch, err := h.auth.GenerateSetupRecoveryCodes(
			r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL, action == "regenerate",
		)
		if errors.Is(err, auth.ErrSetupRecoveryBatchAlreadyGenerated) || errors.Is(err, auth.ErrSetupRecoveryStateChanged) {
			h.redirectSetupRecoveryWithoutQuery(w, r)
			return
		}
		if err != nil {
			h.handleSetupRecoveryAccessError(w, r, err, "generate setup recovery-code batch")
			return
		}
		h.renderSetupRecoveryPage(w, r, http.StatusOK, setupRecoveryViewData(&batch.SetupRecoveryState, batch.Codes, nil))
		return
	case "acknowledge":
		_, err = h.auth.AcknowledgeSetupRecoveryCodes(
			r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL,
			r.PostFormValue("batch_id"), r.PostFormValue("saved") == "yes",
		)
	default:
		h.renderSubmittedSetupRecovery(w, r, http.StatusUnprocessableEntity, map[string]string{
			"form": "Choose a valid recovery-code setup action.",
		})
		return
	}
	if err != nil {
		var validationErr *auth.SetupRecoveryValidationError
		if errors.As(err, &validationErr) {
			h.renderSubmittedSetupRecovery(w, r, http.StatusUnprocessableEntity, validationErr.Fields)
			return
		}
		h.handleSetupRecoveryAccessError(w, r, err, "acknowledge setup recovery-code batch")
		return
	}
	h.redirectSetupRecoveryWithoutQuery(w, r)
}

func (h *Handler) handleSetupRecoveryAccessError(w http.ResponseWriter, r *http.Request, err error, operation string) {
	switch {
	case errors.Is(err, auth.ErrSetupAccessInvalid):
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		http.Redirect(w, r, setupPath, http.StatusSeeOther)
	case errors.Is(err, auth.ErrSetupOwnerDraftRequired), errors.Is(err, auth.ErrSetupOwnerBlocked):
		http.Redirect(w, r, setupOwnerPath, http.StatusSeeOther)
	case errors.Is(err, auth.ErrSetupPasswordDraftRequired):
		http.Redirect(w, r, setupPasswordPath, http.StatusSeeOther)
	case errors.Is(err, auth.ErrSetupTOTPDraftRequired), errors.Is(err, auth.ErrSetupTOTPConfirmationRequired):
		http.Redirect(w, r, setupMFAPath, http.StatusSeeOther)
	case errors.Is(err, auth.ErrSetupRecoveryBatchRequired):
		http.Redirect(w, r, setupRecoveryPath, http.StatusSeeOther)
	default:
		log.Printf("%s: %v", operation, err)
		h.writeSetupServiceFailure(w)
	}
}

func (h *Handler) handleSetupOwner(w http.ResponseWriter, r *http.Request) {
	state, err := h.auth.SetupState(r.Context())
	if err != nil {
		log.Printf("read protected setup state: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	if state.Initialized {
		h.writeSetupNotFound(w)
		return
	}
	if r.URL.RawQuery != "" {
		h.redirectSetupOwnerWithoutQuery(w, r)
		return
	}
	ownerState, err := h.auth.GetSetupOwnerState(
		r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL,
	)
	if err != nil {
		if errors.Is(err, auth.ErrSetupAccessInvalid) {
			auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, setupPath, http.StatusSeeOther)
			return
		}
		if errors.Is(err, auth.ErrSetupOwnerBlocked) {
			h.renderSetupOwnerPage(w, r, http.StatusConflict, views.SetupOwnerData{
				BlockedMessage: "Gofer found an ambiguous or unbounded existing-user topology.",
			})
			return
		}
		log.Printf("read protected setup owner state: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	h.renderSetupOwnerPage(w, r, http.StatusOK, setupOwnerViewData(ownerState, views.SetupOwnerFormData{}))
}

func (h *Handler) handleSetupOwnerSubmit(w http.ResponseWriter, r *http.Request) {
	state, err := h.auth.SetupState(r.Context())
	if err != nil {
		log.Printf("read setup owner submission state: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	if state.Initialized {
		h.writeSetupNotFound(w)
		return
	}
	if r.URL.RawQuery != "" {
		h.redirectSetupOwnerWithoutQuery(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, setupFormMaximumBytes)
	if err := r.ParseForm(); err != nil {
		h.renderSubmittedSetupOwner(w, r, http.StatusUnprocessableEntity, views.SetupOwnerFormData{
			Errors: map[string]string{"form": "The submitted owner profile is too large or invalid."},
		})
		return
	}
	mode, targetUserID := parseSetupOwnerTarget(r.PostFormValue("owner_target"))
	form := views.SetupOwnerFormData{
		Target: r.PostFormValue("owner_target"), Name: r.PostFormValue("name"),
		Username: r.PostFormValue("username"), Email: r.PostFormValue("email"),
	}
	_, err = h.auth.SaveSetupOwnerDraft(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL, auth.SetupOwnerDraftInput{
		Mode: mode, TargetUserID: targetUserID, Name: form.Name, Username: form.Username, Email: form.Email,
	})
	if err != nil {
		var validationErr *auth.SetupOwnerValidationError
		switch {
		case errors.Is(err, auth.ErrSetupAccessInvalid):
			auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, setupPath, http.StatusSeeOther)
		case errors.Is(err, auth.ErrSetupOwnerBlocked):
			h.renderSetupOwnerPage(w, r, http.StatusConflict, views.SetupOwnerData{
				BlockedMessage: "Gofer found an ambiguous or unbounded existing-user topology.",
			})
		case errors.As(err, &validationErr):
			form.Errors = validationErr.Fields
			h.renderSubmittedSetupOwner(w, r, http.StatusUnprocessableEntity, form)
		default:
			log.Printf("save setup owner draft: %v", err)
			h.writeSetupServiceFailure(w)
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, setupPasswordPath, http.StatusSeeOther)
}

func (h *Handler) renderSubmittedSetupOwner(w http.ResponseWriter, r *http.Request, status int, form views.SetupOwnerFormData) {
	ownerState, err := h.auth.GetSetupOwnerState(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		if errors.Is(err, auth.ErrSetupAccessInvalid) {
			auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, setupPath, http.StatusSeeOther)
			return
		}
		if errors.Is(err, auth.ErrSetupOwnerBlocked) {
			h.renderSetupOwnerPage(w, r, http.StatusConflict, views.SetupOwnerData{
				BlockedMessage: "Gofer found an ambiguous or unbounded existing-user topology.",
			})
			return
		}
		log.Printf("reload setup owner form: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	h.renderSetupOwnerPage(w, r, status, setupOwnerViewData(ownerState, form))
}

func (h *Handler) renderSetupOwnerPage(w http.ResponseWriter, r *http.Request, status int, data views.SetupOwnerData) {
	var page bytes.Buffer
	if err := views.SetupOwnerPage(data).Render(r.Context(), &page); err != nil {
		log.Printf("render protected setup owner page: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	writeSetupPage(w, status, &page)
}

func (h *Handler) renderSetupPasswordPage(w http.ResponseWriter, r *http.Request, status int, data views.SetupPasswordData) {
	var page bytes.Buffer
	if err := views.SetupPasswordPage(data).Render(r.Context(), &page); err != nil {
		log.Printf("render protected setup password page: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	writeSetupPage(w, status, &page)
}

func (h *Handler) renderSetupMFAPage(w http.ResponseWriter, r *http.Request, status int, data views.SetupMFAData) {
	var page bytes.Buffer
	if err := views.SetupMFAPage(data).Render(r.Context(), &page); err != nil {
		log.Printf("render protected setup MFA page: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	writeSetupPage(w, status, &page)
}

func (h *Handler) renderSetupRecoveryPage(w http.ResponseWriter, r *http.Request, status int, data views.SetupRecoveryData) {
	var page bytes.Buffer
	if err := views.SetupRecoveryPage(data).Render(r.Context(), &page); err != nil {
		log.Printf("render protected setup recovery page: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	writeSetupPage(w, status, &page)
}

func (h *Handler) renderSubmittedSetupRecovery(w http.ResponseWriter, r *http.Request, status int, errors map[string]string) {
	state, err := h.auth.GetSetupRecoveryState(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.handleSetupRecoveryAccessError(w, r, err, "reload setup recovery-code state")
		return
	}
	h.renderSetupRecoveryPage(w, r, status, setupRecoveryViewData(state, nil, errors))
}

func setupRecoveryViewData(state *auth.SetupRecoveryState, codes []string, errors map[string]string) views.SetupRecoveryData {
	return views.SetupRecoveryData{
		BatchID: state.BatchID, Codes: codes, Generated: state.Generated,
		Acknowledged: state.Acknowledged, Errors: errors,
	}
}

func (h *Handler) renderSubmittedSetupMFA(w http.ResponseWriter, r *http.Request, status int, errors map[string]string) {
	enrollment, err := h.auth.GetSetupTOTPEnrollment(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		h.handleSetupMFAAccessError(w, r, err, "reload setup MFA enrollment")
		return
	}
	h.renderSetupMFAPage(w, r, status, setupMFAViewData(enrollment, errors))
}

func setupMFAViewData(enrollment *auth.SetupTOTPEnrollment, errors map[string]string) views.SetupMFAData {
	data := views.SetupMFAData{
		Algorithm: enrollment.Algorithm, Digits: enrollment.Digits, Period: enrollment.Period,
		TOTPReady: enrollment.Confirmed, Errors: errors,
	}
	if !enrollment.Confirmed {
		data.QRCodeDataURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(enrollment.QRPNG)
		data.ManualKey = enrollment.ManualKey
	}
	return data
}

func (h *Handler) renderSubmittedSetupPassword(w http.ResponseWriter, r *http.Request, status int, data views.SetupPasswordData) {
	ownerState, err := h.auth.GetSetupOwnerState(r.Context(), auth.GetPreAuthToken(r), h.auth.Config().BaseURL)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrSetupAccessInvalid):
			auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, setupPath, http.StatusSeeOther)
		case errors.Is(err, auth.ErrSetupOwnerBlocked):
			http.Redirect(w, r, setupOwnerPath, http.StatusSeeOther)
		default:
			log.Printf("read setup password owner draft after submission: %v", err)
			h.writeSetupServiceFailure(w)
		}
		return
	}
	if ownerState.Draft == nil || ownerState.DraftStale {
		http.Redirect(w, r, setupOwnerPath, http.StatusSeeOther)
		return
	}
	data.PasswordReady = ownerState.PasswordReady
	h.renderSetupPasswordPage(w, r, status, data)
}

func setupOwnerViewData(state *auth.SetupOwnerState, submitted views.SetupOwnerFormData) views.SetupOwnerData {
	data := views.SetupOwnerData{
		Kind: string(state.Topology.Kind), DraftStale: state.DraftStale,
		Candidates: make([]views.SetupOwnerCandidateData, 0, len(state.Topology.Candidates)),
	}
	for _, candidate := range state.Topology.Candidates {
		data.Candidates = append(data.Candidates, views.SetupOwnerCandidateData{
			ID: candidate.ID, Name: candidate.Name, Email: candidate.Email, Username: candidate.Username,
			Status: string(candidate.Status), IsAdmin: candidate.IsAdmin,
			MailboxCount: candidate.MailboxCount, LegacySessions: candidate.LegacySessions,
		})
	}
	if state.Topology.Kind == auth.SetupOwnerTopologyFresh {
		data.Form.Target = "create"
	} else if state.Topology.Kind == auth.SetupOwnerTopologyLegacyDefault {
		data.Form.Target = "existing:default"
	}
	if state.Draft != nil {
		data.DraftSaved = !state.DraftStale
		data.Form = views.SetupOwnerFormData{
			Target: setupOwnerTargetValue(state.Draft.Mode, state.Draft.TargetUserID),
			Name:   state.Draft.Name, Username: state.Draft.Username, Email: state.Draft.Email,
		}
	}
	if submitted.Target != "" || submitted.Name != "" || submitted.Username != "" || submitted.Email != "" || len(submitted.Errors) > 0 {
		data.Form = submitted
	}
	return data
}

func parseSetupOwnerTarget(value string) (auth.SetupOwnerMode, string) {
	if value == "create" {
		return auth.SetupOwnerModeCreate, ""
	}
	if targetUserID, found := strings.CutPrefix(value, "existing:"); found {
		return auth.SetupOwnerModeExisting, targetUserID
	}
	return "", ""
}

func setupOwnerTargetValue(mode auth.SetupOwnerMode, userID string) string {
	if mode == auth.SetupOwnerModeCreate {
		return "create"
	}
	if mode == auth.SetupOwnerModeExisting {
		return "existing:" + userID
	}
	return ""
}

func (h *Handler) renderSetupPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	var page bytes.Buffer
	if err := views.SetupTokenPage(views.SetupTokenData{Message: message}).Render(r.Context(), &page); err != nil {
		log.Printf("render setup token page: %v", err)
		h.writeSetupServiceFailure(w)
		return
	}
	writeSetupPage(w, status, &page)
}

func (h *Handler) writeSetupNotFound(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Error(w, "not found", http.StatusNotFound)
}

func (h *Handler) writeSetupServiceFailure(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Error(w, setupTokenServiceMessage, http.StatusInternalServerError)
}

func (h *Handler) redirectSetupWithoutQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, setupPath, http.StatusSeeOther)
}

func (h *Handler) redirectSetupOwnerWithoutQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, setupOwnerPath, http.StatusSeeOther)
}

func (h *Handler) redirectSetupPasswordWithoutQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, setupPasswordPath, http.StatusSeeOther)
}

func (h *Handler) redirectSetupMFAWithoutQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, setupMFAPath, http.StatusSeeOther)
}

func (h *Handler) redirectSetupRecoveryWithoutQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, setupRecoveryPath, http.StatusSeeOther)
}

func writeSetupPage(w http.ResponseWriter, status int, page *bytes.Buffer) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.WriteHeader(status)
	_, _ = page.WriteTo(w)
}
