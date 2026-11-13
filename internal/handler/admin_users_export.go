package handler

import (
	"encoding/csv"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

// adminUsersCSVHeader names the export's columns. They hold what the Users
// page shows, in the words the page's own values use, so a spreadsheet can
// filter on them; times are UTC, in RFC 3339.
var adminUsersCSVHeader = []string{
	"username", "user_id", "status", "deleting", "role",
	"two_factor", "two_factor_rule", "two_factor_set_up",
	"has_password", "password_change_required", "password_reset_requested_at",
	"invitation", "invitation_expires_at", "created_at", "last_sign_in_at",
}

// handleExportAdminUsers downloads everyone the Users list's search and
// filters keep, in its order, as CSV. The export is recorded first. A
// session that must verify again goes back to the list, which asks for it.
func (h *Handler) handleExportAdminUsers(w http.ResponseWriter, r *http.Request) {
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
	query := adminUsersQueryFrom(r.URL.Query())
	query.Page = 0
	listPath := query.Path(1)
	people, err := h.auth.ExportAdministratorUsers(ctx, user.ID, session.ID, auth.AdministratorUserQuery{
		Search: query.Search, Status: auth.UserStatus(query.Status),
		Attention: auth.AdministratorUserAttention(query.Attention), Sort: auth.AdministratorUserSort(query.Sort),
		Group: query.Group,
	})
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrRecentStepUpRequired):
		http.Redirect(w, r, adminUsersAddParameter(listPath, "verification_required=1"), http.StatusSeeOther)
		return
	case errors.Is(err, auth.ErrAdministratorRequired):
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return
	default:
		log.Printf("users export: %v", err)
		h.setAdminFlashError(w, i18n.T(ctx, "admin.users.export_failed"))
		http.Redirect(w, r, listPath, http.StatusSeeOther)
		return
	}
	policy, err := h.auth.InstanceSecurityPolicy(ctx)
	if err != nil {
		log.Printf("users export: load MFA policy: %v", err)
		h.setAdminFlashError(w, i18n.T(ctx, "admin.users.export_failed"))
		http.Redirect(w, r, listPath, http.StatusSeeOther)
		return
	}
	// The same words the page's columns are drawn from.
	views := adminUsersViewData(people, user.ID, policy.MFA).Users

	setAdminPageHeaders(w)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="gofer-users-`+time.Now().UTC().Format("2006-01-02")+`.csv"`)
	w.WriteHeader(http.StatusOK)
	out := csv.NewWriter(w)
	_ = out.Write(adminUsersCSVHeader)
	for i, person := range people {
		view := views[i]
		invitation := view.InvitationState
		if person.Status != auth.UserStatusPending {
			invitation = ""
		}
		if err := out.Write([]string{
			audit.CSVSafe(person.Username), person.ID, string(person.Status), strconv.FormatBool(person.DeletionPending), view.Role,
			view.MFALabel, view.MFADetail, strconv.FormatBool(person.HasSecondFactor),
			strconv.FormatBool(person.HasPassword), strconv.FormatBool(person.PasswordChangeRequired), adminUsersCSVTime(person.PasswordResetRequestedAt),
			invitation, adminUsersCSVTime(person.InvitationExpiresAt), adminUsersCSVTime(&person.CreatedAt), adminUsersCSVTime(person.LastLoginAt),
		}); err != nil {
			break
		}
	}
	out.Flush()
	if err := out.Error(); err != nil {
		// The download already started; all that's left is to stop it.
		log.Printf("users export: interrupted: %v", err)
		return
	}
	log.Printf("users export: %d people", len(people))
}

func adminUsersCSVTime(at *time.Time) string {
	if at == nil || at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

// adminUsersAddParameter adds parameter to path's query.
func adminUsersAddParameter(path, parameter string) string {
	if strings.Contains(path, "?") {
		return path + "&" + parameter
	}
	return path + "?" + parameter
}
