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

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const adminUserInvitationPath = "/admin/users/invitations"

func adminUserMFAPolicyPath(userID string) string {
	return "/admin/users/" + url.PathEscape(userID) + "/mfa-policy"
}

func adminUserStatusPath(userID string) string {
	return "/admin/users/" + url.PathEscape(userID) + "/status"
}

func adminUserCredentialResetPath(userID string) string {
	return "/admin/users/" + url.PathEscape(userID) + "/credential-reset"
}

func adminUserDeletionPath(userID string) string {
	return "/admin/users/" + url.PathEscape(userID) + "/delete"
}

func adminUserInvitationRevokePath(reference string) string {
	return adminUserInvitationPath + "/" + reference + "/revoke"
}

func adminUserInvitationRotatePath(reference string) string {
	return adminUserInvitationPath + "/" + reference + "/rotate"
}

// adminUsersViewData lists the users for the admin page. Status, role and
// two-step sign-in are codes, which the page names in its language.
func adminUsersViewData(users []auth.AdministratorUserSummary, currentUserID string, instancePolicy auth.InstanceMFAPolicy) views.AdminUsersData {
	data := views.AdminUsersData{Users: make([]views.AdminUserData, 0, len(users)), Total: len(users)}
	for _, user := range users {
		view := views.AdminUserData{
			HasPassword:              user.HasPassword,
			PasswordChangeRequired:   user.PasswordChangeRequired,
			ID:                       user.ID,
			Username:                 user.Username,
			Status:                   "disabled",
			Role:                     "webmail_user",
			Current:                  user.ID == currentUserID,
			InvitationState:          string(user.InvitationState),
			InvitationExpiresAt:      user.InvitationExpiresAt,
			MFALabel:                 "optional",
			MFADetail:                "user_choice",
			MFARequired:              user.MFARequired,
			PasswordResetRequestedAt: user.PasswordResetRequestedAt,
			DeletionPending:          user.DeletionPending,
		}
		if user.InvitationActionReference != "" {
			view.InvitationRevokePath = adminUserInvitationRevokePath(user.InvitationActionReference)
			view.InvitationRotatePath = adminUserInvitationRotatePath(user.InvitationActionReference)
		}
		switch user.Status {
		case auth.UserStatusActive:
			view.Status = "active"
			data.Active++
			if !view.Current && !user.DeletionPending {
				view.StatusPath = adminUserStatusPath(user.ID)
			}
		case auth.UserStatusPending:
			view.Status = "pending"
			data.Pending++
		case auth.UserStatusDisabled:
			data.Disabled++
			if !view.Current && !user.DeletionPending {
				view.StatusPath = adminUserStatusPath(user.ID)
			}
		}
		if user.UserType == auth.UserTypeWebmail && !user.IsAdmin &&
			!user.DeletionPending && (user.Status == auth.UserStatusActive || user.Status == auth.UserStatusDisabled) {
			view.CredentialResetPath = adminUserCredentialResetPath(user.ID)
		}
		if user.UserType == auth.UserTypeWebmail && !user.IsAdmin && !view.Current &&
			!user.DeletionProtected && user.Status == auth.UserStatusDisabled {
			view.DeletionPath = adminUserDeletionPath(user.ID)
		}
		if user.IsAdmin {
			view.Role = "management_admin"
			data.Administrators++
		} else if user.UserType == auth.UserTypeManagement {
			view.Role = "management_user"
		}
		if user.UserType == auth.UserTypeWebmail && user.IsAdmin {
			view.Role = "legacy"
		}
		switch {
		case user.UserType == auth.UserTypeManagement:
			view.MFALabel = "required"
			view.MFADetail = "management_policy"
		case user.MFARequired:
			view.MFALabel = "required"
			view.MFADetail = "individual_policy"
			view.MFAPolicyPath = adminUserMFAPolicyPath(user.ID)
		case instancePolicy.RequiresAllUsers():
			view.MFALabel = "required"
			view.MFADetail = "instance_policy"
		default:
			view.MFAPolicyPath = adminUserMFAPolicyPath(user.ID)
		}
		if user.DeletionPending {
			view.MFAPolicyPath = ""
		}
		data.Users = append(data.Users, view)
	}
	return data
}

