package handler

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"

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

func adminUserInvitationRevokePath(reference string) string {
	return adminUserInvitationPath + "/" + reference + "/revoke"
}

func adminUserInvitationRotatePath(reference string) string {
	return adminUserInvitationPath + "/" + reference + "/rotate"
}

func adminUsersViewData(users []auth.AdministratorUserSummary, currentUserID string, instancePolicy auth.InstanceMFAPolicy) views.AdminUsersData {
	data := views.AdminUsersData{Users: make([]views.AdminUserData, 0, len(users)), Total: len(users)}
	for _, user := range users {
		view := views.AdminUserData{
			ID:                  user.ID,
			Username:            user.Username,
			Status:              "Disabled",
			Role:                "Webmail user",
			Current:             user.ID == currentUserID,
			InvitationState:     string(user.InvitationState),
			InvitationExpiresAt: user.InvitationExpiresAt,
			MFALabel:            "Optional",
			MFADetail:           "User choice",
			MFARequired:         user.MFARequired,
		}
		if user.InvitationActionReference != "" {
			view.InvitationRevokePath = adminUserInvitationRevokePath(user.InvitationActionReference)
			view.InvitationRotatePath = adminUserInvitationRotatePath(user.InvitationActionReference)
		}
		switch user.Status {
		case auth.UserStatusActive:
			view.Status = "Active"
			data.Active++
			if !view.Current {
				view.StatusPath = adminUserStatusPath(user.ID)
			}
		case auth.UserStatusPending:
			view.Status = "Pending"
			data.Pending++
		case auth.UserStatusDisabled:
			data.Disabled++
			if !view.Current {
				view.StatusPath = adminUserStatusPath(user.ID)
			}
		}
		if user.UserType == auth.UserTypeWebmail && !user.IsAdmin &&
			(user.Status == auth.UserStatusActive || user.Status == auth.UserStatusDisabled) {
			view.CredentialResetPath = adminUserCredentialResetPath(user.ID)
		}
		if user.IsAdmin {
			view.Role = "Management administrator"
			data.Administrators++
		} else if user.UserType == auth.UserTypeManagement {
			view.Role = "Management user"
		}
		if user.UserType == auth.UserTypeWebmail && user.IsAdmin {
			view.Role = "Legacy mixed account"
		}
		switch {
		case user.UserType == auth.UserTypeManagement:
			view.MFALabel = "Required"
			view.MFADetail = "Management policy"
		case user.MFARequired:
			view.MFALabel = "Required"
			view.MFADetail = "Individual policy"
			view.MFAPolicyPath = adminUserMFAPolicyPath(user.ID)
		case instancePolicy.RequiresAllUsers():
			view.MFALabel = "Required"
			view.MFADetail = "Instance policy"
		default:
			view.MFAPolicyPath = adminUserMFAPolicyPath(user.ID)
		}
		data.Users = append(data.Users, view)
	}
	return data
}

func (h *Handler) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	h.renderAdminUsers(w, r, http.StatusOK, views.AdminUserInvitationFormData{}, nil, "")
}

func (h *Handler) handleCreateAdminUserInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	currentUser := auth.GetCurrentUser(ctx)
	currentSession := auth.GetCurrentSession(ctx)
	if currentUser == nil || currentSession == nil || h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, "admin access required", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, "Invalid invitation form data.")
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
			form.FieldErrors = validationErr.Fields
			h.renderAdminUsers(w, r, http.StatusBadRequest, form, nil, "Correct the highlighted invitation details.")
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			h.renderAdminUsers(w, r, http.StatusForbidden, form, nil, "Verify this administrator session before creating an invitation.")
		case errors.Is(err, auth.ErrAdministratorRequired):
			http.Error(w, "admin access required", http.StatusForbidden)
		default:
			log.Printf("create administrator user invitation: %v", err)
			h.renderAdminUsers(w, r, http.StatusInternalServerError, form, nil, "Unable to create the invitation right now.")
		}
		return
	}
	result := &views.AdminUserInvitationData{
		Name: invitation.Name, Username: invitation.User.Username,
		RedemptionURL: strings.TrimRight(h.auth.Config().BaseURL, "/") + enrollmentRedemptionPath,
		Token:         invitation.Token.Token, ExpiresAt: invitation.Token.ExpiresAt,
	}
	h.renderAdminUsers(w, r, http.StatusCreated, views.AdminUserInvitationFormData{}, result, "")
}

func (h *Handler) handleRevokeAdminUserInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	currentUser := auth.GetCurrentUser(ctx)
	currentSession := auth.GetCurrentSession(ctx)
	if currentUser == nil || currentSession == nil || h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, "admin access required", http.StatusForbidden)
		return
	}
	err := h.auth.RevokeAdministratorUserInvitation(
		ctx, currentUser.ID, currentSession.ID, r.PathValue("reference"),
	)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			h.renderAdminUsers(w, r, http.StatusForbidden, views.AdminUserInvitationFormData{}, nil, "Verify this administrator session before revoking an invitation.")
		case errors.Is(err, auth.ErrAdministratorRequired):
			http.Error(w, "admin access required", http.StatusForbidden)
		case errors.Is(err, auth.ErrAdministratorUserInvitationTargetInvalid),
			errors.Is(err, auth.ErrAdministratorUserInvitationNotActive):
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, "This invitation is no longer active. Refresh the page and try again.")
		default:
			log.Printf("revoke administrator user invitation: %v", err)
			h.renderAdminUsers(w, r, http.StatusInternalServerError, views.AdminUserInvitationFormData{}, nil, "Unable to revoke the invitation right now.")
		}
		return
	}
	redirectAdminUsers(w, r, "Invitation revoked.")
}

func (h *Handler) handleRotateAdminUserInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	currentUser := auth.GetCurrentUser(ctx)
	currentSession := auth.GetCurrentSession(ctx)
	if currentUser == nil || currentSession == nil || h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, "admin access required", http.StatusForbidden)
		return
	}
	invitation, err := h.auth.RotateAdministratorUserInvitation(ctx, auth.RotateAdministratorUserInvitationOptions{
		ActorUserID: currentUser.ID, ActorSessionID: currentSession.ID,
		ActionReference: r.PathValue("reference"),
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			h.renderAdminUsers(w, r, http.StatusForbidden, views.AdminUserInvitationFormData{}, nil, "Verify this administrator session before issuing a replacement invitation.")
		case errors.Is(err, auth.ErrAdministratorRequired):
			http.Error(w, "admin access required", http.StatusForbidden)
		case errors.Is(err, auth.ErrAdministratorUserInvitationTargetInvalid):
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, "This invitation target is no longer available. Refresh the page and try again.")
		default:
			log.Printf("rotate administrator user invitation: %v", err)
			h.renderAdminUsers(w, r, http.StatusInternalServerError, views.AdminUserInvitationFormData{}, nil, "Unable to issue a replacement invitation right now.")
		}
		return
	}
	result := &views.AdminUserInvitationData{
		Name: invitation.Name, Username: invitation.User.Username,
		RedemptionURL: strings.TrimRight(h.auth.Config().BaseURL, "/") + enrollmentRedemptionPath,
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
		http.Error(w, "admin access required", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, "Invalid MFA policy request.")
		return
	}
	var required bool
	switch r.PostFormValue("required") {
	case "true":
		required = true
	case "false":
	default:
		h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, "Invalid MFA policy request.")
		return
	}
	result, err := h.auth.SetAdministratorUserMFAPolicy(ctx, auth.SetAdministratorUserMFAPolicyOptions{
		ActorUserID: currentUser.ID, ActorSessionID: currentSession.ID,
		TargetUserID: r.PathValue("userID"), Required: required,
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			h.renderAdminUsers(w, r, http.StatusForbidden, views.AdminUserInvitationFormData{}, nil, "Verify this administrator session before changing a user's MFA policy.")
		case errors.Is(err, auth.ErrAdministratorRequired):
			http.Error(w, "admin access required", http.StatusForbidden)
		case errors.Is(err, auth.ErrAdministratorUserMFATargetInvalid):
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, "This webmail user is no longer available. Refresh the page and try again.")
		default:
			log.Printf("set administrator user MFA policy: %v", err)
			h.renderAdminUsers(w, r, http.StatusInternalServerError, views.AdminUserInvitationFormData{}, nil, "Unable to change the user's MFA policy right now.")
		}
		return
	}
	if !result.Changed {
		redirectAdminUsers(w, r, "The user's MFA policy was already up to date.")
		return
	}
	if result.Required {
		redirectAdminUsers(w, r, "MFA is now required for this user's next sign-in. Existing sessions and factors were left unchanged.")
		return
	}
	redirectAdminUsers(w, r, "The individual MFA requirement was cleared. Existing sessions and factors were left unchanged.")
}

