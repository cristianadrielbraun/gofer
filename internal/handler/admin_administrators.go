package handler

import (
	"errors"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// adminAdministratorsExtras are what the Administrators page shows over its
// list: the invitation form as it was sent, a new invitation or sign-in
// reset, a change's dialog, or a change that was refused.
type adminAdministratorsExtras struct {
	status          int
	form            views.AdminUserInvitationFormData
	result          *views.AdminUserInvitationData
	credentialReset *views.AdminUserCredentialResetData
	dialog          *views.AdminUserDialog
	pageError       string
	// returnTo is where verification leads back to; the page when empty.
	returnTo string
}

func (h *Handler) handleAdminAdministrators(w http.ResponseWriter, r *http.Request) {
	h.renderAdminAdministrators(w, r, adminAdministratorsExtras{})
}

// renderAdminAdministrators draws the Administrators page: every
// administrator, pending ones included, with how they sign in.
func (h *Handler) renderAdminAdministrators(w http.ResponseWriter, r *http.Request, extras adminAdministratorsExtras) {
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
	page, err := h.auth.ListAdministratorUserPage(ctx, user.ID, session.ID, auth.AdministratorUserQuery{
		Administrators: true, Sort: auth.AdministratorUserSortStatus, Limit: auth.MaxAdministratorUserPageSize,
	})
	if errors.Is(err, auth.ErrAdministratorRequired) {
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	if err != nil {
		log.Printf("load administrators: %v", err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_load_administrator_users"), http.StatusInternalServerError)
		return
	}
	policy, err := h.auth.InstanceSecurityPolicy(ctx)
	if err != nil {
		log.Printf("load administrators' MFA policy: %v", err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_load_administrator_users"), http.StatusInternalServerError)
		return
	}
	rows := adminUsersViewData(page.Users, user.ID, policy.MFA).Users
	data := views.AdminAdministratorsData{Dialog: extras.dialog, Result: extras.result, CredentialReset: extras.credentialReset}
	for i, person := range page.Users {
		row := rows[i]
		row.ListPath = views.AdminAdministratorsPath
		administrator := views.AdminAdministratorData{User: row, HasSecondFactor: person.HasSecondFactor}
		if person.Status != auth.UserStatusPending {
			detail, err := h.auth.GetAdministratorUser(ctx, user.ID, session.ID, person.ID)
			if err != nil {
				log.Printf("load administrator %q: %v", person.ID, err)
				http.Error(w, i18n.T(ctx, "server.error.failed_to_load_administrator_users"), http.StatusInternalServerError)
				return
			}
			administrator.Passkeys, administrator.RecoveryCodesLeft, administrator.WorkingSessions = detail.Passkeys, detail.RecoveryCodesLeft, detail.WorkingSessions
		}
		data.Administrators = append(data.Administrators, administrator)
	}
	if data.Dialog != nil {
		data.Dialog.User.ListPath = views.AdminAdministratorsPath
	}

	verification := views.AdminSecurityVerificationData{}
	access, err := h.auth.GetSecuritySettingsAccess(ctx, auth.GetSessionToken(r))
	switch {
	case err == nil:
		returnTo := extras.returnTo
		if returnTo == "" {
			returnTo = views.AdminAdministratorsPath
		}
		if data.StepUpRequired = !access.StepUpFresh; data.StepUpRequired {
			open := extras.pageError != "" || r.URL.Query().Get("verification_required") == "1"
			verification = adminSecurityVerificationViewData(ctx, access, returnTo, open)
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
	form := extras.form
	form.Administrator = true
	data.Invitation = views.AdminUsersData{
		InvitationForm: form, StepUpRequired: data.StepUpRequired,
		InvitationCSRFToken: auth.CSRFToken(ctx, http.MethodPost, adminUserInvitationPath),
	}
	status := extras.status
	if status == 0 {
		status = http.StatusOK
	}
	h.renderAdminPage(w, r, status, views.AdminShellPage{
		Section: views.AdminSectionAdministrators,
		Content: views.AdminAdministratorsPage(data, verification),
		Toasts:  adminErrorToasts(extras.pageError),
	})
}
