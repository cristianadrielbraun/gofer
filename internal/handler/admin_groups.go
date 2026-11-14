package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/admin"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/directory"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/policy"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// adminGroupCandidatesShown is how many people the Add people dialog
// offers at once; a search narrows them.
const adminGroupCandidatesShown = 30

// adminGroupPolicyRemove is where a group's page takes a policy away.
const adminGroupPolicyRemove = views.AdminGroupTabPolicies + "/remove"

func (h *Handler) registerAdminGroupRoutes(adminRoute func(string, http.HandlerFunc)) {
	adminRoute("GET "+views.AdminGroupsPath, h.handleAdminGroups)
	adminRoute("GET "+views.AdminGroupsPath+"/{$}", h.handleAdminGroups)
	adminRoute("POST "+views.AdminGroupsPath, h.handleCreateAdminGroup)
	adminRoute("GET "+views.AdminGroupsPath+"/{groupID}", h.handleAdminGroupPage(views.AdminGroupTabMembers, adminGroupExtras{}))
	adminRoute("GET "+views.AdminGroupsPath+"/{groupID}/"+views.AdminGroupTabSettings, h.handleAdminGroupPage(views.AdminGroupTabSettings, adminGroupExtras{}))
	adminRoute("GET "+views.AdminGroupsPath+"/{groupID}/"+views.AdminGroupTabPolicies, h.handleAdminGroupPage(views.AdminGroupTabPolicies, adminGroupExtras{}))
	adminRoute("GET "+views.AdminGroupsPath+"/{groupID}/"+views.AdminGroupPolicyAssign, h.handleAdminGroupPage(views.AdminGroupTabPolicies, adminGroupExtras{policyAssignOpen: true}))
	adminRoute("POST "+views.AdminGroupsPath+"/{groupID}/"+views.AdminGroupPolicyAssign, h.handleChangeAdminGroupPolicy(true))
	adminRoute("POST "+views.AdminGroupsPath+"/{groupID}/"+adminGroupPolicyRemove, h.handleChangeAdminGroupPolicy(false))
	adminRoute("GET "+views.AdminGroupsPath+"/{groupID}/add", h.handleAdminGroupPage(views.AdminGroupTabMembers, adminGroupExtras{addOpen: true}))
	adminRoute("GET "+views.AdminGroupsPath+"/{groupID}/delete", h.handleAdminGroupPage(views.AdminGroupTabSettings, adminGroupExtras{deleteOpen: true}))
	adminRoute("POST "+views.AdminGroupsPath+"/{groupID}/"+views.AdminGroupTabSettings, h.handleUpdateAdminGroup)
	adminRoute("POST "+views.AdminGroupsPath+"/{groupID}/members", h.handleChangeAdminGroupMembers(true))
	adminRoute("POST "+views.AdminGroupsPath+"/{groupID}/members/remove", h.handleChangeAdminGroupMembers(false))
	adminRoute("POST "+views.AdminGroupsPath+"/{groupID}/delete", h.handleDeleteAdminGroup)
}