func (h *Handler) handleSetAdminUserStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	currentUser := auth.GetCurrentUser(ctx)
	currentSession := auth.GetCurrentSession(ctx)
	if currentUser == nil || currentSession == nil || h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, "admin access required", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, "Invalid user status request.")
		return
	}
	var status auth.UserStatus
	switch r.PostFormValue("status") {
	case string(auth.UserStatusActive):
		status = auth.UserStatusActive
	case string(auth.UserStatusDisabled):
		status = auth.UserStatusDisabled
	default:
		h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, "Invalid user status request.")
		return
	}
	result, err := h.auth.SetAdministratorUserStatus(ctx, auth.SetAdministratorUserStatusOptions{
		ActorUserID: currentUser.ID, ActorSessionID: currentSession.ID,
		TargetUserID: r.PathValue("userID"), Status: status,
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			h.renderAdminUsers(w, r, http.StatusForbidden, views.AdminUserInvitationFormData{}, nil, "Verify this administrator session before changing a user's access.")
		case errors.Is(err, auth.ErrAdministratorRequired):
			http.Error(w, "admin access required", http.StatusForbidden)
		case errors.Is(err, auth.ErrLastActiveAdmin):
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, "The last active management administrator cannot be disabled.")
		case errors.Is(err, auth.ErrInstanceMFAEnrollmentNeeded):
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, "This user needs an enrolled authenticator before they can be enabled under the current MFA policy.")
		case errors.Is(err, auth.ErrAdministratorUserStatusInvalid),
			errors.Is(err, auth.ErrAdministratorUserStatusTargetInvalid):
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, "This user is no longer available for that action. Refresh the page and try again.")
		default:
			log.Printf("set administrator user status: %v", err)
			h.renderAdminUsers(w, r, http.StatusInternalServerError, views.AdminUserInvitationFormData{}, nil, "Unable to change the user's access right now.")
		}
		return
	}
	if !result.Changed {
		redirectAdminUsers(w, r, "The user's access was already up to date.")
		return
	}
	if result.Status == auth.UserStatusDisabled {
		notice := "User disabled. Their data and credentials were preserved."
		if result.RevokedSessions == 1 {
			notice = "User disabled and 1 active session revoked. Their data and credentials were preserved."
		} else if result.RevokedSessions > 1 {
			notice = fmt.Sprintf("User disabled and %d active sessions revoked. Their data and credentials were preserved.", result.RevokedSessions)
		}
		redirectAdminUsers(w, r, notice)
		return
	}
	redirectAdminUsers(w, r, "User enabled. Existing credentials were preserved, and previously revoked sessions remain signed out.")
}

