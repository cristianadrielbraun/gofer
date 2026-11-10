package handler

import (
	"context"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const (
	adminUsersPath          = "/admin/users"
	adminUserInvitationPath = "/admin/users/invitations"
)

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

func adminUserRequiredChangePath(userID string) string {
	return "/admin/users/" + url.PathEscape(userID) + "/require-password-change"
}

// adminUserActionDialogRoutes are the addresses of the Users page's actions.
// Each shows its action's dialog (GET) and is where that dialog's form posts.
var adminUserActionDialogRoutes = []struct {
	pattern string
	action  views.AdminUserAction
}{
	{adminUserInvitationPath + "/{reference}/revoke", views.AdminUserActionInvitationRevoke},
	{adminUserInvitationPath + "/{reference}/rotate", views.AdminUserActionInvitationRotate},
	{"/admin/users/{userID}/mfa-policy", views.AdminUserActionMFAPolicy},
	{"/admin/users/{userID}/status", views.AdminUserActionStatus},
	{"/admin/users/{userID}/credential-reset", views.AdminUserActionCredentialReset},
	{"/admin/users/{userID}/require-password-change", views.AdminUserActionRequiredChange},
	{"/admin/users/{userID}/delete", views.AdminUserActionDeletion},
}

// adminUserActionAddress reports whether path is one of those addresses, with
// a plausible user ID or invitation reference in it.
func adminUserActionAddress(path string) bool {
	segments := strings.Split(path, "/")
	for _, route := range adminUserActionDialogRoutes {
		parts := strings.Split(route.pattern, "/")
		if len(parts) != len(segments) {
			continue
		}
		matches := true
		for index, part := range parts {
			switch part {
			case "{userID}":
				matches = adminUserActionTargetID(segments[index])
			case "{reference}":
				matches = adminUserInvitationReference(segments[index])
			default:
				matches = part == segments[index]
			}
			if !matches {
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

// adminUserActionTargetID reports whether value can be the user ID in an
// action address: letters, digits, hyphens and underscores.
func adminUserActionTargetID(value string) bool {
	return value != "" && len(value) <= 128 && strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) < 0
}

// adminUserInvitationReference reports whether value has the shape of an
// invitation's action reference: 43 characters of unpadded base64url.
func adminUserInvitationReference(value string) bool {
	return len(value) == 43 && adminUserActionTargetID(value)
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
			if user.HasPassword {
				view.RequiredChangePath = adminUserRequiredChangePath(user.ID)
			}
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
		http.Redirect(w, r, views.AdminAvatarsPath, http.StatusFound)
		return
	}
	if destination := adminUsersOldDialogAddress(r.URL.Query()); destination != "" {
		http.Redirect(w, r, destination, http.StatusFound)
		return
	}
	h.renderAdminUsers(w, r, http.StatusOK, views.AdminUserInvitationFormData{}, nil, "")
}

// adminUsersOldDialogAddress is the new address for /admin/users?reset_user=
// and ?change_user=, which opened the reset and password-change dialogs
// before each action had an address of its own.
func adminUsersOldDialogAddress(query url.Values) string {
	for _, old := range []struct{ key, action string }{
		{"reset_user", "credential-reset"},
		{"change_user", "require-password-change"},
	} {
		if values := query[old.key]; len(values) == 1 && adminUserActionTargetID(values[0]) {
			return "/admin/users/" + values[0] + "/" + old.action
		}
	}
	return ""
}

// handleAdminUserActionDialog answers an action's address with that action's
// dialog for the person the address names. htmx, asking from the person's
// menu, gets the dialog alone; any other request gets the Users page with
// the dialog open.
func (h *Handler) handleAdminUserActionDialog(action views.AdminUserAction) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.auth != nil && !h.auth.IsEnabled() {
			http.Redirect(w, r, views.AdminAvatarsPath, http.StatusFound)
			return
		}
		state, ok := h.loadAdminUsers(w, r)
		if !ok {
			return
		}
		user, found := adminUserAt(state.data.Users, action, r.URL.EscapedPath())
		if !found {
			h.setAdminFlashError(w, adminUserActionUnavailable(r.Context(), action))
			redirectAdmin(w, r, adminUsersPath)
			return
		}
		dialog := views.AdminUserDialog{
			Action: action, User: user,
			CSRFToken: auth.CSRFToken(r.Context(), http.MethodPost, user.Path(action)),
		}
		// A session that must verify again is asked when the change is sent
		// (admin.js). A page drawn while the session was fresh has nothing to
		// ask with, so the dialog brings the verification along.
		if r.Header.Get("HX-Request") == "true" {
			var verification views.AdminSecurityVerificationData
			if state.data.StepUpRequired && r.Header.Get(views.AdminVerificationOnPageHeader) == "" {
				verification = adminSecurityVerificationViewData(r.Context(), state.access, user.Path(action), false)
			}
			h.writeAdminFragment(w, r, views.AdminUserActionDialog(dialog, verification))
			return
		}
		state.data.Dialog = &dialog
		h.writeAdminUsersPage(w, r, http.StatusOK, state, user.Path(action), false, "")
	}
}

// adminUserAt finds the person whose address for action is path.
func adminUserAt(users []views.AdminUserData, action views.AdminUserAction, path string) (views.AdminUserData, bool) {
	for _, user := range users {
		if address := user.Path(action); address != "" && address == path {
			return user, true
		}
	}
	return views.AdminUserData{}, false
}

// adminUserActionUnavailable explains an action address whose person, or
// the action itself, is gone, for instance after another change.
func adminUserActionUnavailable(ctx context.Context, action views.AdminUserAction) string {
	switch action {
	case views.AdminUserActionInvitationRevoke:
		return i18n.T(ctx, "auth.admin.users.invitation_inactive")
	case views.AdminUserActionInvitationRotate:
		return i18n.T(ctx, "auth.admin.users.invitation_target_gone")
	case views.AdminUserActionCredentialReset:
		return i18n.T(ctx, "auth.admin.users.reset_user_gone")
	default:
		return i18n.T(ctx, "auth.admin.users.user_unavailable")
	}
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
	h.redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.invitation_revoked"))
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
		h.redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.mfa_unchanged"))
		return
	}
	if result.Required {
		h.redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.mfa_required"))
		return
	}
	h.redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.mfa_cleared"))
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
		h.redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.access_unchanged"))
		return
	}
	if result.Status == auth.UserStatusDisabled {
		notice := i18n.T(r.Context(), "auth.admin.users.disabled")
		if result.RevokedSessions > 0 {
			notice = i18n.N(r.Context(), "auth.admin.users.disabled_sessions", int(result.RevokedSessions), "count", i18n.Number(r.Context(), int(result.RevokedSessions)))
		}
		h.redirectAdminUsers(w, r, notice)
		return
	}
	h.redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.enabled"))
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
			h.redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.delete_pending"))
			return
		}
		if err != nil {
			http.Error(w, i18n.T(r.Context(), "auth.admin.users.delete_stopped"), http.StatusServiceUnavailable)
			return
		}
		h.redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.delete_started"))
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
		h.redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.delete_resumed"))
		return
	}
	h.redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.delete_started"))
}

