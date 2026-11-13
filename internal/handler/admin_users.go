package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/admin"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
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

func adminUserSignOutPath(userID string) string {
	return "/admin/users/" + url.PathEscape(userID) + "/sign-out"
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
	{"/admin/users/{userID}/sign-out", views.AdminUserActionSignOut},
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
			LastSignInAt:             user.LastLoginAt,
			Status:                   "disabled",
			Role:                     "webmail_user",
			Current:                  user.ID == currentUserID,
			InvitationState:          string(user.InvitationState),
			InvitationExpiresAt:      user.InvitationExpiresAt,
			MFALabel:                 "optional",
			MFADetail:                "user_choice",
			MFARequired:              user.MFARequired,
			MFAPasskeyCounts:         user.MFAPasskeyCounts,
			PasswordResetRequestedAt: user.PasswordResetRequestedAt,
			DeletionPending:          user.DeletionPending,
		}
		actions := admin.ActionsFor(currentUserID, user, instancePolicy)
		if actions.ManageInvitation {
			view.InvitationRevokePath = adminUserInvitationRevokePath(user.InvitationActionReference)
			view.InvitationRotatePath = adminUserInvitationRotatePath(user.InvitationActionReference)
		}
		switch user.Status {
		case auth.UserStatusActive:
			view.Status = "active"
			data.Active++
		case auth.UserStatusPending:
			view.Status = "pending"
			data.Pending++
		case auth.UserStatusDisabled:
			data.Disabled++
		}
		if actions.SetStatus {
			view.StatusPath = adminUserStatusPath(user.ID)
		}
		if actions.IssueCredentialReset {
			view.CredentialResetPath = adminUserCredentialResetPath(user.ID)
		}
		if actions.RequirePasswordChange {
			view.RequiredChangePath = adminUserRequiredChangePath(user.ID)
		}
		if actions.Delete {
			view.DeletionPath = adminUserDeletionPath(user.ID)
		}
		if actions.SetMFARequirement {
			view.MFAPolicyPath = adminUserMFAPolicyPath(user.ID)
		}
		if actions.SignOut {
			view.SignOutPath = adminUserSignOutPath(user.ID)
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
			if user.MFAPasskeyCounts {
				view.MFADetail = "individual_policy_passkey"
			}
		case instancePolicy.RequiresAllUsers():
			view.MFALabel = "required"
			view.MFADetail = "instance_policy"
		}
		view.MFASetupPending = view.MFALabel == "required" && !user.HasSecondFactor &&
			user.Status == auth.UserStatusActive && !user.DeletionPending
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
	h.renderAdminUsersPage(w, r, http.StatusOK, adminUsersQueryFrom(r.URL.Query()), views.AdminUserInvitationFormData{}, nil, nil, "")
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
		// The page behind a dialog opened at its address is the list it came
		// from.
		listQuery := views.AdminUsersQuery{}
		if list, err := url.Parse(adminUsersListFrom(r.URL.Query().Get("list"))); err == nil {
			listQuery = adminUsersQueryFrom(list.Query())
		}
		state, ok := h.loadAdminUsers(w, r, listQuery)
		if !ok {
			return
		}
		user, found := h.adminUserFor(r, action, state.policy)
		if !found {
			h.setAdminFlashError(w, adminUserActionUnavailable(r.Context(), action))
			redirectAdmin(w, r, adminUsersPath)
			return
		}
		dialog := views.AdminUserDialog{
			Action: action, User: user,
			CSRFToken:      auth.CSRFToken(r.Context(), http.MethodPost, user.Path(action)),
			ProviderSignIn: h.auth.HasGoogleLogin() || h.auth.HasMicrosoftLogin() || h.auth.HasOIDCLogin(),
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
		if adminUsersListFrom(r.URL.Query().Get("list")) == views.AdminAdministratorsPath {
			h.renderAdminAdministrators(w, r, adminAdministratorsExtras{dialog: &dialog, returnTo: user.Path(action)})
			return
		}
		// Opened from a person's page, the page is behind it.
		if userID, tab, list, ok := adminUserPageFrom(r.URL.Query().Get("list")); ok {
			h.renderAdminUserPage(w, r, userID, tab, list, adminUserPageExtras{dialog: &dialog, returnTo: user.Path(action)})
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
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.invitation_form_invalid"))
		return
	}
	form := views.AdminUserInvitationFormData{
		Name: r.PostFormValue("name"), Username: r.PostFormValue("username"),
	}
	var invitation *auth.AdministratorUserInvitation
	var err error
	switch r.PostFormValue("role") {
	case "", "user":
		invitation, err = h.adminService().InviteUser(r.Context(), actor, form.Name, form.Username)
	case "administrator":
		form.Administrator = true
		invitation, err = h.adminService().InviteAdministrator(r.Context(), actor, form.Name, form.Username)
	default:
		h.renderAdminUsers(w, r, http.StatusBadRequest, form, nil, i18n.T(r.Context(), "auth.admin.users.invitation_form_invalid"))
		return
	}
	if err != nil {
		var validationErr *auth.AdministratorUserInvitationValidationError
		if errors.As(err, &validationErr) {
			form.FieldErrors = authFields(r.Context(), validationErr.Fields)
			if form.Administrator {
				h.renderAdminAdministrators(w, r, adminAdministratorsExtras{status: http.StatusBadRequest, form: form})
				return
			}
			h.renderAdminUsers(w, r, http.StatusBadRequest, form, nil, i18n.T(r.Context(), "auth.admin.users.invitation_fix"))
			return
		}
		if form.Administrator && errors.Is(err, auth.ErrRecentStepUpRequired) {
			h.renderAdminAdministrators(w, r, adminAdministratorsExtras{status: http.StatusForbidden, form: form, pageError: i18n.T(r.Context(), adminUserInviteChange.verify)})
			return
		}
		h.adminUserChangeFailed(w, r, err, adminUserInviteChange, form)
		return
	}
	// An administrator joins the Administrators page, where the invitation
	// shows.
	if form.Administrator {
		h.renderAdminAdministrators(w, r, adminAdministratorsExtras{status: http.StatusCreated, result: h.adminInvitationResult(invitation, false)})
		return
	}
	h.renderAdminUsers(w, r, http.StatusCreated, views.AdminUserInvitationFormData{}, h.adminInvitationResult(invitation, false), "")
}

