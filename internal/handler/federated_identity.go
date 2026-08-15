package handler

import (
	"errors"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const securityGoogleIdentityLinkPath = "/settings/security/identities/google/link"

func (h *Handler) handleSecurityGoogleIdentityLink(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() || !h.auth.HasGoogleLogin() {
		http.NotFound(w, r)
		return
	}
	if !h.parseSecurityManagementForm(w, r, "Unable to read the Google sign-in request. Please try again.") {
		return
	}
	clearLegacyOAuthStateCookie(w, h.auth.Config().SecureCookies)
	if previousToken := auth.GetPreAuthToken(r); previousToken != "" {
		err := h.auth.TerminateGoogleAuthorizationChallenge(r.Context(), previousToken)
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		if err != nil && !errors.Is(err, auth.ErrPreAuthChallengeInvalid) {
			log.Printf("replace Google identity-link challenge: %v", err)
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to connect Google sign-in right now. Please try again.")
			return
		}
	}

	start, err := h.auth.BeginGoogleIdentityLink(r.Context(), auth.GetSessionToken(r))
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			http.Redirect(w, r, "/settings/security?verify=1", http.StatusSeeOther)
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			log.Printf("start Google identity link: %v", err)
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to connect Google sign-in right now. Please try again.")
		}
		return
	}
	if start == nil || start.Challenge == nil || start.Challenge.Token == "" {
		log.Printf("start Google identity link returned incomplete authorization")
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to connect Google sign-in right now. Please try again.")
		return
	}
	auth.SetPreAuthCookie(
		w, start.Challenge.Token, h.auth.Config().SecureCookies,
		start.Challenge.ExpiresAt.Sub(start.Challenge.CreatedAt),
	)
	http.Redirect(w, r, start.AuthorizationURL, http.StatusTemporaryRedirect)
}

func federatedIdentityViewData(identities []auth.FederatedIdentitySummary) []views.FederatedIdentityData {
	result := make([]views.FederatedIdentityData, 0, len(identities))
	for _, identity := range identities {
		view := views.FederatedIdentityData{
			Provider: identity.Provider,
			Email:    identity.Email,
			LinkedAt: identity.LinkedAt.Local().Format("Jan 2, 2006"),
		}
		if identity.LastUsedAt != nil {
			view.LastUsedAt = identity.LastUsedAt.Local().Format("Jan 2, 2006")
		}
		result = append(result, view)
	}
	return result
}
