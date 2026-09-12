package handler

import (
	"errors"
	"log"
	"net/http"
	"net/url"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const (
	securityGoogleIdentityLinkPath    = "/settings/security/identities/google/link"
	securityMicrosoftIdentityLinkPath = "/settings/security/identities/microsoft/link"
	securityOIDCIdentityLinkPath      = "/settings/security/identities/oidc/link"
)

func securityGoogleIdentityUnlinkPath(identityID string) string {
	return "/settings/security/identities/google/" + url.PathEscape(identityID) + "/unlink"
}

func securityMicrosoftIdentityUnlinkPath(identityID string) string {
	return "/settings/security/identities/microsoft/" + url.PathEscape(identityID) + "/unlink"
}

func securityOIDCIdentityUnlinkPath(identityID string) string {
	return "/settings/security/identities/oidc/" + url.PathEscape(identityID) + "/unlink"
}

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
	http.Redirect(w, r, start.AuthorizationURL, http.StatusSeeOther)
}

func (h *Handler) handleSecurityGoogleIdentityUnlink(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, "Unable to read the Google sign-in removal request. Please try again.") {
		return
	}
	session, err := h.auth.UnlinkGoogleIdentity(
		r.Context(), auth.GetSessionToken(r), r.PathValue("id"), r.UserAgent(),
	)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrFederatedIdentityUnknown):
			http.NotFound(w, r)
		case errors.Is(err, auth.ErrLastAuthenticator):
			h.renderSecurityManagementError(
				w, r, http.StatusConflict,
				"Add another usable sign-in method or required authenticator before disconnecting Google sign-in.",
			)
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			http.Redirect(w, r, "/settings/security?verify=1", http.StatusSeeOther)
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			log.Printf("unlink Google identity: %v", err)
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to disconnect Google sign-in right now.")
		}
		return
	}
	if session == nil || session.Token == "" {
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to disconnect Google sign-in right now.")
		return
	}
	auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
	auth.SetSessionCookie(w, session.Token, h.auth.Config().SecureCookies)
	http.Redirect(w, r, "/settings/security?google_unlinked=1", http.StatusSeeOther)
}

func (h *Handler) handleSecurityMicrosoftIdentityLink(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() || !h.auth.HasMicrosoftLogin() {
		http.NotFound(w, r)
		return
	}
	if !h.parseSecurityManagementForm(w, r, "Unable to read the Microsoft sign-in request. Please try again.") {
		return
	}
	clearLegacyOAuthStateCookie(w, h.auth.Config().SecureCookies)
	if previousToken := auth.GetPreAuthToken(r); previousToken != "" {
		err := h.auth.TerminateMicrosoftAuthorizationChallenge(r.Context(), previousToken)
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		if err != nil && !errors.Is(err, auth.ErrPreAuthChallengeInvalid) {
			log.Printf("replace Microsoft identity-link challenge: %v", err)
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to connect Microsoft sign-in right now. Please try again.")
			return
		}
	}

	start, err := h.auth.BeginMicrosoftIdentityLink(r.Context(), auth.GetSessionToken(r))
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			http.Redirect(w, r, "/settings/security?verify=1", http.StatusSeeOther)
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			log.Printf("start Microsoft identity link: %v", err)
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to connect Microsoft sign-in right now. Please try again.")
		}
		return
	}
	if start == nil || start.Challenge == nil || start.Challenge.Token == "" {
		log.Printf("start Microsoft identity link returned incomplete authorization")
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to connect Microsoft sign-in right now. Please try again.")
		return
	}
	auth.SetPreAuthCookie(
		w, start.Challenge.Token, h.auth.Config().SecureCookies,
		start.Challenge.ExpiresAt.Sub(start.Challenge.CreatedAt),
	)
	http.Redirect(w, r, start.AuthorizationURL, http.StatusSeeOther)
}

func (h *Handler) handleSecurityMicrosoftIdentityUnlink(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, "Unable to read the Microsoft sign-in removal request. Please try again.") {
		return
	}
	session, err := h.auth.UnlinkMicrosoftIdentity(
		r.Context(), auth.GetSessionToken(r), r.PathValue("id"), r.UserAgent(),
	)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrFederatedIdentityUnknown):
			http.NotFound(w, r)
		case errors.Is(err, auth.ErrLastAuthenticator):
			h.renderSecurityManagementError(
				w, r, http.StatusConflict,
				"Add another usable sign-in method or required authenticator before disconnecting Microsoft sign-in.",
			)
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			http.Redirect(w, r, "/settings/security?verify=1", http.StatusSeeOther)
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			log.Printf("unlink Microsoft identity: %v", err)
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to disconnect Microsoft sign-in right now.")
		}
		return
	}
	if session == nil || session.Token == "" {
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to disconnect Microsoft sign-in right now.")
		return
	}
	auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
	auth.SetSessionCookie(w, session.Token, h.auth.Config().SecureCookies)
	http.Redirect(w, r, "/settings/security?microsoft_unlinked=1", http.StatusSeeOther)
}

