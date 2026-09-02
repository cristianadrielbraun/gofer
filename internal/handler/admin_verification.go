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
		if len(values) > 2 {
			return ""
		}
		for key := range values {
			if key != "page" && key != "filter" {
				return ""
			}
		}
		page := int64(1)
		if pages, ok := values["page"]; ok {
			if len(pages) != 1 {
				return ""
			}
			page, err = strconv.ParseInt(pages[0], 10, 64)
			if err != nil || page < 1 {
				return ""
			}
		}
		filter := auth.AdministratorSecurityEventFilterAll
		if filters, ok := values["filter"]; ok {
			if len(filters) != 1 || filters[0] == "" {
				return ""
			}
			filter = auth.AdministratorSecurityEventFilter(filters[0])
			if !filter.Valid() || filter == auth.AdministratorSecurityEventFilterAll {
				return ""
			}
		}
		if _, hasPage := values["page"]; !hasPage {
			if _, hasFilter := values["filter"]; !hasFilter {
				return ""
			}
		}
		return adminSecurityActivityReturnTo(page, filter)
	default:
		return ""
	}
}

func adminSecurityActivityReturnTo(page int64, filter auth.AdministratorSecurityEventFilter) string {
	values := url.Values{}
	if filter != auth.AdministratorSecurityEventFilterAll {
		values.Set("filter", string(filter))
	}
	if page > 1 {
		values.Set("page", strconv.FormatInt(page, 10))
	}
	if len(values) == 0 {
		return adminSecurityActivityPath
	}
	return adminSecurityActivityPath + "?" + values.Encode()
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
