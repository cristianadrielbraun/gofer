package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/admin"
	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// adminUserPageTabs are the tabs of a person's page, each an address below
// it.
var adminUserPageTabs = []string{views.AdminUserTabOverview, views.AdminUserTabSignIn, views.AdminUserTabSessions, views.AdminUserTabMailboxes, views.AdminUserTabActivity}

// adminUserPageAddress is the person's page path names, rebuilt from what it
// understands: a plausible user ID, one of the tabs, and the Users list it
// was opened from. ok is false for anything else.
func adminUserPageAddress(parsed *url.URL) (string, bool) {
	userID, tab, list, ok := adminUserPageParts(parsed)
	if !ok {
		return "", false
	}
	return views.AdminUserPagePath(userID, tab, list), true
}

// adminUserPageParts reads a person's page address: whose it is, which tab,
// and the Users list it was opened from.
func adminUserPageParts(parsed *url.URL) (userID, tab, list string, ok bool) {
	rest, found := strings.CutPrefix(parsed.Path, "/admin/users/")
	if !found {
		return "", "", "", false
	}
	userID, tab, _ = strings.Cut(rest, "/")
	if !adminUserActionTargetID(userID) || userID == "export" || userID == "bulk" || userID == "invitations" {
		return "", "", "", false
	}
	known := false
	for _, candidate := range adminUserPageTabs {
		if tab == candidate {
			known = true
		}
	}
	if !known {
		return "", "", "", false
	}
	for key, given := range parsed.Query() {
		if key != "list" || len(given) != 1 {
			return "", "", "", false
		}
		// Only a Users list: a person's page never leads back to another's.
		if list = adminUsersListFrom(given[0]); list == "" || !strings.HasPrefix(list, adminUsersPath) || strings.HasPrefix(list, adminUsersPath+"/") {
			return "", "", "", false
		}
	}
	return userID, tab, list, true
}

// adminUserPageFrom reads value as a person's page, such as the one a
// change was made from.
func adminUserPageFrom(value string) (userID, tab, list string, ok bool) {
	parsed, err := url.Parse(value)
	if value == "" || err != nil || parsed.Scheme != "" || parsed.Host != "" {
		return "", "", "", false
	}
	return adminUserPageParts(parsed)
}

// adminUserPageExtras are what a person's page shows over itself: a
// change's dialog, or what a change issued, shown once.
type adminUserPageExtras struct {
	status          int
	dialog          *views.AdminUserDialog
	invitation      *views.AdminUserInvitationData
	credentialReset *views.AdminUserCredentialResetData
	// returnTo is where verification leads back to; the page itself when
	// empty.
	returnTo string
}

// handleAdminUserPage answers tab of a person's page.
func (h *Handler) handleAdminUserPage(tab string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list := adminUsersListFrom(r.URL.Query().Get("list"))
		if strings.HasPrefix(list, adminUsersPath+"/") {
			list = ""
		}
		h.renderAdminUserPage(w, r, r.PathValue("userID"), tab, list, adminUserPageExtras{})
	}
}