func (h *Handler) handleSecurityOIDCIdentityLink(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() || !h.auth.HasOIDCLogin() {
		http.NotFound(w, r)
		return
	}
	if !h.parseSecurityManagementForm(w, r, "Unable to read the external sign-in request. Please try again.") {
		return
	}
	clearLegacyOAuthStateCookie(w, h.auth.Config().SecureCookies)
	if previousToken := auth.GetPreAuthToken(r); previousToken != "" {
		err := h.auth.TerminateOIDCAuthorizationChallenge(r.Context(), previousToken)
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		if err != nil && !errors.Is(err, auth.ErrPreAuthChallengeInvalid) {
			log.Printf("replace OIDC identity-link challenge: %v", err)
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to connect external sign-in right now. Please try again.")
			return
		}
	}

	start, err := h.auth.BeginOIDCIdentityLink(r.Context(), auth.GetSessionToken(r))
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			http.Redirect(w, r, "/settings/security?verify=1", http.StatusSeeOther)
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			log.Printf("start OIDC identity link failed: reason=challenge_initialization_failed")
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to connect external sign-in right now. Please try again.")
		}
		return
	}
	if start == nil || start.Challenge == nil || start.Challenge.Token == "" {
		log.Printf("start OIDC identity link returned incomplete authorization")
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to connect external sign-in right now. Please try again.")
		return
	}
	auth.SetPreAuthCookie(
		w, start.Challenge.Token, h.auth.Config().SecureCookies,
		start.Challenge.ExpiresAt.Sub(start.Challenge.CreatedAt),
	)
	http.Redirect(w, r, start.AuthorizationURL, http.StatusSeeOther)
}

func (h *Handler) handleSecurityOIDCIdentityUnlink(w http.ResponseWriter, r *http.Request) {
	if !h.parseSecurityManagementForm(w, r, "Unable to read the external sign-in removal request. Please try again.") {
		return
	}
	session, err := h.auth.UnlinkOIDCIdentity(
		r.Context(), auth.GetSessionToken(r), r.PathValue("id"), r.UserAgent(),
	)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrFederatedIdentityUnknown):
			http.NotFound(w, r)
		case errors.Is(err, auth.ErrLastAuthenticator):
			h.renderSecurityManagementError(
				w, r, http.StatusConflict,
				"Add another usable sign-in method or required authenticator before disconnecting external sign-in.",
			)
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			http.Redirect(w, r, "/settings/security?verify=1", http.StatusSeeOther)
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			log.Printf("unlink OIDC identity: %v", err)
			h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to disconnect external sign-in right now.")
		}
		return
	}
	if session == nil || session.Token == "" {
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to disconnect external sign-in right now.")
		return
	}
	auth.ClearSecurityChallengeCookie(w, h.auth.Config().SecureCookies)
	auth.SetSessionCookie(w, session.Token, h.auth.Config().SecureCookies)
	http.Redirect(w, r, "/settings/security?oidc_unlinked=1", http.StatusSeeOther)
}

func federatedIdentityViewData(identities []auth.FederatedIdentitySummary) []views.FederatedIdentityData {
	result := make([]views.FederatedIdentityData, 0, len(identities))
	for _, identity := range identities {
		view := views.FederatedIdentityData{
			ID: identity.ID, Provider: identity.Provider,
			Email: identity.Email, LinkedAt: identity.LinkedAt.Local().Format("Jan 2, 2006"),
			CanUnlink: identity.CanUnlink, UnlinkReason: identity.UnlinkReason,
		}
		if identity.Provider == "google" {
			view.UnlinkPath = securityGoogleIdentityUnlinkPath(identity.ID)
		} else if identity.Provider == "microsoft" {
			view.UnlinkPath = securityMicrosoftIdentityUnlinkPath(identity.ID)
		} else if identity.Provider == "oidc" {
			view.UnlinkPath = securityOIDCIdentityUnlinkPath(identity.ID)
		}
		if identity.LastUsedAt != nil {
			view.LastUsedAt = identity.LastUsedAt.Local().Format("Jan 2, 2006")
		}
		result = append(result, view)
	}
	return result
}
