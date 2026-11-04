package handler

import (
	"bytes"
	"context"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
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

func securitySessionViewData(ctx context.Context, list *auth.SecuritySessionList, oidcName string) ([]views.SecuritySessionData, bool) {
	if list == nil {
		return nil, false
	}
	result := make([]views.SecuritySessionData, 0, len(list.Sessions))
	for _, session := range list.Sessions {
		view := views.SecuritySessionData{
			Client:         securitySessionClientLabel(ctx, session.UserAgent),
			Authentication: securitySessionAuthenticationLabel(ctx, session.AuthenticationMethod, oidcName),
			Assurance:      securitySessionAssuranceLabel(ctx, session.AssuranceLevel),
			SignedInAt:     formatSecuritySessionTime(ctx, session.AuthenticatedAt),
			LastActiveAt:   formatSecuritySessionTime(ctx, session.LastUsedAt),
			Current:        session.Current,
			Active:         session.Active,
		}
		if session.RevokedAt != nil {
			view.EndedAt = formatSecuritySessionTime(ctx, *session.RevokedAt)
		}
		if session.Active && !session.Current {
			view.RevokePath = securitySessionRevokePath(session.ActionReference)
		}
		result = append(result, view)
	}
	return result, list.Truncated
}

func securitySessionAuthenticationLabel(ctx context.Context, method auth.AuthenticationMethod, oidcName string) string {
	switch method {
	case auth.AuthenticationMethodPassword:
		return i18n.T(ctx, "signin.password")
	case auth.AuthenticationMethodPasskey:
		return i18n.T(ctx, "security.passkey")
	case auth.AuthenticationMethodTOTP:
		return i18n.T(ctx, "server.session.authenticator_app")
	case auth.AuthenticationMethodRecoveryCode:
		return i18n.T(ctx, "signin.recovery.code_label")
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
		return i18n.T(ctx, "server.session.existing_signin")
	}
}

func securitySessionAssuranceLabel(ctx context.Context, level auth.AssuranceLevel) string {
	switch level {
	// A passkey on its own is one factor, like a password.
	case auth.AssuranceLevelSingleFactor, auth.AssuranceLevelPhishingResistant:
		return i18n.T(ctx, "server.session.single_factor")
	case auth.AssuranceLevelMultiFactor:
		return i18n.T(ctx, "server.session.two_factor")
	default:
		return i18n.T(ctx, "server.session.legacy_assurance")
	}
}

func securitySessionClientLabel(ctx context.Context, userAgent string) string {
	normalized := strings.Map(func(value rune) rune {
		if unicode.IsControl(value) {
			return ' '
		}
		return value
	}, strings.ToValidUTF8(userAgent, ""))
	normalized = strings.Join(strings.Fields(normalized), " ")
	if normalized == "" {
		return i18n.T(ctx, "server.session.unknown_client")
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
		browser = i18n.T(ctx, "server.session.go_http_client")
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
		return i18n.T(ctx, "server.session.browser_on_platform", "browser", browser, "platform", platform)
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

func formatSecuritySessionTime(ctx context.Context, value time.Time) string {
	return i18n.Dates(ctx).DateTime(value.Local())
}

func (h *Handler) handleSecuritySessionHistory(w http.ResponseWriter, r *http.Request) {
	setSecurityActivityHeaders(w)
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/settings/security", http.StatusSeeOther)
		return
	}
	page, err := parseSecurityActivityPage(r)
	if err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_page"), http.StatusBadRequest)
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
		http.Error(w, i18n.T(r.Context(), "server.error.unable_to_load_session_history"), http.StatusInternalServerError)
		return
	}
	sessions, _ := securitySessionViewData(r.Context(), list, h.auth.OIDCLoginName())
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
		http.Error(w, i18n.T(r.Context(), "server.error.unable_to_render_session_history"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = output.WriteTo(w)
}
