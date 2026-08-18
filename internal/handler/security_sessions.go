package handler

import (
	"strings"
	"time"
	"unicode"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

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
