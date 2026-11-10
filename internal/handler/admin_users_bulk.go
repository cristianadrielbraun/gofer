package handler

import (
	"context"
	"log"
	"net/http"
	"net/url"

	"github.com/cristianadrielbraun/gofer/internal/admin"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// adminUsersBulkActionNames are the changes the Users page makes to many
// people at once, by the names their addresses use.
func adminUsersBulkActionNames() []string {
	names := make([]string, len(admin.BulkActions))
	for i, action := range admin.BulkActions {
		names[i] = string(action)
	}
	return names
}

// adminUsersBulkPath is where action's dialog is and where it's sent.
func adminUsersBulkPath(action admin.BulkAction) string {
	return auth.AdminUsersBulkPath + string(action)
}

// adminUsersBulkChange is how the Users page answers a change to many
// people that the administrator couldn't make at all.
func adminUsersBulkChange(action admin.BulkAction) adminUserChange {
	change := adminUserChange{
		name: "change many people (" + string(action) + ")", verify: "auth.admin.verify.user_access",
		refused: map[admin.Code]string{
			admin.CodeInvalid: "auth.admin.users.bulk_invalid", admin.CodeTooMany: "auth.admin.users.bulk_invalid",
		},
		failed: "auth.admin.users.bulk_failed",
	}
	switch action {
	case admin.BulkRequireMFA, admin.BulkClearMFA:
		change.verify = "auth.admin.verify.mfa_requirement"
	case admin.BulkRequirePasswordChange:
		change.verify = "auth.admin.verify.require_password"
	}
	return change
}

// handleAdminUsersBulkDialog answers a change's address with its dialog for
// the people the address names: each by ID (user=), or everyone a list
// matches (all=1 and the list's search and filters). htmx, asking from the
// selection's menu, gets the dialog alone; any other request gets the Users
// page with the dialog open.
func (h *Handler) handleAdminUsersBulkDialog(action admin.BulkAction) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.auth != nil && !h.auth.IsEnabled() {
			http.Redirect(w, r, views.AdminAvatarsPath, http.StatusFound)
			return
		}
		values := r.URL.Query()
		list := adminUsersListFrom(values.Get("list"))
		state, ok := h.loadAdminUsers(w, r, adminUsersListQuery(list))
		if !ok {
			return
		}
		bulk, err := h.adminUsersBulkPlan(r.Context(), action, values, state.policy)
		if err != nil {
			log.Printf("plan a change to many people (%s): %v", action, err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		bulk.ListPath = list
		if r.Header.Get("HX-Request") == "true" {
			var verification views.AdminSecurityVerificationData
			if state.data.StepUpRequired && r.Header.Get(views.AdminVerificationOnPageHeader) == "" {
				verification = adminSecurityVerificationViewData(r.Context(), state.access, adminUsersPath, false)
			}
			h.writeAdminFragment(w, r, views.AdminUsersBulkDialog(bulk, verification))
			return
		}
		state.data.Bulk = &bulk
		h.writeAdminUsersPage(w, r, http.StatusOK, state, adminUsersPath, false, "")
	}
}

// adminUsersListQuery is the query of list, one of the Users page's own
// addresses, or the whole list's when list is empty.
func adminUsersListQuery(list string) views.AdminUsersQuery {
	parsed, err := url.Parse(list)
	if list == "" || err != nil {
		return views.AdminUsersQuery{}
	}
	return adminUsersQueryFrom(parsed.Query())
}

// adminUsersBulkPlan is action's dialog before it's made: whom values name,
// split into those it would change and those it leaves out, in the order
// they were named or, for everyone a list matches, the list's. TooMany says
// they're more than one change takes.
func (h *Handler) adminUsersBulkPlan(ctx context.Context, action admin.BulkAction, values url.Values, policy auth.InstanceMFAPolicy) (views.AdminUsersBulkData, error) {
	path := adminUsersBulkPath(action)
	plan := views.AdminUsersBulkData{
		Action: string(action), Path: path, Limit: admin.MaxBulkPeople,
		CSRFToken: auth.CSRFToken(ctx, http.MethodPost, path),
	}
	actor, ok := admin.ActorFromContext(ctx)
	if !ok {
		return plan, auth.ErrAdministratorRequired
	}
	query := auth.AdministratorUserQuery{Limit: admin.MaxBulkPeople}
	var ids []string
	if values.Get("all") == "1" {
		list := adminUsersQueryFrom(values)
		query.Search, query.Status = list.Search, auth.UserStatus(list.Status)
		query.Attention, query.Sort = auth.AdministratorUserAttention(list.Attention), auth.AdministratorUserSort(list.Sort)
	} else {
		seen := map[string]bool{}
		for _, id := range values["user"] {
			if adminUserActionTargetID(id) && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		if len(ids) > admin.MaxBulkPeople {
			plan.TooMany = true
			return plan, nil
		}
		if len(ids) == 0 {
			return plan, nil
		}
		query.UserIDs = ids
	}
	page, err := h.auth.ListAdministratorUserPage(ctx, actor.UserID, actor.SessionID, query)
	if err != nil {
		return plan, err
	}
	if page.Matching > admin.MaxBulkPeople {
		plan.TooMany = true
		return plan, nil
	}
	people := page.Users
	if ids != nil {
		// The people named, in the order they were named, anyone gone
		// among them.
		byID := make(map[string]auth.AdministratorUserSummary, len(people))
		for _, user := range people {
			byID[user.ID] = user
		}
		people = make([]auth.AdministratorUserSummary, len(ids))
		for i, id := range ids {
			if user, found := byID[id]; found {
				people[i] = user
			} else {
				people[i] = auth.AdministratorUserSummary{ID: id}
			}
		}
	}
	for _, user := range people {
		person := views.AdminUsersBulkPerson{ID: user.ID, Username: user.Username, Reason: string(admin.ReasonGone)}
		if user.Status != "" {
			person.Reason = string(admin.BulkReason(actor.UserID, action, user, policy))
		}
		if person.Reason != "" {
			plan.LeftOut = append(plan.LeftOut, person)
			continue
		}
		plan.Change = append(plan.Change, person)
	}
	return plan, nil
}

// handleAdminUsersBulk makes action to the people the form names, then
// shows the list it was made from with what the change did to each.
func (h *Handler) handleAdminUsersBulk(action admin.BulkAction) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if h.ownedMailbox != nil && (action == admin.BulkDisable || action == admin.BulkEnable) {
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
			h.renderAdminUsers(w, r, http.StatusBadRequest, views.AdminUserInvitationFormData{}, nil, i18n.T(ctx, "auth.admin.users.bulk_invalid"))
			return
		}
		result, err := h.adminService().Bulk(ctx, actor, action, r.PostForm["user"])
		if err != nil {
			h.adminUserChangeFailed(w, r, err, adminUsersBulkChange(action), views.AdminUserInvitationFormData{})
			return
		}
		list := adminUsersListFrom(r.PostFormValue("list"))
		done := views.AdminUsersBulkData{Action: string(action), Limit: admin.MaxBulkPeople, ListPath: list, Done: true}
		for _, outcome := range result.Outcomes {
			if outcome.Err != nil {
				log.Printf("change many people (%s, %s): %s: %v", action, result.Correlation, outcome.UserID, outcome.Err)
			}
			person := views.AdminUsersBulkPerson{ID: outcome.UserID, Username: outcome.Username, Reason: string(outcome.Reason), Sessions: outcome.Sessions}
			if outcome.Changed {
				done.Change = append(done.Change, person)
			} else {
				done.LeftOut = append(done.LeftOut, person)
			}
		}
		state, ok := h.loadAdminUsers(w, r, adminUsersListQuery(list))
		if !ok {
			return
		}
		state.data.Bulk = &done
		h.writeAdminUsersPage(w, r, http.StatusOK, state, adminUsersPath, false, "")
	}
}
