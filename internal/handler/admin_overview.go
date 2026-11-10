package handler

import (
	"errors"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// adminOverviewRecent is how many administrative changes the Overview shows.
const adminOverviewRecent = 5

// handleAdminOverview shows the console's front page: the people, what
// needs an administrator, and the latest administrative changes. It reads
// only the central database, so it stays quick however many people there
// are; what needs each person's own database waits for health snapshots.
func (h *Handler) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.auth == nil || !h.auth.IsEnabled() {
		http.Redirect(w, r, views.AdminAvatarsPath, http.StatusFound)
		return
	}
	state, ok := h.loadAdminUsers(w, r)
	if !ok {
		return
	}
	data := views.AdminOverviewData{
		Total: state.data.Total, Active: state.data.Active, Pending: state.data.Pending,
		Disabled: state.data.Disabled, Administrators: state.data.Administrators,
	}
	var resets, mfaSetup, expired, deleting int
	for _, user := range state.data.Users {
		if user.PasswordResetRequestedAt != nil {
			resets++
		}
		if user.MFASetupPending {
			mfaSetup++
		}
		if user.InvitationState == string(auth.AdministratorUserInvitationExpired) {
			expired++
		}
		if user.DeletionPending {
			deleting++
		}
	}
	for _, item := range []views.AdminOverviewItem{
		{Key: "reset_requests", Count: resets, Href: adminUsersPath},
		{Key: "mfa_setup", Count: mfaSetup, Href: adminUsersPath},
		{Key: "expired_invitations", Count: expired, Href: adminUsersPath},
		{Key: "deletions", Count: deleting, Href: adminUsersPath},
	} {
		if item.Count > 0 {
			data.Attention = append(data.Attention, item)
		}
	}
	if state.data.StepUpRequired {
		data.RecentLocked = true
	} else {
		user, session := auth.GetCurrentUser(ctx), auth.GetCurrentSession(ctx)
		page, err := h.auth.ListAdministrationEvents(ctx, user.ID, session.ID, audit.Filter{}, 0, adminOverviewRecent)
		switch {
		case err == nil:
			data.Recent = page.Records
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			data.RecentLocked = true
		default:
			log.Printf("admin overview: load recent changes: %v", err)
			http.Error(w, i18n.T(ctx, "server.error.failed_to_load_administrator_security_activity"), http.StatusInternalServerError)
			return
		}
	}
	h.renderAdminPage(w, r, http.StatusOK, views.AdminShellPage{
		Section: views.AdminSectionOverview,
		Content: views.AdminOverviewPage(data),
	})
}