// redirectAdminUsers returns to the Users page, which shows notice as a
// toast.
func (h *Handler) redirectAdminUsers(w http.ResponseWriter, r *http.Request, notice string) {
	h.setAdminFlashSuccess(w, notice)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, adminUsersPath, http.StatusSeeOther)
}

// redirectAdmin sends the browser to location: htmx goes there with a whole
// page through HX-Redirect, other requests with a See Other.
func redirectAdmin(w http.ResponseWriter, r *http.Request, location string) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", location)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

func (h *Handler) renderAdminUsers(w http.ResponseWriter, r *http.Request, status int, form views.AdminUserInvitationFormData, invitation *views.AdminUserInvitationData, pageError string) {
	h.renderAdminUsersPage(w, r, status, form, invitation, nil, pageError)
}

func (h *Handler) renderAdminUsersWithCredentialReset(w http.ResponseWriter, r *http.Request, status int, reset *views.AdminUserCredentialResetData, pageError string) {
	h.renderAdminUsersPage(w, r, status, views.AdminUserInvitationFormData{}, nil, reset, pageError)
}

func (h *Handler) renderAdminUsersPage(w http.ResponseWriter, r *http.Request, status int, form views.AdminUserInvitationFormData, invitation *views.AdminUserInvitationData, reset *views.AdminUserCredentialResetData, pageError string) {
	state, ok := h.loadAdminUsers(w, r)
	if !ok {
		return
	}
	state.data.InvitationForm = form
	state.data.Invitation = invitation
	state.data.CredentialReset = reset
	// A refused change comes back to its own dialog once the session verifies.
	returnTo := adminUsersPath
	if r.Method == http.MethodPost && pageError != "" {
		if address := adminSecurityVerificationReturnTo(r.URL.EscapedPath()); address != "" {
			returnTo = address
		}
	}
	openVerification := pageError != "" || r.URL.Query().Get("verification_required") == "1"
	h.writeAdminUsersPage(w, r, status, state, returnTo, openVerification, pageError)
}

