package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const (
	securityPasskeyStartPath  = "/settings/security/passkeys/start"
	securityPasskeyFinishPath = "/settings/security/passkeys/finish"
)

func (h *Handler) handleSecurityPasskeyStart(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, "Unable to read the passkey request. Please try again.") {
		return
	}
	options, err := h.auth.StartPasskeyRegistration(
		r.Context(), auth.GetSessionToken(r), h.auth.Config().BaseURL, r.PostFormValue("name"),
	)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrPasskeyNameInvalid):
			writePasskeyJSONError(w, http.StatusUnprocessableEntity, "Enter a passkey name between 1 and 64 characters.")
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			writePasskeyJSONError(w, http.StatusForbidden, "Verify this session before adding a passkey.")
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONError(w, http.StatusUnauthorized, "Your session expired. Sign in again.")
		default:
			log.Printf("start passkey registration: %v", err)
			writePasskeyJSONError(w, http.StatusInternalServerError, "Unable to start passkey registration right now.")
		}
		return
	}
	if options == nil || options.Challenge == nil || options.Challenge.Token == "" || len(options.CreationJSON) == 0 {
		log.Printf("start passkey registration returned incomplete options")
		writePasskeyJSONError(w, http.StatusInternalServerError, "Unable to start passkey registration right now.")
		return
	}
	auth.SetSecurityChallengeCookie(
		w, options.Challenge.Token, h.auth.Config().SecureCookies,
		options.Challenge.ExpiresAt.Sub(options.Challenge.CreatedAt),
	)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(options.CreationJSON)
}

func (h *Handler) handleSecurityPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	responseJSON, err := io.ReadAll(r.Body)
	if err != nil {
		writePasskeyJSONError(w, http.StatusBadRequest, "Unable to read the passkey response. Start again.")
		return
	}
	_, err = h.auth.FinishPasskeyRegistration(
		r.Context(), auth.GetSecurityChallengeToken(r), auth.GetSessionToken(r),
		h.auth.Config().BaseURL, responseJSON, r.UserAgent(),
	)
	if err != nil {
		var validationError *auth.PasskeyRegistrationValidationError
		switch {
		case errors.As(err, &validationError):
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONError(w, http.StatusUnprocessableEntity, "The passkey response was not accepted. Start again.")
		case errors.Is(err, auth.ErrPasskeyDuplicate):
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONError(w, http.StatusConflict, "That passkey is already registered.")
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONError(w, http.StatusForbidden, "Session verification expired. Verify again, then start over.")
		case errors.Is(err, auth.ErrPasskeyRegistrationInvalid):
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONError(w, http.StatusGone, "That passkey request expired or was already used. Start again.")
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONError(w, http.StatusUnauthorized, "Your session expired. Sign in again.")
		default:
			log.Printf("finish passkey registration: %v", err)
			writePasskeyJSONError(w, http.StatusInternalServerError, "Unable to save this passkey right now.")
		}
		return
	}
	auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"redirect": "/settings/security?passkey_added=1"})
}

func (h *Handler) handleSecurityPasskeyRemove(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, "Unable to read the passkey removal request. Please try again.") {
		return
	}
	session, err := h.auth.RemovePasskey(
		r.Context(), auth.GetSessionToken(r), r.PathValue("id"), r.UserAgent(),
	)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrPasskeyNotFound):
			http.NotFound(w, r)
		case errors.Is(err, auth.ErrLastAuthenticator):
			h.renderSecurityManagementError(w, r, http.StatusConflict, "Add another sign-in method or strong authenticator before removing this passkey.")
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			http.Redirect(w, r, "/settings/security?verify=1", http.StatusSeeOther)
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			log.Printf("remove passkey: %v", err)
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to remove this passkey right now.")
		}
		return
	}
	if session == nil || session.Token == "" {
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to remove this passkey right now.")
		return
	}
	auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
	auth.SetSessionCookie(w, session.Token, h.auth.Config().SecureCookies)
	http.Redirect(w, r, "/settings/security?passkey_removed=1", http.StatusSeeOther)
}

func writePasskeyJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func passkeySecurityViewData(passkeys []auth.PasskeyCredentialSummary) []views.PasskeySecurityData {
	result := make([]views.PasskeySecurityData, 0, len(passkeys))
	for _, passkey := range passkeys {
		details := "Passkey"
		switch passkey.Attachment {
		case "platform":
			details = "This device"
		case "cross-platform":
			details = "Security key or another device"
		}
		if passkey.BackupState {
			details += ", synced"
		} else if passkey.BackupEligible {
			details += ", sync capable"
		} else if len(passkey.Transports) > 0 {
			details += ", " + strings.Join(passkey.Transports, "/")
		}
		view := views.PasskeySecurityData{
			ID: passkey.ID, Name: passkey.Name, Details: details,
			CreatedAt: passkey.CreatedAt.Local().Format("Jan 2, 2006"),
			CanRemove: passkey.CanRemove, RemoveReason: passkey.RemoveReason,
		}
		if passkey.LastUsedAt != nil {
			view.LastUsedAt = passkey.LastUsedAt.Local().Format("Jan 2, 2006")
		}
		result = append(result, view)
	}
	return result
}