// renderAdminUserPage draws tab of userID's page, opened from list, with
// extras over it.
func (h *Handler) renderAdminUserPage(w http.ResponseWriter, r *http.Request, userID, tab, list string, extras adminUserPageExtras) {
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
		Dialog: extras.dialog, Invitation: extras.invitation, CredentialReset: extras.credentialReset,
		CreatedAt: detail.CreatedAt, DisabledAt: detail.DisabledAt, DeletionStartedAt: detail.DeletionStartedAt,
		PasswordChangedAt: detail.PasswordChangedAt, Passkeys: detail.Passkeys, PasskeyLastUsedAt: detail.PasskeyLastUsedAt,
		AuthenticatorAppSince: detail.AuthenticatorAppSince, AuthenticatorAppLastUsedAt: detail.AuthenticatorAppLastUsedAt,
		RecoveryCodesLeft: detail.RecoveryCodesLeft, WorkingSessions: detail.WorkingSessions,
	}
	if data.Dialog != nil {
		data.Dialog.User.ListPath = pagePath
	}
	for _, identity := range detail.Identities {
		data.Identities = append(data.Identities, views.AdminUserIdentityData{
			Provider: identity.Provider, Email: identity.Email, LinkedAt: identity.LinkedAt, LastUsedAt: identity.LastUsedAt,
		})
	}
	if tab == views.AdminUserTabSessions {
		sessions, err := h.auth.ListAdministratorUserSessions(ctx, user.ID, session.ID, detail.ID)
		if err != nil {
			log.Printf("load administrator user %q sessions: %v", detail.ID, err)
			http.Error(w, i18n.T(ctx, "server.error.failed_to_load_administrator_users"), http.StatusInternalServerError)
			return
		}
		data.Sessions = adminUserSessionsViewData(ctx, sessions, row, h.auth.OIDCLoginName())
	}
	if tab == views.AdminUserTabMailboxes {
		if data.Mailboxes, data.HasMailboxes, err = h.adminUserMailboxes(ctx, user, detail.AdministratorUserSummary); err != nil {
			log.Printf("load administrator user %q mailboxes: %v", detail.ID, err)
			http.Error(w, i18n.T(ctx, "admin.user.mailboxes_failed"), http.StatusInternalServerError)
			return
		}
	}
	returnTo := extras.returnTo
	if returnTo == "" {
		returnTo = pagePath
	}
	verification := views.AdminSecurityVerificationData{}
	access, err := h.auth.GetSecuritySettingsAccess(ctx, auth.GetSessionToken(r))
	switch {
	case err == nil:
		if data.StepUpRequired = !access.StepUpFresh; data.StepUpRequired {
			verification = adminSecurityVerificationViewData(ctx, access, returnTo, r.URL.Query().Get("verification_required") == "1")
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
	// Activity needs a recently verified session; a stale one sees the tab
	// locked, and nothing is read or refused.
	if tab == views.AdminUserTabActivity && !data.StepUpRequired {
		err := h.loadAdminUserActivity(ctx, user.ID, session.ID, detail.AdministratorUserSummary, &data)
		switch {
		case err == nil:
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			data.StepUpRequired = true
			verification = adminSecurityVerificationViewData(ctx, access, returnTo, false)
		default:
			log.Printf("load administrator user %q activity: %v", detail.ID, err)
			http.Error(w, i18n.T(ctx, "admin.user.activity_failed"), http.StatusInternalServerError)
			return
		}
	}
	status := extras.status
	if status == 0 {
		status = http.StatusOK
	}
	h.renderAdminPage(w, r, status, views.AdminShellPage{
		Section: views.AdminSectionUsers,
		Content: views.AdminUserPage(data, verification),
	})
}

func adminUsersPageBack(list string) string {
	if list == "" {
		return adminUsersPath
	}
	return list
}

// adminUserSessionsViewData is the Sessions tab's table. A session can be
// signed out alone when the person can be signed out at all.
func adminUserSessionsViewData(ctx context.Context, sessions []auth.AdministratorUserSession, user views.AdminUserData, oidcName string) []views.AdminUserSessionData {
	result := make([]views.AdminUserSessionData, 0, len(sessions))
	for index, session := range sessions {
		view := views.AdminUserSessionData{
			UserAgent:       session.UserAgent,
			Authentication:  securitySessionAuthenticationLabel(ctx, session.AuthenticationMethod, oidcName),
			Assurance:       securitySessionAssuranceLabel(ctx, session.AssuranceLevel),
			AuthenticatedAt: session.AuthenticatedAt, LastUsedAt: session.LastUsedAt, ExpiresAt: session.ExpiresAt,
			DialogID: "admin-user-session-" + strconv.Itoa(index),
		}
		if user.SignOutPath != "" {
			view.SignOutPath = adminUserSessionSignOutPath(user.ID, session.Reference)
			view.CSRFToken = auth.CSRFToken(ctx, http.MethodPost, view.SignOutPath)
		}
		result = append(result, view)
	}
	return result
}

func adminUserSessionSignOutPath(userID, reference string) string {
	return "/admin/users/" + userID + "/sessions/" + reference + "/sign-out"
}

// handleSignOutAdminUserSession ends one of someone's sessions and returns
// to the page it was ended from.
func (h *Handler) handleSignOutAdminUserSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	userID := r.PathValue("userID")
	back := views.AdminUserPagePath(userID, views.AdminUserTabSessions, "")
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(ctx, "auth.admin.form_invalid"), http.StatusBadRequest)
		return
	}
	if list := adminUsersListFrom(r.PostFormValue("list")); strings.HasPrefix(list, adminUsersPath+"/"+userID+"/") || strings.HasPrefix(list, adminUsersPath+"/"+userID+"?") {
		back = list
	}
	result, err := h.adminService().SignOutSession(ctx, actor, userID, r.PathValue("reference"))
	w.Header().Set("Cache-Control", "no-store")
	switch code := admin.CodeOf(err); code {
	case "":
		h.setAdminFlashSuccess(w, i18n.T(ctx, "admin.user.session_signed_out", "client", views.ClientLabel(ctx, result.UserAgent)))
	case admin.CodeForbidden:
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return
	case admin.CodeVerificationRequired:
		http.Redirect(w, r, adminUsersAddParameter(back, "verification_required=1"), http.StatusSeeOther)
		return
	case admin.CodeUnavailable:
		h.setAdminFlashError(w, i18n.T(ctx, "admin.user.session_gone"))
	default:
		log.Printf("sign administrator user session out: %v", err)
		h.setAdminFlashError(w, i18n.T(ctx, "auth.admin.users.sign_out_failed"))
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// loadAdminUserActivity reads the newest activity about person into data.
func (h *Handler) loadAdminUserActivity(ctx context.Context, actorUserID, actorSessionID string, person auth.AdministratorUserSummary, data *views.AdminUserPageData) error {
	events, err := h.auth.ListAdministratorUserSecurityEvents(ctx, actorUserID, actorSessionID, person.ID, views.AdminUserActivityLimit)
	if err != nil {
		return err
	}
	data.SecurityEvents = adminSecurityActivityViewData(ctx, &auth.AdministratorSecurityEventPage{Events: events, Page: 1, TotalPages: 1}).Events
	changes, err := h.auth.ListAdministrationEvents(ctx, actorUserID, actorSessionID, audit.Filter{TargetKind: "user", TargetID: person.ID}, 0, views.AdminUserActivityLimit)
	if err != nil {
		return err
	}
	data.Changes = changes.Records
	if person.IsAdmin {
		data.ShowChangesMade = true
		made, err := h.auth.ListAdministrationEvents(ctx, actorUserID, actorSessionID, audit.Filter{ActorID: person.ID}, 0, views.AdminUserActivityLimit)
		if err != nil {
			return err
		}
		data.ChangesMade = made.Records
	}
	return nil
}