func (h *Handler) handleIssueAdminUserCredentialReset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	currentUser := auth.GetCurrentUser(ctx)
	currentSession := auth.GetCurrentSession(ctx)
	if currentUser == nil || currentSession == nil || h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, "admin access required", http.StatusForbidden)
		return
	}
	targetUser, err := h.auth.GetUserByID(ctx, r.PathValue("userID"))
	if err != nil {
		log.Printf("load administrator credential-reset target: %v", err)
		h.renderAdminUsers(w, r, http.StatusInternalServerError, views.AdminUserInvitationFormData{}, nil, "Unable to generate a reset token right now.")
		return
	}
	if targetUser == nil || targetUser.UserType != auth.UserTypeWebmail || targetUser.IsAdmin ||
		(targetUser.Status != auth.UserStatusActive && targetUser.Status != auth.UserStatusDisabled) {
		h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, "This webmail user is no longer available for password reset. Refresh the page and try again.")
		return
	}
	token, err := h.auth.IssueEnrollmentToken(ctx, auth.IssueEnrollmentTokenOptions{
		UserID: targetUser.ID, CreatedBy: currentUser.ID, ActorSessionID: currentSession.ID,
		Purpose: auth.EnrollmentTokenPurposeCredentialReset,
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			h.renderAdminUsers(w, r, http.StatusForbidden, views.AdminUserInvitationFormData{}, nil, "Verify this administrator session before generating a password-reset token.")
		case errors.Is(err, auth.ErrAdministratorRequired):
			http.Error(w, "admin access required", http.StatusForbidden)
		case errors.Is(err, auth.ErrEnrollmentTokenTargetInvalid):
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, "This webmail user is no longer available for password reset. Refresh the page and try again.")
		default:
			log.Printf("issue administrator user credential reset: %v", err)
			h.renderAdminUsers(w, r, http.StatusInternalServerError, views.AdminUserInvitationFormData{}, nil, "Unable to generate a reset token right now.")
		}
		return
	}
	result := &views.AdminUserCredentialResetData{
		Username:      targetUser.Username,
		RedemptionURL: strings.TrimRight(h.auth.Config().BaseURL, "/") + enrollmentRedemptionPath,
		Token:         token.Token, ExpiresAt: token.ExpiresAt,
	}
	h.renderAdminUsersWithCredentialReset(w, r, http.StatusCreated, result, "")
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
		http.Error(w, "admin access required", http.StatusForbidden)
		return
	}
	users, err := h.auth.ListAdministratorUsersForSession(ctx, currentUser.ID, currentSession.ID)
	if errors.Is(err, auth.ErrAdministratorRequired) {
		http.Error(w, "admin access required", http.StatusForbidden)
		return
	}
	if err != nil {
		log.Printf("load administrator users: %v", err)
		http.Error(w, "failed to load administrator users", http.StatusInternalServerError)
		return
	}
	policy, err := h.auth.InstanceSecurityPolicy(ctx)
	if err != nil {
		log.Printf("load administrator user MFA policy: %v", err)
		http.Error(w, "failed to load administrator users", http.StatusInternalServerError)
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
	data.StepUpRequired = true
	if h.auth.IsEnabled() {
		err := h.auth.RequireRecentSecurityStepUp(ctx, auth.GetSessionToken(r))
		switch {
		case err == nil:
			data.StepUpRequired = false
		case errors.Is(err, auth.ErrRecentStepUpRequired):
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		default:
			log.Printf("load administrator invitation verification state: %v", err)
			http.Error(w, "failed to verify administrator session", http.StatusInternalServerError)
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
		err = views.AdminPartial(data, models.AvatarStatus{}, models.ContactAdminStatus{}, models.LabelAdminStatus{}, models.MailSecurityAdminData{}, models.MailOperationsAdminStatus{}, "users", "").Render(ctx, &output)
	} else {
		err = views.ManagementAdminLayout(uiSettings, data, models.AvatarStatus{}, models.ContactAdminStatus{}, models.LabelAdminStatus{}, models.MailSecurityAdminData{}, models.MailOperationsAdminStatus{}, "users", "").Render(ctx, &output)
	}
	if err != nil {
		log.Printf("render administrator users: %v", err)
		http.Error(w, "failed to render administrator users", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = output.WriteTo(w)
}