// adminStepUpVerification says whether the session must verify again before
// a change on a console page, and the dialog that asks, leading back to returnTo. A session
// that is no longer valid is sent to sign in, and ok is false.
func (h *Handler) adminStepUpVerification(w http.ResponseWriter, r *http.Request, returnTo string, open bool) (views.AdminSecurityVerificationData, bool, bool) {
	ctx := r.Context()
	access, err := h.auth.GetSecuritySettingsAccess(ctx, auth.GetSessionToken(r))
	switch {
	case err == nil:
		if access.StepUpFresh {
			return views.AdminSecurityVerificationData{}, false, true
		}
		open = open || r.URL.Query().Get("verification_required") == "1"
		return adminSecurityVerificationViewData(ctx, access, returnTo, open), true, true
	case errors.Is(err, auth.ErrSecuritySessionInvalid):
		auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
		redirectAdmin(w, r, "/admin/login")
	default:
		log.Printf("load administrator verification state: %v", err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_verify_administrator_session"), http.StatusInternalServerError)
	}
	return views.AdminSecurityVerificationData{}, false, false
}

// adminGroupsExtras are what the Groups page shows over its list: the New
// group dialog as it was sent, and a change that was refused.
type adminGroupsExtras struct {
	status     int
	form       views.AdminGroupForm
	createOpen bool
	pageError  string
}

func (h *Handler) handleAdminGroups(w http.ResponseWriter, r *http.Request) {
	h.renderAdminGroups(w, r, adminGroupsExtras{})
}

func (h *Handler) renderAdminGroups(w http.ResponseWriter, r *http.Request, extras adminGroupsExtras) {
	ctx := r.Context()
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	groups, err := h.adminService().ListGroups(ctx, actor)
	if admin.CodeOf(err) == admin.CodeForbidden {
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	if err != nil {
		log.Printf("load groups: %v", err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_load_groups"), http.StatusInternalServerError)
		return
	}
	data := views.AdminGroupsData{
		Search: strings.TrimSpace(r.URL.Query().Get("q")), Total: len(groups),
		Form: extras.form, CreateOpen: extras.createOpen,
		CSRFToken: auth.CSRFToken(ctx, http.MethodPost, views.AdminGroupsPath),
	}
	protected, err := h.adminService().ProtectedGroups(ctx, actor)
	if err != nil && admin.CodeOf(err) != admin.CodeForbidden {
		log.Printf("find the protected groups: %v", err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_load_groups"), http.StatusInternalServerError)
		return
	}
	search := strings.ToLower(data.Search)
	for _, group := range groups {
		if search != "" && !strings.Contains(strings.ToLower(group.Name), search) && !strings.Contains(strings.ToLower(group.Description), search) {
			continue
		}
		data.Groups = append(data.Groups, views.AdminGroupRow{
			ID: group.ID, Name: group.Name, Description: group.Description, Members: group.MemberCount, UpdatedAt: group.UpdatedAt,
			Protected: protected[group.ID],
		})
	}
	verification, stepUp, ok := h.adminStepUpVerification(w, r, views.AdminGroupsPath, extras.pageError != "")
	if !ok {
		return
	}
	data.StepUpRequired = stepUp
	h.renderAdminPage(w, r, adminStatus(extras.status), views.AdminShellPage{
		Section: views.AdminSectionGroups,
		Content: views.AdminGroupsPage(data, verification),
		Toasts:  adminErrorToasts(extras.pageError),
	})
}

func (h *Handler) handleCreateAdminGroup(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	form := views.AdminGroupForm{Name: r.PostFormValue("name"), Description: r.PostFormValue("description")}
	group, err := h.adminService().CreateGroup(ctx, actor, form.Name, form.Description)
	if err == nil {
		h.setAdminFlashSuccess(w, i18n.T(ctx, "admin.groups.created", "group", group.Name))
		redirectAdmin(w, r, views.AdminGroupPath(group.ID, views.AdminGroupTabMembers))
		return
	}
	extras := adminGroupsExtras{form: form, createOpen: true}
	if !adminGroupFormError(ctx, err, &extras.form) {
		extras.status, extras.pageError = adminGroupRefusal(ctx, err, "creating")
		extras.createOpen = extras.status != http.StatusForbidden
	} else {
		extras.status = http.StatusBadRequest
	}
	h.renderAdminGroups(w, r, extras)
}

// adminGroupFormError puts a refused name or description on its field,
// and says whether it did.
func adminGroupFormError(ctx context.Context, err error, form *views.AdminGroupForm) bool {
	switch {
	case errors.Is(err, directory.ErrNameTaken):
		form.NameError = i18n.T(ctx, "admin.groups.error.name_taken")
	case errors.Is(err, directory.ErrNameInvalid):
		form.NameError = i18n.T(ctx, "admin.groups.error.name_invalid")
	case errors.Is(err, directory.ErrDescriptionLong):
		form.DescriptionError = i18n.T(ctx, "admin.groups.error.description_invalid")
	default:
		return false
	}
	return true
}

// adminGroupRefusal is the status and message for a change to groups that
// didn't go through for a reason no field shows.
func adminGroupRefusal(ctx context.Context, err error, doing string) (int, string) {
	switch admin.CodeOf(err) {
	case admin.CodeForbidden:
		return http.StatusForbidden, i18n.T(ctx, "server.error.admin_access_required")
	case admin.CodeVerificationRequired:
		return http.StatusForbidden, i18n.T(ctx, "admin.groups.error.verify")
	case admin.CodeStale:
		return http.StatusConflict, i18n.T(ctx, "admin.groups.error.stale")
	case admin.CodeUnavailable:
		return http.StatusConflict, i18n.T(ctx, "admin.groups.error.unavailable")
	case admin.CodeInvalid:
		return http.StatusBadRequest, i18n.T(ctx, "admin.groups.error.nobody")
	case admin.CodeTooMany:
		return http.StatusBadRequest, i18n.N(ctx, "admin.groups.error.too_many", admin.MaxBulkPeople, "count", i18n.Number(ctx, admin.MaxBulkPeople))
	}
	log.Printf("%s a group: %v", doing, err)
	return http.StatusInternalServerError, i18n.T(ctx, "admin.groups.error.failed")
}

// adminGroupExtras are what a group's page shows over its tab: a dialog
// open, the settings as they were sent, and a change that was refused.
type adminGroupExtras struct {
	status           int
	addOpen          bool
	deleteOpen       bool
	policyAssignOpen bool
	settings         *views.AdminGroupForm
	pageError        string
}

func (h *Handler) handleAdminGroupPage(tab string, extras adminGroupExtras) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.renderAdminGroupPage(w, r, tab, extras)
	}
}

func (h *Handler) renderAdminGroupPage(w http.ResponseWriter, r *http.Request, tab string, extras adminGroupExtras) {
	ctx := r.Context()
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	service := h.adminService()
	groupID := r.PathValue("groupID")
	detail, err := service.Group(ctx, actor, groupID)
	switch admin.CodeOf(err) {
	case "":
	case admin.CodeForbidden:
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return
	case admin.CodeUnavailable:
		if extras.pageError != "" {
			// The group went while the change was on its way.
			h.setAdminFlashError(w, extras.pageError)
			redirectAdmin(w, r, views.AdminGroupsPath)
			return
		}
		http.NotFound(w, r)
		return
	default:
		log.Printf("load group %q: %v", groupID, err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_load_groups"), http.StatusInternalServerError)
		return
	}
	group := detail.Group
	data := views.AdminGroupPageData{
		ID: group.ID, Name: group.Name, Description: group.Description, Version: group.Version,
		CreatedAt: group.CreatedAt, UpdatedAt: group.UpdatedAt,
		CreatedBy: h.adminUsername(ctx, group.CreatedBy), UpdatedBy: h.adminUsername(ctx, group.UpdatedBy),
		Tab: tab, AddOpen: extras.addOpen, DeleteOpen: extras.deleteOpen,
		Settings: views.AdminGroupForm{Name: group.Name, Description: group.Description},
		CSRF: views.AdminGroupCSRF{
			Add:      auth.CSRFToken(ctx, http.MethodPost, views.AdminGroupPath(group.ID, "members")),
			Remove:   auth.CSRFToken(ctx, http.MethodPost, views.AdminGroupPath(group.ID, "members/remove")),
			Settings: auth.CSRFToken(ctx, http.MethodPost, views.AdminGroupPath(group.ID, views.AdminGroupTabSettings)),
			Delete:   auth.CSRFToken(ctx, http.MethodPost, views.AdminGroupPath(group.ID, "delete")),
		},
	}
	if extras.settings != nil {
		data.Settings = *extras.settings
	}
	for _, member := range detail.Members {
		data.Members = append(data.Members, views.AdminGroupMember{
			UserID: member.UserID, Username: member.Username, Status: string(member.Status),
			AddedAt: member.AddedAt, AddedBy: member.AddedByUsername,
		})
	}
	if err := h.loadAdminGroupPolicies(ctx, actor, group.ID, group.Name, extras.policyAssignOpen, &data); err != nil {
		log.Printf("load group %q policies: %v", group.ID, err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_load_groups"), http.StatusInternalServerError)
		return
	}
	if data.AddOpen {
		data.AddSearch = strings.TrimSpace(r.URL.Query().Get("q"))
		page, err := h.auth.ListAdministratorUserPage(ctx, actor.UserID, actor.SessionID, auth.AdministratorUserQuery{
			Search: data.AddSearch, Groupable: true, NotInGroup: group.ID, Limit: adminGroupCandidatesShown,
		})
		if err != nil {
			log.Printf("find people to add to group %q: %v", group.ID, err)
			http.Error(w, i18n.T(ctx, "server.error.failed_to_load_groups"), http.StatusInternalServerError)
			return
		}
		for _, person := range page.Users {
			data.Candidates = append(data.Candidates, views.AdminGroupCandidate{ID: person.ID, Username: person.Username, Status: string(person.Status)})
		}
		data.CandidatesMore = page.Matching > len(page.Users)
	}

	returnTo := views.AdminGroupPath(group.ID, tab)
	switch {
	case extras.addOpen:
		returnTo = views.AdminGroupAddPath(group.ID, "")
	case extras.deleteOpen:
		returnTo = views.AdminGroupPath(group.ID, "delete")
	case extras.policyAssignOpen:
		returnTo = views.AdminGroupPath(group.ID, views.AdminGroupPolicyAssign)
	}
	verification, stepUp, ok := h.adminStepUpVerification(w, r, returnTo, extras.pageError != "")
	if !ok {
		return
	}
	data.StepUpRequired = stepUp
	h.renderAdminPage(w, r, adminStatus(extras.status), views.AdminShellPage{
		Section: views.AdminSectionGroups,
		Content: views.AdminGroupPage(data, verification),
		Toasts:  adminErrorToasts(extras.pageError),
	})
}

// loadAdminGroupPolicies reads the policies assigned to a group into data,
// and the ones the Assign dialog offers when it's open. An administrator
// who can't see policies sees none.
func (h *Handler) loadAdminGroupPolicies(ctx context.Context, actor admin.Actor, groupID, name string, assignOpen bool, data *views.AdminGroupPageData) error {
	policies, err := h.adminService().PoliciesOfGroup(ctx, actor, groupID)
	if admin.CodeOf(err) == admin.CodeForbidden {
		return nil
	}
	if err != nil {
		return err
	}
	assignPath := views.AdminGroupPath(groupID, views.AdminGroupPolicyAssign)
	removePath := views.AdminGroupPath(groupID, adminGroupPolicyRemove)
	data.Policies = views.AdminTargetPolicies{
		Name: name, Policies: adminTargetPolicyViews(ctx, policies), CanAssign: true, AssignOpen: assignOpen,
		AssignPath: assignPath, AssignCSRF: auth.CSRFToken(ctx, http.MethodPost, assignPath),
		RemovePath: removePath, RemoveCSRF: auth.CSRFToken(ctx, http.MethodPost, removePath),
	}
	assigned := map[string]bool{}
	for _, item := range policies {
		assigned[item.ID] = true
		data.Protected = data.Protected || item.Security
	}
	if !assignOpen {
		return nil
	}
	return h.adminTargetPolicyChoices(ctx, actor, assigned, &data.Policies)
}

// handleChangeAdminGroupPolicy assigns a group the policy the form names,
// or takes it away.
func (h *Handler) handleChangeAdminGroupPolicy(assign bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := h.adminActor(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		groupID := r.PathValue("groupID")
		change, err := h.adminChangeTargetPolicy(r, actor, policy.Target{Kind: policy.TargetGroup, ID: groupID}, assign)
		if err != nil {
			status, message := adminTargetPolicyRefusal(ctx, err)
			h.renderAdminGroupPage(w, r, views.AdminGroupTabPolicies, adminGroupExtras{
				status: status, pageError: message, policyAssignOpen: assign && admin.CodeOf(err) != admin.CodeForbidden,
			})
			return
		}
		h.setAdminFlashSuccess(w, adminPolicyAssignmentMessage(ctx, assign, change))
		redirectAdmin(w, r, views.AdminGroupPath(groupID, views.AdminGroupTabPolicies))
	}
}

// adminUsername names an administrator for a group's details; someone
// deleted since shows as nobody.
func (h *Handler) adminUsername(ctx context.Context, userID string) string {
	if userID == "" {
		return ""
	}
	user, err := h.auth.GetUserByID(ctx, userID)
	if err != nil || user == nil {
		return ""
	}
	return user.Username
}

func (h *Handler) handleUpdateAdminGroup(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	groupID := r.PathValue("groupID")
	form := views.AdminGroupForm{Name: r.PostFormValue("name"), Description: r.PostFormValue("description")}
	version, _ := strconv.ParseInt(r.PostFormValue("version"), 10, 64)
	group, err := h.adminService().UpdateGroup(ctx, actor, groupID, version, form.Name, form.Description)
	if err == nil {
		h.setAdminFlashSuccess(w, i18n.T(ctx, "admin.group.saved", "group", group.Name))
		redirectAdmin(w, r, views.AdminGroupPath(group.ID, views.AdminGroupTabSettings))
		return
	}
	extras := adminGroupExtras{status: http.StatusBadRequest, settings: &form}
	if !adminGroupFormError(ctx, err, &form) {
		extras.status, extras.pageError = adminGroupRefusal(ctx, err, "changing")
		if admin.CodeOf(err) == admin.CodeStale {
			// The page shows the group as it is now.
			extras.settings = nil
		}
	}
	h.renderAdminGroupPage(w, r, views.AdminGroupTabSettings, extras)
}

func (h *Handler) handleDeleteAdminGroup(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	groupID := r.PathValue("groupID")
	service := h.adminService()
	name := ""
	if preview, err := service.PreviewGroupDeletion(ctx, actor, groupID); err == nil {
		name = preview.Group.Name
	}
	version, _ := strconv.ParseInt(r.PostFormValue("version"), 10, 64)
	if _, err := service.DeleteGroup(ctx, actor, groupID, version); err != nil {
		status, message := adminGroupRefusal(ctx, err, "deleting")
		h.renderAdminGroupPage(w, r, views.AdminGroupTabSettings, adminGroupExtras{
			status: status, pageError: message, deleteOpen: admin.CodeOf(err) == admin.CodeVerificationRequired,
		})
		return
	}
	h.setAdminFlashSuccess(w, i18n.T(ctx, "admin.group.deleted", "group", name))
	redirectAdmin(w, r, views.AdminGroupsPath)
}

func (h *Handler) handleChangeAdminGroupMembers(add bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := h.adminActor(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		groupID := r.PathValue("groupID")
		if err := r.ParseForm(); err != nil {
			http.Error(w, i18n.T(ctx, "server.error.invalid_form"), http.StatusBadRequest)
			return
		}
		var people []string
		for _, id := range r.PostForm["user_id"] {
			if id = strings.TrimSpace(id); id != "" {
				people = append(people, id)
			}
		}
		service := h.adminService()
		var change *auth.GroupMembershipChange
		var err error
		if add {
			change, err = service.AddToGroup(ctx, actor, groupID, people)
		} else {
			change, err = service.RemoveFromGroup(ctx, actor, groupID, people)
		}
		if err != nil {
			status, message := adminGroupRefusal(ctx, err, "changing the members of")
			h.renderAdminGroupPage(w, r, views.AdminGroupTabMembers, adminGroupExtras{
				status: status, pageError: message, addOpen: add && admin.CodeOf(err) != admin.CodeForbidden,
			})
			return
		}
		h.setAdminFlashSuccess(w, adminGroupMembersMessage(ctx, add, change))
		redirectAdmin(w, r, views.AdminGroupPath(groupID, views.AdminGroupTabMembers))
	}
}

// adminGroupMembersMessage says who a change to a group's members
// affected, naming one person and counting more.
func adminGroupMembersMessage(ctx context.Context, add bool, change *auth.GroupMembershipChange) string {
	key := "admin.group.removed"
	if add {
		key = "admin.group.added"
	}
	switch len(change.Changed) {
	case 0:
		if add {
			return i18n.T(ctx, "admin.group.added_nobody", "group", change.Group.Name)
		}
		return i18n.T(ctx, "admin.group.removed_nobody", "group", change.Group.Name)
	case 1:
		return i18n.T(ctx, key+"_person", "user", change.Changed[0], "group", change.Group.Name)
	}
	return i18n.N(ctx, key, len(change.Changed), "count", i18n.Number(ctx, len(change.Changed)), "group", change.Group.Name)
}

// adminGroupAddress is a Groups page that verification can lead back to:
// the list, a group's tab, or one of its dialogs.
func adminGroupAddress(path string) bool {
	if path == views.AdminGroupsPath {
		return true
	}
	rest, ok := strings.CutPrefix(path, views.AdminGroupsPath+"/")
	if !ok {
		return false
	}
	id, tab, _ := strings.Cut(rest, "/")
	if id == "" || strings.ContainsAny(id, "?#%") {
		return false
	}
	switch tab {
	case "", views.AdminGroupTabSettings, views.AdminGroupTabPolicies, views.AdminGroupPolicyAssign, "add", "delete":
		return true
	}
	return false
}

func adminStatus(status int) int {
	if status == 0 {
		return http.StatusOK
	}
	return status
}
