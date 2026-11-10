package handler

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// adminAuditPageSize is how many administrative changes one page shows.
const adminAuditPageSize = 50

// handleAdminAuditActivity shows the Activity page's administration tab:
// the audit log of administrative changes, newest first, filtered by kind.
// Like sign-in activity, reading it needs a recent verification.
func (h *Handler) handleAdminAuditActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	kind, before := adminAuditActivityQuery(r.URL.Query())
	data := views.AdminAuditActivityData{ManagedMode: h.auth != nil && h.auth.IsEnabled(), Kind: kind, Before: before}
	verification := views.AdminSecurityVerificationData{}
	if data.ManagedMode {
		access, err := h.auth.GetSecuritySettingsAccess(ctx, auth.GetSessionToken(r))
		if errors.Is(err, auth.ErrSecuritySessionInvalid) {
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		if err != nil {
			log.Printf("authorize administration activity: %v", err)
			http.Error(w, i18n.T(ctx, "server.error.failed_to_authorize_administrator_security_activity"), http.StatusInternalServerError)
			return
		}
		returnTo := adminAuditActivityReturnTo(kind, before)
		if !access.StepUpFresh {
			data.StepUpRequired = true
			verification = adminSecurityVerificationViewData(ctx, access, returnTo, r.URL.Query().Get("verification_required") == "1")
		} else {
			user, session := auth.GetCurrentUser(ctx), auth.GetCurrentSession(ctx)
			actorUserID, actorSessionID := "", ""
			if user != nil {
				actorUserID = user.ID
			}
			if session != nil {
				actorSessionID = session.ID
			}
			filter := audit.Filter{}
			if kind != "" {
				filter.Action = kind + "."
			}
			page, err := h.auth.ListAdministrationEvents(ctx, actorUserID, actorSessionID, filter, before, adminAuditPageSize)
			switch {
			case err == nil:
				data.Records = page.Records
				data.Next = page.Next
			case errors.Is(err, auth.ErrRecentStepUpRequired):
				data.StepUpRequired = true
				access.StepUpFresh = false
				verification = adminSecurityVerificationViewData(ctx, access, returnTo, true)
			case errors.Is(err, auth.ErrAdministratorRequired):
				http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
				return
			default:
				log.Printf("load administration activity: %v", err)
				http.Error(w, i18n.T(ctx, "server.error.failed_to_load_administrator_security_activity"), http.StatusInternalServerError)
				return
			}
		}
	}
	h.renderAdminPage(w, r, http.StatusOK, views.AdminShellPage{
		Section: views.AdminSectionActivity,
		Content: views.AdminAuditActivityPage(data, verification),
	})
}

// adminAuditActivityQuery reads the tab's filter and cursor, ignoring
// values it doesn't know.
func adminAuditActivityQuery(query url.Values) (string, int64) {
	kind := query.Get("kind")
	if !slices.Contains(views.AdminAuditKinds, kind) {
		kind = ""
	}
	before, err := strconv.ParseInt(query.Get("before"), 10, 64)
	if err != nil || before < 0 {
		before = 0
	}
	return kind, before
}

// adminAuditActivityReturnTo is the tab's address for a filter and cursor,
// where verification leads back to.
func adminAuditActivityReturnTo(kind string, before int64) string {
	values := url.Values{}
	if kind != "" {
		values.Set("kind", kind)
	}
	if before > 0 {
		values.Set("before", strconv.FormatInt(before, 10))
	}
	if len(values) == 0 {
		return views.AdminAuditActivityPath
	}
	return views.AdminAuditActivityPath + "?" + values.Encode()
}
