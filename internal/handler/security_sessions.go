package handler

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const securitySessionRevokeOthersPath = "/settings/security/sessions/revoke-others"

func securitySessionRevokePath(actionReference string) string {
	if actionReference == "" {
		return ""
	}
	return "/settings/security/sessions/" + url.PathEscape(actionReference) + "/revoke"
}

func (h *Handler) handleSecuritySessionRevoke(w http.ResponseWriter, r *http.Request) {
	result, err := h.auth.RevokeSecuritySession(
		r.Context(), auth.GetSessionToken(r), r.PathValue("reference"), r.UserAgent(),
	)
	if err != nil {
		h.handleSecuritySessionRevocationError(w, r, err, "revoke security session")
		return
	}
	if result == nil || result.RevokedSessions != 1 {
		log.Printf("revoke security session returned unexpected result: %#v", result)
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to sign out that session right now. Please try again.")
		return
	}
	http.Redirect(w, r, "/settings/security?session_revoked=1", http.StatusSeeOther)
}

func (h *Handler) handleSecuritySessionRevokeOthers(w http.ResponseWriter, r *http.Request) {
	result, err := h.auth.RevokeOtherSecuritySessions(
		r.Context(), auth.GetSessionToken(r), r.UserAgent(),
	)
	if err != nil {
		h.handleSecuritySessionRevocationError(w, r, err, "revoke other security sessions")
		return
	}
	if result == nil {
		log.Printf("revoke other security sessions returned no result")
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to sign out other sessions right now. Please try again.")
		return
	}
	redirect := "/settings/security?other_sessions_revoked=1"
	if result.RevokedSessions == 0 {
		redirect = "/settings/security?other_sessions_unchanged=1"
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (h *Handler) handleSecuritySessionRevocationError(w http.ResponseWriter, r *http.Request, err error, operation string) {
	switch {
	case errors.Is(err, auth.ErrRecentStepUpRequired):
		http.Redirect(w, r, "/settings/security?verification_required=1", http.StatusSeeOther)
	case errors.Is(err, auth.ErrSecuritySessionInvalid):
		auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	case errors.Is(err, auth.ErrSecuritySessionTargetInvalid):
		http.Redirect(w, r, "/settings/security?session_unavailable=1", http.StatusSeeOther)
	default:
		log.Printf("%s: %v", operation, err)
		h.renderSecurityManagementError(w, r, http.StatusInternalServerError, "Unable to update signed-in sessions right now. Please try again.")
	}
}

func securitySessionViewData(list *auth.SecuritySessionList, oidcName string) ([]views.SecuritySessionData, bool) {
	if list == nil {
		return nil, false
	}
	result := make([]views.SecuritySessionData, 0, len(list.Sessions))
	for _, session := range list.Sessions {
		view := views.SecuritySessionData{
			Client:         securitySessionClientLabel(session.UserAgent),
			Authentication: securitySessionAuthenticationLabel(session.AuthenticationMethod, oidcName),
			Assurance:      securitySessionAssuranceLabel(session.AssuranceLevel),
			SignedInAt:     formatSecuritySessionTime(session.AuthenticatedAt),
			LastActiveAt:   formatSecuritySessionTime(session.LastUsedAt),
			Current:        session.Current,
			Active:         session.Active,
		}
		if session.RevokedAt != nil {
			view.EndedAt = formatSecuritySessionTime(*session.RevokedAt)
		}
		if session.Active && !session.Current {
			view.RevokePath = securitySessionRevokePath(session.ActionReference)
		}
		result = append(result, view)
	}
	return result, list.Truncated
}

func securitySessionAuthenticationLabel(method auth.AuthenticationMethod, oidcName string) string {
	switch method {
	case auth.AuthenticationMethodPassword:
		return "Password"
	case auth.AuthenticationMethodPasskey:
		return "Passkey"
	case auth.AuthenticationMethodTOTP:
		return "Authenticator app"
	case auth.AuthenticationMethodRecoveryCode:
		return "Recovery code"
	case auth.AuthenticationMethodFederatedGoogle:
		return "Google"
	case auth.AuthenticationMethodFederatedMicrosoft:
		return "Microsoft"
	case auth.AuthenticationMethodFederatedOIDC:
		if strings.TrimSpace(oidcName) != "" {
			return strings.TrimSpace(oidcName)
		}
		return "OpenID Connect"
	default:
		return "Existing sign-in"
	}
}

func securitySessionAssuranceLabel(level auth.AssuranceLevel) string {
	switch level {
	case auth.AssuranceLevelSingleFactor:
		return "Single factor"
	case auth.AssuranceLevelMultiFactor:
		return "Multi-factor"
	case auth.AssuranceLevelPhishingResistant:
		return "Phishing-resistant"
	default:
		return "Legacy assurance"
	}
}

func securitySessionClientLabel(userAgent string) string {
	normalized := strings.Map(func(value rune) rune {
		if unicode.IsControl(value) {
			return ' '
		}
		return value
	}, strings.ToValidUTF8(userAgent, ""))
	normalized = strings.Join(strings.Fields(normalized), " ")
	if normalized == "" {
		return "Unknown browser or device"
	}
	lower := strings.ToLower(normalized)
	browser := ""
	switch {
	case strings.Contains(lower, "edg/") || strings.Contains(lower, "edga/") || strings.Contains(lower, "edgios/"):
		browser = "Microsoft Edge"
	case strings.Contains(lower, "opr/"):
		browser = "Opera"
	case strings.Contains(lower, "firefox/") || strings.Contains(lower, "fxios/"):
		browser = "Firefox"
	case strings.Contains(lower, "chrome/") || strings.Contains(lower, "crios/"):
		browser = "Chrome"
	case strings.Contains(lower, "safari/") && strings.Contains(lower, "version/"):
		browser = "Safari"
	case strings.Contains(lower, "curl/"):
		browser = "curl"
	case strings.Contains(lower, "wget/"):
		browser = "Wget"
	case strings.Contains(lower, "go-http-client/"):
		browser = "Go HTTP client"
	case strings.Contains(lower, "postmanruntime/"):
		browser = "Postman"
	}
	platform := ""
	switch {
	case strings.Contains(lower, "iphone"):
		platform = "iPhone"
	case strings.Contains(lower, "ipad"):
		platform = "iPad"
	case strings.Contains(lower, "android"):
		platform = "Android"
	case strings.Contains(lower, "windows"):
		platform = "Windows"
	case strings.Contains(lower, "cros"):
		platform = "ChromeOS"
	case strings.Contains(lower, "macintosh") || strings.Contains(lower, "mac os x"):
		platform = "macOS"
	case strings.Contains(lower, "linux"):
		platform = "Linux"
	}
	if browser != "" && platform != "" {
		return browser + " on " + platform
	}
	if browser != "" {
		return browser
	}
	runes := []rune(normalized)
	if len(runes) > 96 {
		return string(runes[:95]) + "…"
	}
	return normalized
}

func formatSecuritySessionTime(value time.Time) string {
	return value.Local().Format("Jan 2, 2006 at 3:04 PM")
}

func (h *Handler) handleSecuritySessionHistory(w http.ResponseWriter, r *http.Request) {
	setSecurityActivityHeaders(w)
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/settings/security", http.StatusSeeOther)
		return
	}
	page, err := parseSecurityActivityPage(r)
	if err != nil {
		http.Error(w, "Invalid page", http.StatusBadRequest)
		return
	}
	list, err := h.auth.ListSecuritySessionPage(r.Context(), auth.GetSessionToken(r), max(int64(2), page))
	if errors.Is(err, auth.ErrRecentStepUpRequired) {
		w.Header().Set("HX-Redirect", "/settings/security?verification_required=1")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if errors.Is(err, auth.ErrSecuritySessionInvalid) {
		auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		log.Printf("load session history: %v", err)
		http.Error(w, "Unable to load session history", http.StatusInternalServerError)
		return
	}
	sessions, _ := securitySessionViewData(list, h.auth.OIDCLoginName())
	// If history disappeared while the dialog was open, do not repeat the main card.
	if list.Page == 1 {
		sessions = nil
	}
	csrf := make(map[string]string)
	for _, session := range sessions {
		if session.RevokePath != "" {
			csrf[session.RevokePath] = auth.CSRFToken(r.Context(), http.MethodPost, session.RevokePath)
		}
	}
	var output bytes.Buffer
	if err := views.SecuritySessionHistoryPage(sessions, csrf, list.Page, list.TotalPages).Render(r.Context(), &output); err != nil {
		http.Error(w, "Unable to render session history", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = output.WriteTo(w)
}