func (h *Handler) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if h.auth != nil && !h.auth.IsEnabled() {
		http.Redirect(w, r, "/admin/avatars/", http.StatusFound)
		return
	}
	h.renderAdminUsers(w, r, http.StatusOK, views.AdminUserInvitationFormData{}, nil, "")
}

func (h *Handler) handleCreateAdminUserInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	currentUser := auth.GetCurrentUser(ctx)
	currentSession := auth.GetCurrentSession(ctx)
	if currentUser == nil || currentSession == nil || h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.invitation_form_invalid"))
		return
	}
	form := views.AdminUserInvitationFormData{
		Name: r.PostFormValue("name"), Username: r.PostFormValue("username"),
	}
	invitation, err := h.auth.CreateAdministratorUserInvitation(ctx, auth.CreateAdministratorUserInvitationOptions{
		ActorUserID: currentUser.ID, ActorSessionID: currentSession.ID,
		Name: form.Name, Username: form.Username,
	})
	if err != nil {
		var validationErr *auth.AdministratorUserInvitationValidationError
		switch {
		case errors.As(err, &validationErr):
			form.FieldErrors = authFields(r.Context(), validationErr.Fields)
			h.renderAdminUsers(w, r, http.StatusBadRequest, form, nil, i18n.T(r.Context(), "auth.admin.users.invitation_fix"))
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			h.renderAdminUsers(w, r, http.StatusForbidden, form, nil, i18n.T(r.Context(), "auth.admin.verify.create_invitation"))
		case errors.Is(err, auth.ErrAdministratorRequired):
			http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		default:
			log.Printf("create administrator user invitation: %v", err)
			h.renderAdminUsers(w, r, http.StatusInternalServerError, form, nil, i18n.T(r.Context(), "auth.admin.users.invitation_create_failed"))
		}
		return
	}
	result := &views.AdminUserInvitationData{
		Name: invitation.Name, Username: invitation.User.Username,
		RedemptionURL: strings.TrimRight(h.auth.Config().BaseURL, "/") + invitationEnrollmentPath,
		Token:         invitation.Token.Token, ExpiresAt: invitation.Token.ExpiresAt,
	}
	h.renderAdminUsers(w, r, http.StatusCreated, views.AdminUserInvitationFormData{}, result, "")
}

func (h *Handler) handleRevokeAdminUserInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	currentUser := auth.GetCurrentUser(ctx)
	currentSession := auth.GetCurrentSession(ctx)
	if currentUser == nil || currentSession == nil || h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	err := h.auth.RevokeAdministratorUserInvitation(
		ctx, currentUser.ID, currentSession.ID, r.PathValue("reference"),
	)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			h.renderAdminUsers(w, r, http.StatusForbidden, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.verify.revoke_invitation"))
		case errors.Is(err, auth.ErrAdministratorRequired):
			http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		case errors.Is(err, auth.ErrAdministratorUserInvitationTargetInvalid),
			errors.Is(err, auth.ErrAdministratorUserInvitationNotActive):
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.invitation_inactive"))
		default:
			log.Printf("revoke administrator user invitation: %v", err)
			h.renderAdminUsers(w, r, http.StatusInternalServerError, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.invitation_revoke_failed"))
		}
		return
	}
	redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.invitation_revoked"))
}

