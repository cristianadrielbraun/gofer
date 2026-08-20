package handler

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const adminUserInvitationPath = "/admin/users/invitations"

func adminUsersViewData(users []auth.AdministratorUserSummary, currentUserID string) views.AdminUsersData {
	data := views.AdminUsersData{Users: make([]views.AdminUserData, 0, len(users)), Total: len(users)}
	for _, user := range users {
		view := views.AdminUserData{
			ID:       user.ID,
			Username: user.Username,
			Email:    user.Email,
			Status:   "Disabled",
			Role:     "Webmail user",
			Current:  user.ID == currentUserID,
		}
		switch user.Status {
		case auth.UserStatusActive:
			view.Status = "Active"
			data.Active++
		case auth.UserStatusPending:
			view.Status = "Pending"
			data.Pending++
		case auth.UserStatusDisabled:
			data.Disabled++
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
		Name: r.PostFormValue("name"), Username: r.PostFormValue("username"), Email: r.PostFormValue("email"),
	}
	invitation, err := h.auth.CreateAdministratorUserInvitation(ctx, auth.CreateAdministratorUserInvitationOptions{
		ActorUserID: currentUser.ID, ActorSessionID: currentSession.ID,
		Name: form.Name, Username: form.Username, Email: form.Email,
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
		Name: invitation.Name, Username: invitation.User.Username, Email: invitation.User.Email,
		RedemptionURL: strings.TrimRight(h.auth.Config().BaseURL, "/") + enrollmentRedemptionPath,
		Token:         invitation.Token.Token, ExpiresAt: invitation.Token.ExpiresAt,
	}
	h.renderAdminUsers(w, r, http.StatusCreated, views.AdminUserInvitationFormData{}, result, "")
}

func (h *Handler) renderAdminUsers(w http.ResponseWriter, r *http.Request, status int, form views.AdminUserInvitationFormData, invitation *views.AdminUserInvitationData, pageError string) {
	ctx := r.Context()
	currentUser := auth.GetCurrentUser(ctx)
	if currentUser == nil || h.auth == nil {
		http.Error(w, "admin access required", http.StatusForbidden)
		return
	}
	users, err := h.auth.ListAdministratorUsers(ctx, currentUser.ID)
	if errors.Is(err, auth.ErrAdministratorRequired) {
		http.Error(w, "admin access required", http.StatusForbidden)
		return
	}
	if err != nil {
		log.Printf("load administrator users: %v", err)
		http.Error(w, "failed to load administrator users", http.StatusInternalServerError)
		return
	}
	data := adminUsersViewData(users, currentUser.ID)
	data.InvitationForm = form
	data.Invitation = invitation
	data.Error = pageError
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
