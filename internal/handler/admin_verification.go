package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func adminSecurityVerificationViewData(
	ctx context.Context,
	access *auth.SecuritySettingsAccess,
	returnTo string,
	open bool,
) views.AdminSecurityVerificationData {
	if access == nil || access.StepUpFresh {
		return views.AdminSecurityVerificationData{}
	}
	returnTo = adminSecurityVerificationReturnTo(returnTo)
	if returnTo == "" {
		returnTo = "/admin/users"
	}
	data := views.AdminSecurityVerificationData{
		Required:   true,
		Open:       open,
		HasTOTP:    access.HasTOTP,
		HasPasskey: access.HasPasskey,
		ReturnTo:   returnTo,
	}
	if data.HasTOTP {
		data.TOTPPath = securityStepUpPath
		data.TOTPCSRFToken = auth.CSRFToken(ctx, http.MethodPost, securityStepUpPath)
	}
	if data.HasPasskey {
		data.PasskeyStartPath = securityPasskeyStepUpStartPath
		data.PasskeyFinishPath = securityPasskeyStepUpFinishPath
		data.PasskeyStartCSRFToken = auth.CSRFToken(ctx, http.MethodPost, securityPasskeyStepUpStartPath)
		data.PasskeyFinishCSRFToken = auth.CSRFToken(ctx, http.MethodPost, securityPasskeyStepUpFinishPath)
	}
	return data
}

func adminSecurityVerificationReturnTo(value string) string {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(value))
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Fragment != "" {
		return ""
	}
	switch parsed.Path {
	case "/admin/users", "/admin/security":
		if parsed.RawQuery != "" {
			return ""
		}
		return parsed.Path
	case adminSecurityActivityPath:
		values := parsed.Query()
		if len(values) == 0 {
			return parsed.Path
		}
		pages, ok := values["page"]
		if !ok || len(values) != 1 || len(pages) != 1 {
			return ""
		}
		page, err := strconv.ParseInt(pages[0], 10, 64)
		if err != nil || page < 1 {
			return ""
		}
		return adminSecurityActivityPath + "?page=" + strconv.FormatInt(page, 10)
	default:
		return ""
	}
}

func adminSecurityActivityReturnTo(page int64) string {
	if page <= 1 {
		return adminSecurityActivityPath
	}
	return adminSecurityActivityPath + "?page=" + strconv.FormatInt(page, 10)
}

func securityVerificationJSONRequested(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json")
}

func writeSecurityVerificationJSON(w http.ResponseWriter, status int, payload map[string]string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeSecurityVerificationJSONError(w http.ResponseWriter, status int, message string) {
	writeSecurityVerificationJSON(w, status, map[string]string{"error": message})
}