func (h *Handler) handleRevokeAdminUserInvitation(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	if err := h.adminService().RevokeInvitation(r.Context(), actor, r.PathValue("reference")); err != nil {
		h.adminUserChangeFailed(w, r, err, adminUserRevokeChange, views.AdminUserInvitationFormData{})
		return
	}
	h.redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.invitation_revoked"))
}

func (h *Handler) handleRotateAdminUserInvitation(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	invitation, err := h.adminService().RotateInvitation(r.Context(), actor, r.PathValue("reference"))
	if err != nil {
		h.adminUserChangeFailed(w, r, err, adminUserRotateChange, views.AdminUserInvitationFormData{})
		return
	}
	result := h.adminInvitationResult(invitation, true)
	if adminUsersListFrom(r.PostFormValue("list")) == views.AdminAdministratorsPath {
		h.renderAdminAdministrators(w, r, adminAdministratorsExtras{status: http.StatusCreated, result: result})
		return
	}
	// Made from the person's page, the new invitation shows there.
	if userID, tab, list, ok := adminUserPageFrom(r.PostFormValue("list")); ok {
		h.renderAdminUserPage(w, r, userID, tab, list, adminUserPageExtras{status: http.StatusCreated, invitation: result})
		return
	}
	h.renderAdminUsers(w, r, http.StatusCreated, views.AdminUserInvitationFormData{}, result, "")
}

// adminInvitationResult shows a new invitation once, with where to use it.
func (h *Handler) adminInvitationResult(invitation *auth.AdministratorUserInvitation, rotated bool) *views.AdminUserInvitationData {
	path := invitationEnrollmentPath
	administrator := invitation.User.UserType == auth.UserTypeManagement
	if administrator {
		path = auth.AdminEnrollmentPath
	}
	return &views.AdminUserInvitationData{
		Administrator: administrator, Name: invitation.Name, Username: invitation.User.Username,
		RedemptionURL: strings.TrimRight(h.auth.Config().BaseURL, "/") + path,
		Token:         invitation.Token.Token, ExpiresAt: invitation.Token.ExpiresAt,
		Rotated: rotated,
	}
}