// adminUsersState is what every answer about the Users page starts from:
// the people it lists, and the session's verification state.
type adminUsersState struct {
	data   views.AdminUsersData
	access *auth.SecuritySettingsAccess
}

// loadAdminUsers reads the Users page's people and whether the session must
// verify again. When it fails it has answered already, and ok is false.
func (h *Handler) loadAdminUsers(w http.ResponseWriter, r *http.Request) (state adminUsersState, ok bool) {
	ctx := r.Context()
	currentUser := auth.GetCurrentUser(ctx)
	currentSession := auth.GetCurrentSession(ctx)
	if currentUser == nil || currentSession == nil || h.auth == nil {
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return state, false
	}
	users, err := h.auth.ListAdministratorUsersForSession(ctx, currentUser.ID, currentSession.ID)
	if errors.Is(err, auth.ErrAdministratorRequired) {
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return state, false
	}
	if err != nil {
		log.Printf("load administrator users: %v", err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_load_administrator_users"), http.StatusInternalServerError)
		return state, false
	}
	policy, err := h.auth.InstanceSecurityPolicy(ctx)
	if err != nil {
		log.Printf("load administrator user MFA policy: %v", err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_load_administrator_users"), http.StatusInternalServerError)
		return state, false
	}
	state.data = adminUsersViewData(users, currentUser.ID, policy.MFA)
	if !h.auth.IsEnabled() {
		return state, true
	}
	access, err := h.auth.GetSecuritySettingsAccess(ctx, auth.GetSessionToken(r))
	switch {
	case err == nil:
		state.access = access
		state.data.StepUpRequired = !access.StepUpFresh
	case errors.Is(err, auth.ErrSecuritySessionInvalid):
		auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
		redirectAdmin(w, r, "/admin/login")
		return state, false
	default:
		log.Printf("load administrator invitation verification state: %v", err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_verify_administrator_session"), http.StatusInternalServerError)
		return state, false
	}
	return state, true
}

// writeAdminUsersPage renders the Users page from state. returnTo is where
// verification leads back to, and openVerification opens it with the page.
func (h *Handler) writeAdminUsersPage(w http.ResponseWriter, r *http.Request, status int, state adminUsersState, returnTo string, openVerification bool, pageError string) {
	ctx := r.Context()
	verification := views.AdminSecurityVerificationData{}
	if state.data.StepUpRequired {
		verification = adminSecurityVerificationViewData(ctx, state.access, returnTo, openVerification)
	}
	state.data.InvitationCSRFToken = auth.CSRFToken(ctx, http.MethodPost, adminUserInvitationPath)
	h.renderAdminPage(w, r, status, views.AdminShellPage{
		Section: views.AdminSectionUsers,
		Content: views.AdminUsersPage(state.data, verification),
		Toasts:  adminErrorToasts(pageError),
	})
}
