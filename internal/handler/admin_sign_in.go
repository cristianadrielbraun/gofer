package handler

import (
	"errors"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/admin"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// adminSignInExtras are what the Sign-in page shows besides itself: the
// confirmation open, and a change that was refused.
type adminSignInExtras struct {
	status     int
	dialogOpen bool
	pageError  string
}

func (h *Handler) handleAdminSignIn(w http.ResponseWriter, r *http.Request) {
	h.renderAdminSignIn(w, r, adminSignInExtras{})
}

// handleAdminSignInTwoFactorDialog is the confirmation at its own address,
// which a session that verified again comes back to.
func (h *Handler) handleAdminSignInTwoFactorDialog(w http.ResponseWriter, r *http.Request) {
	h.renderAdminSignIn(w, r, adminSignInExtras{dialogOpen: true})
}

// renderAdminSignIn draws the Sign-in page: who has to use two-factor
// sign-in, and what requiring it of everyone would do now.
func (h *Handler) renderAdminSignIn(w http.ResponseWriter, r *http.Request, extras adminSignInExtras) {
	ctx := r.Context()
	if h.auth == nil || !h.auth.IsEnabled() {
		http.NotFound(w, r)
		return
	}
	actor, ok := admin.ActorFromContext(ctx)
	if !ok {
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	preview, err := h.auth.PreviewInstanceMFAPolicy(ctx, actor.UserID)
	if admin.CodeOf(err) == admin.CodeForbidden {
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	if err != nil {
		log.Printf("load the instance two-factor policy: %v", err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_load_sign_in_policy"), http.StatusInternalServerError)
		return
	}
	data := views.AdminSignInData{
		Everyone:  preview.Policy.MFA.RequiresAllUsers(),
		UpdatedAt: preview.Policy.UpdatedAt, UpdatedByUsername: preview.UpdatedByUsername,
		WithoutAppTotal: preview.WithoutAppTotal, SessionsToEnd: preview.SessionsToEnd,
		CSRFToken:  auth.CSRFToken(ctx, http.MethodPost, views.AdminSignInTwoFactorPath),
		DialogOpen: extras.dialogOpen,
	}
	for _, person := range preview.WithoutApp {
		data.WithoutApp = append(data.WithoutApp, views.AdminSignInPerson{ID: person.ID, Username: person.Username})
	}

	verification := views.AdminSecurityVerificationData{}
	access, err := h.auth.GetSecuritySettingsAccess(ctx, auth.GetSessionToken(r))
	switch {
	case err == nil:
		if data.StepUpRequired = !access.StepUpFresh; data.StepUpRequired {
			// Verifying again leads back to the confirmation when it was open.
			returnTo := views.AdminSignInPath
			if extras.dialogOpen || extras.pageError != "" {
				returnTo = views.AdminSignInTwoFactorPath
				data.DialogOpen = false
			}
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
	status := extras.status
	if status == 0 {
		status = http.StatusOK
	}
	h.renderAdminPage(w, r, status, views.AdminShellPage{
		Section: views.AdminSectionSignIn,
		Content: views.AdminSignInPage(data, verification),
		Toasts:  adminErrorToasts(extras.pageError),
	})
}

// handleSetAdminSignInTwoFactor requires two-factor sign-in of everyone, or
// of administrators only again.
func (h *Handler) handleSetAdminSignInTwoFactor(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	policy := auth.InstanceMFAPolicy(r.PostFormValue("policy"))
	if !policy.Valid() {
		h.renderAdminSignIn(w, r, adminSignInExtras{status: http.StatusBadRequest, pageError: i18n.T(ctx, "auth.admin.sign_in.request_invalid")})
		return
	}
	result, err := h.auth.SetInstanceMFAPolicy(ctx, auth.SetInstanceMFAPolicyOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, Policy: policy,
	})
	switch {
	case err == nil:
	case admin.CodeOf(err) == admin.CodeForbidden, errors.Is(err, auth.ErrAdministratorRequired):
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return
	case admin.CodeOf(err) == admin.CodeVerificationRequired:
		h.renderAdminSignIn(w, r, adminSignInExtras{status: http.StatusForbidden, pageError: i18n.T(ctx, "auth.admin.verify.instance_policy")})
		return
	default:
		log.Printf("change the instance two-factor policy: %v", err)
		h.renderAdminSignIn(w, r, adminSignInExtras{status: http.StatusInternalServerError, pageError: i18n.T(ctx, "auth.admin.sign_in.change_failed")})
		return
	}
	switch {
	case !result.Changed:
		h.setAdminFlashSuccess(w, i18n.T(ctx, "auth.admin.sign_in.unchanged"))
	case policy.RequiresAllUsers():
		h.setAdminFlashSuccess(w, i18n.N(ctx, "auth.admin.sign_in.required_everyone", int(result.RevokedSessions), "count", i18n.Number(ctx, int(result.RevokedSessions))))
	default:
		h.setAdminFlashSuccess(w, i18n.T(ctx, "auth.admin.sign_in.required_administrators"))
	}
	redirectAdmin(w, r, views.AdminSignInPath)
}
