package handler

import (
	"bytes"
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

func writeSetupPage(w http.ResponseWriter, status int, page *bytes.Buffer) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.WriteHeader(status)
	_, _ = page.WriteTo(w)
}
