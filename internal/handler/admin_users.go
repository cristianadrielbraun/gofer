package handler

import (
	"bytes"
	"errors"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func adminUsersViewData(users []auth.AdministratorUserSummary, currentUserID string) views.AdminUsersData {
	data := views.AdminUsersData{Users: make([]views.AdminUserData, 0, len(users)), Total: len(users)}
	for _, user := range users {
		view := views.AdminUserData{
			ID:       user.ID,
			Username: user.Username,
			Email:    user.Email,
			Status:   "Disabled",
			Role:     "User",
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
			view.Role = "Administrator"
			data.Administrators++
		}
		data.Users = append(data.Users, view)
	}
	return data
}

func (h *Handler) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
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
	uiSettings := h.db.GetUISettings(ctx, currentUser.ID)
	var output bytes.Buffer
	if r.Header.Get("HX-Request") == "true" {
		err = views.AdminPartial(data, models.AvatarStatus{}, models.ContactAdminStatus{}, models.LabelAdminStatus{}, models.MailSecurityAdminData{}, models.MailOperationsAdminStatus{}, "users", "").Render(ctx, &output)
	} else {
		err = views.AdminLayout(uiSettings, data, models.AvatarStatus{}, models.ContactAdminStatus{}, models.LabelAdminStatus{}, models.MailSecurityAdminData{}, models.MailOperationsAdminStatus{}, "users", "").Render(ctx, &output)
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
	_, _ = output.WriteTo(w)
}
