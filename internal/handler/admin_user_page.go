package handler

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// adminUserPageTabs are the tabs of a person's page, each an address below
// it.
var adminUserPageTabs = []string{views.AdminUserTabOverview, views.AdminUserTabSignIn}

// adminUserPageAddress is the person's page path names, rebuilt from what it
// understands: a plausible user ID, one of the tabs, and the Users list it
// was opened from. ok is false for anything else.
func adminUserPageAddress(parsed *url.URL) (string, bool) {
	rest, found := strings.CutPrefix(parsed.Path, "/admin/users/")
	if !found {
		return "", false
	}
	userID, tab, _ := strings.Cut(rest, "/")
	if !adminUserActionTargetID(userID) || userID == "export" || userID == "bulk" || userID == "invitations" {
		return "", false
	}
	known := false
	for _, candidate := range adminUserPageTabs {
		if tab == candidate {
			known = true
		}
	}
	if !known {
		return "", false
	}
	values := parsed.Query()
	list := ""
	for key, given := range values {
		if key != "list" || len(given) != 1 {
			return "", false
		}
		// Only a Users list: a person's page never leads back to another's.
		if list = adminUsersListFrom(given[0]); list == "" || !strings.HasPrefix(list, adminUsersPath) || strings.HasPrefix(list, adminUsersPath+"/") {
			return "", false
		}
	}
	return views.AdminUserPagePath(userID, tab, list), true
}

// handleAdminUserPage answers tab of a person's page.
func (h *Handler) handleAdminUserPage(tab string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if h.auth == nil || !h.auth.IsEnabled() {
			http.NotFound(w, r)
			return
		}
		user, session := auth.GetCurrentUser(ctx), auth.GetCurrentSession(ctx)
		if user == nil || session == nil {
			http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
			return
		}
		list := adminUsersListFrom(r.URL.Query().Get("list"))
		if strings.HasPrefix(list, adminUsersPath+"/") {
			list = ""
		}
		userID := r.PathValue("userID")
		detail, err := h.auth.GetAdministratorUser(ctx, user.ID, session.ID, userID)
		if !adminUserActionTargetID(userID) || errors.Is(err, auth.ErrAdministratorUserNotFound) {
			h.setAdminFlashError(w, i18n.T(ctx, "admin.user.gone"))
			redirectAdmin(w, r, adminUsersPageBack(list))
			return
		}
		if errors.Is(err, auth.ErrAdministratorRequired) {
			http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
			return
		}
		if err != nil {
			log.Printf("load administrator user %q: %v", userID, err)
			http.Error(w, i18n.T(ctx, "server.error.failed_to_load_administrator_users"), http.StatusInternalServerError)
			return
		}
		policy, err := h.auth.InstanceSecurityPolicy(ctx)
		if err != nil {
			log.Printf("load administrator user MFA policy: %v", err)
			http.Error(w, i18n.T(ctx, "server.error.failed_to_load_administrator_users"), http.StatusInternalServerError)
			return
		}
		pagePath := views.AdminUserPagePath(detail.ID, tab, list)
		row := adminUsersViewData([]auth.AdministratorUserSummary{detail.AdministratorUserSummary}, user.ID, policy.MFA).Users[0]
		// A change made from the page comes back to it.
		row.ListPath = pagePath
		data := views.AdminUserPageData{
			User: row, Tab: tab, ListPath: list,
			CreatedAt: detail.CreatedAt, DisabledAt: detail.DisabledAt, DeletionStartedAt: detail.DeletionStartedAt,
			PasswordChangedAt: detail.PasswordChangedAt, Passkeys: detail.Passkeys, PasskeyLastUsedAt: detail.PasskeyLastUsedAt,
			AuthenticatorAppSince: detail.AuthenticatorAppSince, AuthenticatorAppLastUsedAt: detail.AuthenticatorAppLastUsedAt,
			RecoveryCodesLeft: detail.RecoveryCodesLeft, WorkingSessions: detail.WorkingSessions,
		}
		for _, identity := range detail.Identities {
			data.Identities = append(data.Identities, views.AdminUserIdentityData{
				Provider: identity.Provider, Email: identity.Email, LinkedAt: identity.LinkedAt, LastUsedAt: identity.LastUsedAt,
			})
		}
		verification := views.AdminSecurityVerificationData{}
		access, err := h.auth.GetSecuritySettingsAccess(ctx, auth.GetSessionToken(r))
		switch {
		case err == nil:
			if data.StepUpRequired = !access.StepUpFresh; data.StepUpRequired {
				verification = adminSecurityVerificationViewData(ctx, access, pagePath, r.URL.Query().Get("verification_required") == "1")
			}
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			redirectAdmin(w, r, "/admin/login")
			return
		default:
			log.Printf("load administrator verification state: %v", err)
			http.Error(w, i18n.T(ctx, "server.error.failed_to_verify_administrator_session"), http.StatusInternalServerError)
			return
		}
		h.renderAdminPage(w, r, http.StatusOK, views.AdminShellPage{
			Section: views.AdminSectionUsers,
			Content: views.AdminUserPage(data, verification),
		})
	}
}

func adminUsersPageBack(list string) string {
	if list == "" {
		return adminUsersPath
	}
	return list
}
