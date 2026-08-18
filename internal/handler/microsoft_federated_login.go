package handler

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

func (h *Handler) handleMicrosoftRedirect(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() || !h.auth.HasMicrosoftLogin() {
		http.Error(w, "auth not enabled", http.StatusNotFound)
		return
	}
	clearLegacyOAuthStateCookie(w, h.auth.Config().SecureCookies)
	if previousToken := auth.GetPreAuthToken(r); previousToken != "" {
		err := h.auth.TerminateMicrosoftAuthorizationChallenge(r.Context(), previousToken)
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		if err != nil && !errors.Is(err, auth.ErrPreAuthChallengeInvalid) {
			http.Error(w, "failed to initialize authentication", http.StatusInternalServerError)
			return
		}
	}

	start, err := h.auth.BeginMicrosoftLogin(r.Context())
	if err != nil {
		log.Printf("Microsoft application login could not start: reason=challenge_initialization_failed")
		http.Error(w, "failed to initialize authentication", http.StatusInternalServerError)
		return
	}
	challenge := start.Challenge
	auth.SetPreAuthCookie(w, challenge.Token, h.auth.Config().SecureCookies, challenge.ExpiresAt.Sub(challenge.CreatedAt))
	http.Redirect(w, r, start.AuthorizationURL, http.StatusTemporaryRedirect)
}

func (h *Handler) handleMicrosoftCallback(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() || !h.auth.HasMicrosoftLogin() {
		http.Error(w, "auth not enabled", http.StatusNotFound)
		return
	}
	clearLegacyOAuthStateCookie(w, h.auth.Config().SecureCookies)

	preAuthToken := auth.GetPreAuthToken(r)
	purpose := auth.ChallengePurposeFederatedLogin
	if preAuthToken != "" {
		callbackPurpose, err := h.auth.GetMicrosoftCallbackPurpose(r.Context(), preAuthToken)
		if err == nil {
			purpose = callbackPurpose
		} else if !errors.Is(err, auth.ErrPreAuthChallengeInvalid) {
			log.Printf("Microsoft authorization callback purpose lookup failed")
			h.rejectMicrosoftCallback(w, r, purpose, preAuthToken, "challenge_lookup_failed", false)
			return
		}
	}
	if !h.auth.IsCanonicalMicrosoftCallbackRequest(r) {
		h.rejectMicrosoftCallback(w, r, purpose, preAuthToken, "callback_origin_invalid", true)
		return
	}
	if preAuthToken == "" {
		h.rejectMicrosoftCallback(w, r, purpose, "", "challenge_missing", false)
		return
	}
	if !auth.PreAuthTokensMatch(r.URL.Query().Get("state"), preAuthToken) {
		h.rejectMicrosoftCallback(w, r, purpose, preAuthToken, "state_mismatch", true)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		reason := "authorization_code_missing"
		if strings.TrimSpace(r.URL.Query().Get("error")) != "" {
			reason = "provider_denied"
		}
		h.rejectMicrosoftCallback(w, r, purpose, preAuthToken, reason, true)
		return
	}
	if purpose == auth.ChallengePurposeFederatedLink {
		_, err := h.auth.CompleteMicrosoftIdentityLink(
			r.Context(), preAuthToken, auth.GetSessionToken(r), code, r.UserAgent(),
		)
		auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
		if err != nil {
			reason := auth.FederatedLoginReason(err)
			if errors.Is(err, auth.ErrFederatedIdentityConflict) {
				reason = auth.FederatedLoginFailureIdentityConflict
			}
			log.Printf("Microsoft identity link rejected: reason=%s", reason)
			http.Redirect(w, r, "/settings/security?microsoft_link_failed=1", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/settings/security?microsoft_linked=1", http.StatusSeeOther)
		return
	}
	if purpose != auth.ChallengePurposeFederatedLogin {
		h.rejectMicrosoftCallback(w, r, purpose, preAuthToken, "challenge_purpose_invalid", true)
		return
	}

	user, result, err := h.auth.HandleMicrosoftCallback(r.Context(), preAuthToken, code, r.UserAgent())
	auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
	if err != nil {
		log.Printf("Microsoft application login rejected: reason=%s", auth.FederatedLoginReason(err))
		http.Redirect(w, r, "/login?error=auth_failed", http.StatusSeeOther)
		return
	}
	_ = user
	if result == nil || (result.Session == nil) == (result.PreAuthChallenge == nil) {
		http.Redirect(w, r, "/login?error=auth_failed", http.StatusSeeOther)
		return
	}
	if result.PreAuthChallenge != nil {
		challenge := result.PreAuthChallenge
		auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
		auth.ClearPasskeyLoginChallengeCookie(w, h.auth.Config().SecureCookies)
		auth.SetPreAuthCookie(
			w, challenge.Token, h.auth.Config().SecureCookies,
			challenge.ExpiresAt.Sub(challenge.CreatedAt),
		)
		http.Redirect(w, r, "/login/mfa", http.StatusSeeOther)
		return
	}
	auth.ClearPasskeyLoginChallengeCookie(w, h.auth.Config().SecureCookies)
	auth.SetSessionCookie(w, result.Session.Token, h.auth.Config().SecureCookies)
	returnTo := auth.GetReturnTo(r)
	auth.ClearReturnToCookie(w, h.auth.Config().SecureCookies)
	if returnTo == "" {
		returnTo = "/"
	}
	http.Redirect(w, r, returnTo, http.StatusSeeOther)
}

func (h *Handler) rejectMicrosoftCallback(
	w http.ResponseWriter,
	r *http.Request,
	purpose auth.ChallengePurpose,
	token, reason string,
	terminate bool,
) {
	if terminate && token != "" {
		if err := h.auth.TerminateMicrosoftAuthorizationChallenge(r.Context(), token); err != nil && !errors.Is(err, auth.ErrPreAuthChallengeInvalid) {
			reason = "challenge_termination_failed"
		}
	}
	auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
	if purpose == auth.ChallengePurposeFederatedLink {
		log.Printf("Microsoft identity link rejected: reason=%s", reason)
		http.Redirect(w, r, "/settings/security?microsoft_link_failed=1", http.StatusSeeOther)
		return
	}
	log.Printf("Microsoft application login rejected: reason=%s", reason)
	http.Redirect(w, r, "/login?error=auth_failed", http.StatusSeeOther)
}
