package handler

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// adminOverviewRecent is how many administrative changes the Overview shows.
const adminOverviewRecent = 5

// adminOverviewRefusedSince is how far back the Overview counts refused
// attempts.
const adminOverviewRefusedSince = 7 * 24 * time.Hour

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
	// The counts are everyone's; a page of one is all the list it takes.
	state, ok := h.loadAdminUserList(w, r, views.AdminUsersQuery{}, 1)
	if !ok {
		return
	}
	counts := state.counts
	data := views.AdminOverviewData{
		Total: counts.Total, Active: counts.Active, Pending: counts.Pending,
		Disabled: counts.Disabled, Administrators: counts.Administrators,
	}
	// Each item leads to the people it counts.
	for _, item := range []views.AdminOverviewItem{
		{Key: "reset_requests", Count: counts.ResetRequested, Href: views.AdminUsersQuery{Attention: string(auth.AdministratorUserAttentionResetRequested)}.Path(1)},
		{Key: "mfa_setup", Count: counts.MFASetup, Href: views.AdminUsersQuery{Attention: string(auth.AdministratorUserAttentionMFASetup)}.Path(1)},
		{Key: "expired_invitations", Count: counts.InvitationExpired, Href: views.AdminUsersQuery{Attention: string(auth.AdministratorUserAttentionInvitationExpired)}.Path(1)},
		{Key: "deletions", Count: counts.Deleting, Href: views.AdminUsersQuery{Attention: string(auth.AdministratorUserAttentionDeleting)}.Path(1)},
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
		if !data.RecentLocked {
			// Refused attempts are part of the log, so only a session that
			// may read it sees them counted.
			refused, err := h.auth.ListAdministrationEvents(ctx, user.ID, session.ID, audit.Filter{
				Action: auth.AuditActionAccessDenied, Since: time.Now().Add(-adminOverviewRefusedSince),
			}, 0, audit.MaxPageSize)
			switch {
			case err != nil:
				log.Printf("admin overview: count refused attempts: %v", err)
			case len(refused.Records) > 0:
				data.Attention = append(data.Attention, views.AdminOverviewItem{
					Key: "refused", Count: len(refused.Records), Href: views.AdminAuditActivityPath + "?kind=access",
				})
			}
		}
	}
	h.renderAdminPage(w, r, http.StatusOK, views.AdminShellPage{
		Section: views.AdminSectionOverview,
		Content: views.AdminOverviewPage(data),
	})
}