func (h *Handler) handleRotateAdminUserInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	currentUser := auth.GetCurrentUser(ctx)
	currentSession := auth.GetCurrentSession(ctx)
	if currentUser == nil || currentSession == nil || h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	invitation, err := h.auth.RotateAdministratorUserInvitation(ctx, auth.RotateAdministratorUserInvitationOptions{
		ActorUserID: currentUser.ID, ActorSessionID: currentSession.ID,
		ActionReference: r.PathValue("reference"),
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			h.renderAdminUsers(w, r, http.StatusForbidden, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.verify.replace_invitation"))
		case errors.Is(err, auth.ErrAdministratorRequired):
			http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		case errors.Is(err, auth.ErrAdministratorUserInvitationTargetInvalid):
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.invitation_target_gone"))
		default:
			log.Printf("rotate administrator user invitation: %v", err)
			h.renderAdminUsers(w, r, http.StatusInternalServerError, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.invitation_replace_failed"))
		}
		return
	}
	result := &views.AdminUserInvitationData{
		Name: invitation.Name, Username: invitation.User.Username,
		RedemptionURL: strings.TrimRight(h.auth.Config().BaseURL, "/") + invitationEnrollmentPath,
		Token:         invitation.Token.Token, ExpiresAt: invitation.Token.ExpiresAt,
		Rotated: true,
	}
	h.renderAdminUsers(w, r, http.StatusCreated, views.AdminUserInvitationFormData{}, result, "")
}

func (h *Handler) handleSetAdminUserMFAPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	currentUser := auth.GetCurrentUser(ctx)
	currentSession := auth.GetCurrentSession(ctx)
	if currentUser == nil || currentSession == nil || h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.mfa_request_invalid"))
		return
	}
	var required bool
	switch r.PostFormValue("required") {
	case "true":
		required = true
	case "false":
	default:
		h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.mfa_request_invalid"))
		return
	}
	result, err := h.auth.SetAdministratorUserMFAPolicy(ctx, auth.SetAdministratorUserMFAPolicyOptions{
		ActorUserID: currentUser.ID, ActorSessionID: currentSession.ID,
		TargetUserID: r.PathValue("userID"), Required: required,
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			h.renderAdminUsers(w, r, http.StatusForbidden, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.verify.mfa_requirement"))
		case errors.Is(err, auth.ErrAdministratorRequired):
			http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		case errors.Is(err, auth.ErrAdministratorUserMFATargetInvalid):
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.user_gone"))
		default:
			log.Printf("set administrator user MFA policy: %v", err)
			h.renderAdminUsers(w, r, http.StatusInternalServerError, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.mfa_change_failed"))
		}
		return
	}
	if !result.Changed {
		redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.mfa_unchanged"))
		return
	}
	if result.Required {
		redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.mfa_required"))
		return
	}
	redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.mfa_cleared"))
}

func (h *Handler) handleSetAdminUserStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.ownedMailbox != nil {
		var cancel context.CancelFunc
		ctx, cancel = h.ownedAdminDiagnosticContext(ctx)
		defer cancel()
		r = r.WithContext(ctx)
	}
	currentUser := auth.GetCurrentUser(ctx)
	currentSession := auth.GetCurrentSession(ctx)
	if currentUser == nil || currentSession == nil || h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.status_request_invalid"))
		return
	}
	var status auth.UserStatus
	switch r.PostFormValue("status") {
	case string(auth.UserStatusActive):
		status = auth.UserStatusActive
	case string(auth.UserStatusDisabled):
		status = auth.UserStatusDisabled
	default:
		h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.status_request_invalid"))
		return
	}
	result, err := h.auth.SetAdministratorUserStatus(ctx, auth.SetAdministratorUserStatusOptions{
		ActorUserID: currentUser.ID, ActorSessionID: currentSession.ID,
		TargetUserID: r.PathValue("userID"), Status: status,
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			h.renderAdminUsers(w, r, http.StatusForbidden, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.verify.user_access"))
		case errors.Is(err, auth.ErrAdministratorRequired):
			http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		case errors.Is(err, auth.ErrLastActiveAdmin):
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.last_admin"))
		case errors.Is(err, auth.ErrInstanceMFAEnrollmentNeeded):
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.needs_totp"))
		case errors.Is(err, auth.ErrAdministratorUserStatusInvalid),
			errors.Is(err, auth.ErrAdministratorUserStatusTargetInvalid):
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.user_unavailable"))
		default:
			log.Printf("set administrator user status: %v", err)
			h.renderAdminUsers(w, r, http.StatusInternalServerError, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.access_change_failed"))
		}
		return
	}
	if err := h.refreshOwnedUserStatus(ctx, r.PathValue("userID"), result.Status); err != nil {
		log.Printf("refresh user status services: %v", err)
		h.renderAdminUsers(w, r, http.StatusServiceUnavailable, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.access_refresh_failed"))
		return
	}
	if !result.Changed {
		redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.access_unchanged"))
		return
	}
	if result.Status == auth.UserStatusDisabled {
		notice := i18n.T(r.Context(), "auth.admin.users.disabled")
		if result.RevokedSessions > 0 {
			notice = i18n.N(r.Context(), "auth.admin.users.disabled_sessions", int(result.RevokedSessions), "count", i18n.Number(r.Context(), int(result.RevokedSessions)))
		}
		redirectAdminUsers(w, r, notice)
		return
	}
	redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.enabled"))
}

func (h *Handler) handleIssueAdminUserCredentialReset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	currentUser := auth.GetCurrentUser(ctx)
	currentSession := auth.GetCurrentSession(ctx)
	if currentUser == nil || currentSession == nil || h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	targetUser, err := h.auth.GetUserByID(ctx, r.PathValue("userID"))
	if err != nil {
		log.Printf("load administrator credential-reset target: %v", err)
		h.renderAdminUsers(w, r, http.StatusInternalServerError, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.reset_failed"))
		return
	}
	if targetUser == nil || targetUser.UserType != auth.UserTypeWebmail || targetUser.IsAdmin ||
		(targetUser.Status != auth.UserStatusActive && targetUser.Status != auth.UserStatusDisabled) {
		h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.reset_user_gone"))
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "auth.admin.users.reset_form_invalid"), http.StatusBadRequest)
		return
	}
	if _, present := r.PostForm["require_password_change"]; present {
		http.Error(w, i18n.T(r.Context(), "auth.admin.users.use_password_change"), http.StatusBadRequest)
		return
	}
	token, err := h.auth.IssueEnrollmentToken(ctx, auth.IssueEnrollmentTokenOptions{
		UserID: targetUser.ID, CreatedBy: currentUser.ID, ActorSessionID: currentSession.ID,
		Purpose: auth.EnrollmentTokenPurposeCredentialReset,
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			h.renderAdminUsers(w, r, http.StatusForbidden, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.verify.reset_token"))
		case errors.Is(err, auth.ErrAdministratorRequired):
			http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		case errors.Is(err, auth.ErrEnrollmentTokenTargetInvalid):
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.reset_user_gone"))
		default:
			log.Printf("issue administrator user credential reset: %v", err)
			h.renderAdminUsers(w, r, http.StatusInternalServerError, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.reset_failed"))
		}
		return
	}
	result := &views.AdminUserCredentialResetData{
		Username:      targetUser.Username,
		RedemptionURL: strings.TrimRight(h.auth.Config().BaseURL, "/") + credentialRedemptionPath,
		Token:         token.Token, ExpiresAt: token.ExpiresAt,
	}
	h.renderAdminUsersWithCredentialReset(w, r, http.StatusCreated, result, "")
}

func (h *Handler) handleDeleteAdminUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	currentUser := auth.GetCurrentUser(ctx)
	currentSession := auth.GetCurrentSession(ctx)
	if currentUser == nil || currentSession == nil || h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.delete_request_invalid"))
		return
	}
	var owned *userRemovalWorker
	if h.ownedMailbox != nil {
		owned = h.removalWorker()
		if owned == nil || owned.ctx.Err() != nil {
			http.Error(w, i18n.T(r.Context(), "server.error.user_deletion_runtime_unavailable_retry_later"), http.StatusServiceUnavailable)
			return
		}
	}
	result, err := h.auth.PrepareAdministratorUserDeletion(ctx, auth.PrepareAdministratorUserDeletionOptions{
		ActorUserID: currentUser.ID, ActorSessionID: currentSession.ID,
		TargetUserID: r.PathValue("userID"), Confirmation: r.PostFormValue("confirmation"),
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			h.renderAdminUsers(w, r, http.StatusForbidden, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.verify.delete_user"))
		case errors.Is(err, auth.ErrAdministratorRequired):
			http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		case errors.Is(err, auth.ErrAdministratorUserDeletionConfirmationInvalid):
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.delete_confirm"))
		case errors.Is(err, auth.ErrAdministratorUserDeletionTargetInvalid):
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.delete_only_disabled"))
		default:
			log.Printf("prepare administrator user deletion: %v", err)
			h.renderAdminUsers(w, r, http.StatusInternalServerError, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.delete_failed"))
		}
		return
	}
	if owned != nil {
		owned.h.userIMAP.StopUser(result.TargetUserID)
		err := owned.enqueue(userRemovalJob{owner: result.TargetUserID, actorSession: currentSession.ID})
		if errors.Is(err, errUserRemovalBusy) {
			redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.delete_pending"))
			return
		}
		if err != nil {
			http.Error(w, i18n.T(r.Context(), "auth.admin.users.delete_stopped"), http.StatusServiceUnavailable)
			return
		}
		redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.delete_started"))
		return
	}
	for _, accountID := range result.AccountIDs {
		h.closeBodyClient(accountID)
		if h.syncer != nil {
			h.syncer.StopAccount(accountID)
		}
	}

	go func(userID, actorSessionID string) {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := h.cleanupDeletingUser(cleanupCtx, userID, actorSessionID); err != nil {
			log.Printf("delete user cleanup %s failed: %v", userID, err)
		}
	}(result.TargetUserID, currentSession.ID)

	if result.Resumed {
		redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.delete_resumed"))
		return
	}
	redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.delete_started"))
}

func redirectAdminUsers(w http.ResponseWriter, r *http.Request, notice string) {
	values := url.Values{}
	if notice != "" {
		values.Set("notice", notice)
	}
	target := "/admin/users"
	if encoded := values.Encode(); encoded != "" {
		target += "?" + encoded
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *Handler) renderAdminUsers(w http.ResponseWriter, r *http.Request, status int, form views.AdminUserInvitationFormData, invitation *views.AdminUserInvitationData, pageError string) {
	h.renderAdminUsersPage(w, r, status, form, invitation, nil, pageError)
}

func (h *Handler) renderAdminUsersWithCredentialReset(w http.ResponseWriter, r *http.Request, status int, reset *views.AdminUserCredentialResetData, pageError string) {
	h.renderAdminUsersPage(w, r, status, views.AdminUserInvitationFormData{}, nil, reset, pageError)
}

func (h *Handler) renderAdminUsersPage(w http.ResponseWriter, r *http.Request, status int, form views.AdminUserInvitationFormData, invitation *views.AdminUserInvitationData, reset *views.AdminUserCredentialResetData, pageError string) {
	ctx := r.Context()
	currentUser := auth.GetCurrentUser(ctx)
	currentSession := auth.GetCurrentSession(ctx)
	if currentUser == nil || currentSession == nil || h.auth == nil {
		http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	users, err := h.auth.ListAdministratorUsersForSession(ctx, currentUser.ID, currentSession.ID)
	if errors.Is(err, auth.ErrAdministratorRequired) {
		http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	if err != nil {
		log.Printf("load administrator users: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_load_administrator_users"), http.StatusInternalServerError)
		return
	}
	policy, err := h.auth.InstanceSecurityPolicy(ctx)
	if err != nil {
		log.Printf("load administrator user MFA policy: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_load_administrator_users"), http.StatusInternalServerError)
		return
	}
	data := adminUsersViewData(users, currentUser.ID, policy.MFA)
	data.InvitationForm = form
	data.Invitation = invitation
	data.CredentialReset = reset
	data.Error = pageError
	data.Notice = strings.TrimSpace(r.URL.Query().Get("notice"))
	if queryError := strings.TrimSpace(r.URL.Query().Get("error")); data.Error == "" && queryError != "" {
		data.Error = queryError
	}
	// A continuation only opens a confirmation; issuance still requires its POST.
	continuationKey := "reset_user"
	if r.URL.Query().Get("change_user") != "" || strings.HasSuffix(r.URL.Path, "/require-password-change") {
		continuationKey = "change_user"
	}
	resetUserID := r.URL.Query().Get(continuationKey)
	if r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/credential-reset") || strings.HasSuffix(r.URL.Path, "/require-password-change")) && pageError != "" {
		resetUserID = r.PathValue("userID")
	}
	resetReturnTo := adminSecurityVerificationReturnTo("/admin/users?" + url.Values{continuationKey: {resetUserID}}.Encode())
	if resetReturnTo == "" {
		resetUserID = ""
	}
	returnTo := "/admin/users"
	for _, user := range data.Users {
		if user.ID == resetUserID && user.CredentialResetPath != "" && (continuationKey == "reset_user" || user.HasPassword) {
			returnTo = resetReturnTo
			break
		}
	}
	verification := views.AdminSecurityVerificationData{}
	if h.auth.IsEnabled() {
		access, accessErr := h.auth.GetSecuritySettingsAccess(ctx, auth.GetSessionToken(r))
		switch {
		case accessErr == nil:
			data.StepUpRequired = !access.StepUpFresh
			if data.StepUpRequired {
				verification = adminSecurityVerificationViewData(
					ctx, access, returnTo,
					data.Error != "" || r.URL.Query().Get("verification_required") == "1" || returnTo != "/admin/users",
				)
			}
		case errors.Is(accessErr, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		default:
			log.Printf("load administrator invitation verification state: %v", accessErr)
			http.Error(w, i18n.T(r.Context(), "server.error.failed_to_verify_administrator_session"), http.StatusInternalServerError)
			return
		}
	}
	data.InvitationCSRFToken = auth.CSRFToken(ctx, http.MethodPost, adminUserInvitationPath)
	for index := range data.Users {
		if data.Users[index].MFAPolicyPath != "" {
			data.Users[index].MFAPolicyCSRFToken = auth.CSRFToken(ctx, http.MethodPost, data.Users[index].MFAPolicyPath)
		}
		if data.Users[index].StatusPath != "" {
			data.Users[index].StatusCSRFToken = auth.CSRFToken(ctx, http.MethodPost, data.Users[index].StatusPath)
		}
		if data.Users[index].CredentialResetPath != "" {
			data.Users[index].CredentialResetCSRFToken = auth.CSRFToken(ctx, http.MethodPost, data.Users[index].CredentialResetPath)
			data.Users[index].CredentialResetOpen = continuationKey == "reset_user" && !data.StepUpRequired && reset == nil && data.Users[index].ID == resetUserID
			if data.Users[index].HasPassword {
				data.Users[index].RequiredChangePath = "/admin/users/" + url.PathEscape(data.Users[index].ID) + "/require-password-change"
				data.Users[index].RequiredChangeCSRFToken = auth.CSRFToken(ctx, http.MethodPost, data.Users[index].RequiredChangePath)
				data.Users[index].RequiredChangeOpen = continuationKey == "change_user" && !data.StepUpRequired && data.Users[index].ID == resetUserID
			}
		}
		if data.Users[index].DeletionPath != "" {
			data.Users[index].DeletionCSRFToken = auth.CSRFToken(ctx, http.MethodPost, data.Users[index].DeletionPath)
		}
		if data.Users[index].InvitationRevokePath != "" {
			data.Users[index].InvitationRevokeCSRFToken = auth.CSRFToken(ctx, http.MethodPost, data.Users[index].InvitationRevokePath)
		}
		if data.Users[index].InvitationRotatePath != "" {
			data.Users[index].InvitationRotateCSRFToken = auth.CSRFToken(ctx, http.MethodPost, data.Users[index].InvitationRotatePath)
		}
	}
	uiSettings := h.db.GetUISettings(ctx, currentUser.ID)
	var output bytes.Buffer
	if r.Header.Get("HX-Request") == "true" {
		err = views.AdminPartial(data, models.AvatarStatus{}, models.ContactAdminStatus{}, models.LabelAdminStatus{}, models.MailSecurityAdminData{}, models.MailOperationsAdminStatus{}, "users", "", verification).Render(ctx, &output)
	} else {
		err = views.ManagementAdminLayout(uiSettings, data, models.AvatarStatus{}, models.ContactAdminStatus{}, models.LabelAdminStatus{}, models.MailSecurityAdminData{}, models.MailOperationsAdminStatus{}, "users", "", verification).Render(ctx, &output)
	}
	if err != nil {
		log.Printf("render administrator users: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_render_administrator_users"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = output.WriteTo(w)
}