func (h *Handler) handleSetAdminUserMFAPolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.adminActor(w, r)
	if !ok {
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
	// passkey=true lets a passkey satisfy the requirement; without it the
	// requirement takes the authenticator app (TOTP), as it always did.
	var passkeyCounts bool
	switch r.PostFormValue("passkey") {
	case "true":
		passkeyCounts = true
	case "false", "":
	default:
		h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.mfa_request_invalid"))
		return
	}
	result, err := h.adminService().SetMFARequirement(r.Context(), actor, r.PathValue("userID"), required, passkeyCounts)
	if err != nil {
		h.adminUserChangeFailed(w, r, err, adminUserMFAChange, views.AdminUserInvitationFormData{})
		return
	}
	if !result.Changed {
		h.redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.mfa_unchanged"))
		return
	}
	if result.Required && result.PasskeyCounts {
		h.redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.mfa_required_passkey"))
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
	actor, ok := h.adminActor(w, r)
	if !ok {
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
	result, err := h.adminService().SetStatus(ctx, actor, r.PathValue("userID"), status)
	var hookErr *admin.HookError
	if errors.As(err, &hookErr) {
		log.Printf("refresh user status services: %v", hookErr.Err)
		h.renderAdminUsers(w, r, http.StatusServiceUnavailable, views.AdminUserInvitationFormData{}, nil, i18n.T(r.Context(), "auth.admin.users.access_refresh_failed"))
		return
	}
	if err != nil {
		h.adminUserChangeFailed(w, r, err, adminUserStatusChange, views.AdminUserInvitationFormData{})
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

func (h *Handler) handleSignOutAdminUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	result, err := h.adminService().SignOut(r.Context(), actor, r.PathValue("userID"))
	if err != nil {
		h.adminUserChangeFailed(w, r, err, adminUserSignOutChange, views.AdminUserInvitationFormData{})
		return
	}
	if result.RevokedSessions == 0 {
		h.redirectAdminUsers(w, r, i18n.T(r.Context(), "auth.admin.users.not_signed_in"))
		return
	}
	h.redirectAdminUsers(w, r, i18n.N(r.Context(), "auth.admin.users.signed_out", int(result.RevokedSessions), "count", i18n.Number(r.Context(), int(result.RevokedSessions))))
}

func (h *Handler) handleIssueAdminUserCredentialReset(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.adminActor(w, r)
	if !ok {
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
	token, target, err := h.adminService().IssueCredentialReset(r.Context(), actor, r.PathValue("userID"))
	if err != nil {
		h.adminUserChangeFailed(w, r, err, adminUserResetChange, views.AdminUserInvitationFormData{})
		return
	}
	path, administrator := credentialRedemptionPath, target.UserType == auth.UserTypeManagement
	if administrator {
		path = auth.AdminResetPath
	}
	result := &views.AdminUserCredentialResetData{
		Administrator: administrator, Username: target.Username,
		RedemptionURL: strings.TrimRight(h.auth.Config().BaseURL, "/") + path,
		Token:         token.Token, ExpiresAt: token.ExpiresAt,
	}
	// Issued from the person's page, the token shows there.
	if userID, tab, list, ok := adminUserPageFrom(r.PostFormValue("list")); ok {
		h.renderAdminUserPage(w, r, userID, tab, list, adminUserPageExtras{status: http.StatusCreated, credentialReset: result})
		return
	}
	if adminUsersListFrom(r.PostFormValue("list")) == views.AdminAdministratorsPath {
		h.renderAdminAdministrators(w, r, adminAdministratorsExtras{status: http.StatusCreated, credentialReset: result})
		return
	}
	h.renderAdminUsersWithCredentialReset(w, r, http.StatusCreated, result, "")
}

func (h *Handler) handleDeleteAdminUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := h.adminActor(w, r)
	if !ok {
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
	result, err := h.adminService().PrepareDeletion(ctx, actor, r.PathValue("userID"), r.PostFormValue("confirmation"))
	if err != nil {
		h.adminUserChangeFailed(w, r, err, adminUserDeleteChange, views.AdminUserInvitationFormData{})
		return
	}
	if owned != nil {
		owned.h.userIMAP.StopUser(result.TargetUserID)
		err := owned.enqueue(userRemovalJob{owner: result.TargetUserID, actorSession: actor.SessionID})
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
	}(result.TargetUserID, actor.SessionID)

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
	target := adminUsersPath
	if list := adminUsersListFrom(r.PostFormValue("list")); list != "" {
		target = list
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// adminUsersListFrom is the list a change was made from, when value is one
// of the Users page's own addresses, rebuilt from what it understands;
// anything else is "".
func adminUsersListFrom(value string) string {
	parsed, err := url.Parse(value)
	if value == "" || err != nil || parsed.Scheme != "" || parsed.Host != "" {
		return ""
	}
	if parsed.Path == views.AdminAdministratorsPath && parsed.RawQuery == "" {
		return views.AdminAdministratorsPath
	}
	if parsed.Path != adminUsersPath {
		// A person's page, which a change made from it returns to.
		page, _ := adminUserPageAddress(parsed)
		return page
	}
	query := adminUsersQueryFrom(parsed.Query())
	return query.Path(max(query.Page, 1))
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
	h.renderAdminUsersPage(w, r, status, views.AdminUsersQuery{}, form, invitation, nil, pageError)
}

func (h *Handler) renderAdminUsersWithCredentialReset(w http.ResponseWriter, r *http.Request, status int, reset *views.AdminUserCredentialResetData, pageError string) {
	h.renderAdminUsersPage(w, r, status, views.AdminUsersQuery{}, views.AdminUserInvitationFormData{}, nil, reset, pageError)
}

func (h *Handler) renderAdminUsersPage(w http.ResponseWriter, r *http.Request, status int, query views.AdminUsersQuery, form views.AdminUserInvitationFormData, invitation *views.AdminUserInvitationData, reset *views.AdminUserCredentialResetData, pageError string) {
	state, ok := h.loadAdminUsers(w, r, query)
	if !ok {
		return
	}
	// A list changed in place puts its own address in the browser's,
	// without the choices left at their defaults.
	if r.Method == http.MethodGet && r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Push-Url", state.data.Query.Path(max(state.data.Query.Page, 1)))
	}
	state.data.InvitationForm = form
	state.data.Invitation = invitation
	state.data.CredentialReset = reset
	// A refused change comes back to its own dialog once the session verifies.
	returnTo := adminUsersPath
	if r.Method == http.MethodGet && r.URL.Query().Get("verification_required") == "1" {
		// Back to the list asked for, such as the one an export came from.
		returnTo = state.data.Query.Path(max(state.data.Query.Page, 1))
	}
	if r.Method == http.MethodPost && pageError != "" {
		if address := adminSecurityVerificationReturnTo(r.URL.EscapedPath()); address != "" {
			returnTo = address
		}
	}
	openVerification := pageError != "" || r.URL.Query().Get("verification_required") == "1"
	h.writeAdminUsersPage(w, r, status, state, returnTo, openVerification, pageError)
}

// adminUsersState is what every answer about the Users page starts from:
// one page of its list, everyone's counts, the instance's 2FA policy, and
// the session's verification state.
type adminUsersState struct {
	data   views.AdminUsersData
	counts auth.AdministratorUserCounts
	policy auth.InstanceMFAPolicy
	access *auth.SecuritySettingsAccess
}

// adminUsersQueryFrom reads the list's search, filters, order and page from
// an address, keeping only what it understands.
func adminUsersQueryFrom(values url.Values) views.AdminUsersQuery {
	query := views.AdminUsersQuery{Search: strings.TrimSpace(values.Get("q"))}
	if search := []rune(query.Search); len(search) > adminUsersSearchLength {
		query.Search = string(search[:adminUsersSearchLength])
	}
	switch status := auth.UserStatus(values.Get("status")); status {
	case auth.UserStatusActive, auth.UserStatusPending, auth.UserStatusDisabled:
		query.Status = string(status)
	}
	if attention := auth.AdministratorUserAttention(values.Get("attention")); attention.Valid() {
		query.Attention = string(attention)
	}
	if sort := auth.AdministratorUserSort(values.Get("sort")); sort.Valid() && sort != auth.AdministratorUserSortUsername {
		query.Sort = string(sort)
	}
	if page, err := strconv.Atoi(values.Get("page")); err == nil && page > 1 {
		query.Page = page
	}
	return query
}

// adminUsersSearchLength bounds a search.
const adminUsersSearchLength = 100

// loadAdminUsers reads a page of the Users page's list, as query asks, and
// whether the session must verify again. When it fails it has answered
// already, and ok is false.
func (h *Handler) loadAdminUsers(w http.ResponseWriter, r *http.Request, query views.AdminUsersQuery) (state adminUsersState, ok bool) {
	return h.loadAdminUserList(w, r, query, auth.DefaultAdministratorUserPageSize)
}

// loadAdminUserList is loadAdminUsers with pages of size people; a page of
// one is enough for the counts.
func (h *Handler) loadAdminUserList(w http.ResponseWriter, r *http.Request, query views.AdminUsersQuery, size int) (state adminUsersState, ok bool) {
	ctx := r.Context()
	currentUser := auth.GetCurrentUser(ctx)
	currentSession := auth.GetCurrentSession(ctx)
	if currentUser == nil || currentSession == nil || h.auth == nil {
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return state, false
	}
	pageNumber := max(query.Page, 1)
	list := func() (*auth.AdministratorUserPage, error) {
		return h.auth.ListAdministratorUserPage(ctx, currentUser.ID, currentSession.ID, auth.AdministratorUserQuery{
			Search: query.Search, Status: auth.UserStatus(query.Status), Attention: auth.AdministratorUserAttention(query.Attention),
			Sort: auth.AdministratorUserSort(query.Sort), Offset: (pageNumber - 1) * size, Limit: size,
		})
	}
	page, err := list()
	// An address past the last page shows the last one.
	if err == nil && len(page.Users) == 0 && pageNumber > 1 && page.Matching > 0 {
		pageNumber = (page.Matching + size - 1) / size
		page, err = list()
	}
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
	state.policy = policy.MFA
	state.counts = page.Counts
	state.data = adminUsersViewData(page.Users, currentUser.ID, policy.MFA)
	state.data.Total = page.Counts.Total
	state.data.Active, state.data.Pending, state.data.Disabled = page.Counts.Active, page.Counts.Pending, page.Counts.Disabled
	state.data.Administrators = page.Counts.Administrators
	if pageNumber > 1 {
		query.Page = pageNumber
	} else {
		query.Page = 0
	}
	state.data.Query = query
	for i := range state.data.Users {
		state.data.Users[i].ListPath = query.Path(pageNumber)
	}
	state.data.Matching = page.Matching
	if h.auth.IsEnabled() {
		state.data.BulkActions = adminUsersBulkActionNames()
		state.data.BulkLimit = admin.MaxBulkPeople
		state.data.Exportable = true
	}
	offset := (pageNumber - 1) * size
	if len(page.Users) > 0 {
		state.data.From, state.data.To = offset+1, offset+len(page.Users)
	}
	if pageNumber > 1 {
		state.data.PreviousPath = query.Path(pageNumber - 1)
	}
	if offset+len(page.Users) < page.Matching {
		state.data.NextPath = query.Path(pageNumber + 1)
	}
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

// adminUserFor is the person an action's address names, as the Users page
// shows them, when the action is one they can take.
func (h *Handler) adminUserFor(r *http.Request, action views.AdminUserAction, policy auth.InstanceMFAPolicy) (views.AdminUserData, bool) {
	ctx := r.Context()
	actor, ok := admin.ActorFromContext(ctx)
	if !ok {
		return views.AdminUserData{}, false
	}
	userID := r.PathValue("userID")
	if reference := r.PathValue("reference"); reference != "" {
		id, err := h.auth.AdministratorInvitationUserID(ctx, actor.UserID, actor.SessionID, reference)
		if err != nil {
			return views.AdminUserData{}, false
		}
		userID = id
	}
	if userID == "" {
		return views.AdminUserData{}, false
	}
	page, err := h.auth.ListAdministratorUserPage(ctx, actor.UserID, actor.SessionID, auth.AdministratorUserQuery{UserIDs: []string{userID}, Limit: 1})
	if err != nil {
		log.Printf("load administrator user %q: %v", userID, err)
		return views.AdminUserData{}, false
	}
	user, found := adminUserAt(adminUsersViewData(page.Users, actor.UserID, policy).Users, action, r.URL.EscapedPath())
	user.ListPath = adminUsersListFrom(r.URL.Query().Get("list"))
	return user, found
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
