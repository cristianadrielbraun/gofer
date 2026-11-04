package handler

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
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
			writePasskeyJSONErrorFor(w, r, http.StatusUnprocessableEntity, "Enter a passkey name between 1 and 64 characters.")
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			writePasskeyJSONErrorFor(w, r, http.StatusForbidden, "Verify this session before adding a passkey.")
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONErrorFor(w, r, http.StatusUnauthorized, "Your session expired. Sign in again.")
		default:
			log.Printf("start passkey registration: %v", err)
			writePasskeyJSONErrorFor(w, r, http.StatusInternalServerError, "Unable to start passkey registration right now.")
		}
		return
	}
	if options == nil || options.Challenge == nil || options.Challenge.Token == "" || len(options.CreationJSON) == 0 {
		log.Printf("start passkey registration returned incomplete options")
		writePasskeyJSONErrorFor(w, r, http.StatusInternalServerError, "Unable to start passkey registration right now.")
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
		writePasskeyJSONErrorFor(w, r, http.StatusBadRequest, "Unable to read the passkey response. Start again.")
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
			writePasskeyJSONErrorFor(w, r, http.StatusUnprocessableEntity, "The passkey response was not accepted. Start again.")
		case errors.Is(err, auth.ErrPasskeyDuplicate):
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONErrorFor(w, r, http.StatusConflict, "That passkey is already registered.")
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONErrorFor(w, r, http.StatusForbidden, "Session verification expired. Verify again, then start over.")
		case errors.Is(err, auth.ErrPasskeyRegistrationInvalid):
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONErrorFor(w, r, http.StatusGone, "That passkey request expired or was already used. Start again.")
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
			writePasskeyJSONErrorFor(w, r, http.StatusUnauthorized, "Your session expired. Sign in again.")
		default:
			log.Printf("finish passkey registration: %v", err)
			writePasskeyJSONErrorFor(w, r, http.StatusInternalServerError, "Unable to save this passkey right now.")
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
			h.renderSecurityManagementError(w, r, http.StatusConflict, "Add another sign-in method before removing this passkey.")
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

// writePasskeyJSONErrorFor is writePasskeyJSONError in the request's language.
func writePasskeyJSONErrorFor(w http.ResponseWriter, r *http.Request, status int, message string) {
	writePasskeyJSONError(w, status, serverText(r.Context(), message))
}

func writePasskeyJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func passkeySecurityViewData(ctx context.Context, passkeys []auth.PasskeyCredentialSummary) []views.PasskeySecurityData {
	result := make([]views.PasskeySecurityData, 0, len(passkeys))
	for _, passkey := range passkeys {
		details := i18n.T(ctx, "security.passkey")
		switch passkey.Attachment {
		case "platform":
			details = i18n.T(ctx, "server.passkey.this_device")
		case "cross-platform":
			details = i18n.T(ctx, "server.passkey.security_key")
		}
		if passkey.BackupState {
			details += ", " + i18n.T(ctx, "server.passkey.synced")
		} else if passkey.BackupEligible {
			details += ", " + i18n.T(ctx, "server.passkey.sync_capable")
		} else if len(passkey.Transports) > 0 {
			details += ", " + strings.Join(passkey.Transports, "/")
		}
		view := views.PasskeySecurityData{
			ID: passkey.ID, Name: passkey.Name, Details: details,
			CreatedAt: i18n.Dates(ctx).Date(passkey.CreatedAt.Local()),
			CanRemove: passkey.CanRemove, RemoveReason: authText(ctx, passkey.RemoveReason),
		}
		if passkey.LastUsedAt != nil {
			view.LastUsedAt = i18n.Dates(ctx).Date(passkey.LastUsedAt.Local())
		}
		result = append(result, view)
	}
	return result
}
