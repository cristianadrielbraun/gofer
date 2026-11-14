package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/admin"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/policy"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// A group's page and a person's page list the policies assigned to them,
// and assign or remove one at a time. The change is the same one the
// policy's own page makes, recorded the same way.

// adminTargetPolicyViews are policies as a group's or person's page lists
// them.
func adminTargetPolicyViews(ctx context.Context, policies []auth.TargetPolicy) []views.AdminTargetPolicy {
	result := make([]views.AdminTargetPolicy, 0, len(policies))
	for _, item := range policies {
		view := views.AdminTargetPolicy{
			ID: item.ID, Name: item.Name, Description: item.Description, Priority: item.Priority,
			Sets: adminSettingNames(ctx, item.Keys), Security: item.Security,
			Direct: item.Direct, AddedAt: item.AssignedAt, AddedBy: item.AssignedByUsername,
		}
		for _, group := range item.Groups {
			view.Groups = append(view.Groups, views.AdminGroupRef{ID: group.ID, Name: group.Name})
		}
		result = append(result, view)
	}
	return result
}

// adminSettingNames names the settings keys are, leaving out any no longer
// declared.
func adminSettingNames(ctx context.Context, keys []string) []string {
	var names []string
	for _, key := range keys {
		if setting, ok := policy.Lookup(key); ok {
			names = append(names, i18n.T(ctx, setting.Label))
		}
	}
	return names
}

// adminTargetPolicyChoices are the named policies the Assign dialog
// offers, leaving out the ones assigned already.
func (h *Handler) adminTargetPolicyChoices(ctx context.Context, actor admin.Actor, assigned map[string]bool, data *views.AdminTargetPolicies) error {
	summaries, err := h.adminService().ListPolicies(ctx, actor)
	if err != nil {
		return err
	}
	for _, summary := range summaries {
		if summary.Default {
			continue
		}
		data.HasPolicies = true
		if assigned[summary.ID] {
			continue
		}
		data.Choices = append(data.Choices, views.AdminTargetPolicy{
			ID: summary.ID, Name: summary.Name, Description: summary.Description, Priority: summary.Priority,
			Sets: adminSettingNames(ctx, summary.Keys), Security: summary.Security,
		})
	}
	return nil
}

// adminChangeTargetPolicy assigns the policy the form names to target, or
// takes it away.
func (h *Handler) adminChangeTargetPolicy(r *http.Request, actor admin.Actor, target policy.Target, assign bool) (*auth.PolicyAssignmentChange, error) {
	ctx := r.Context()
	policyID := strings.TrimSpace(r.PostFormValue("policy_id"))
	if policyID == "" || policyID == policy.DefaultID {
		return nil, errAdminNoPolicyChosen
	}
	service := h.adminService()
	targets := []policy.Target{target}
	if assign {
		return service.AssignPolicy(ctx, actor, policyID, targets)
	}
	return service.UnassignPolicy(ctx, actor, policyID, targets)
}

// errAdminNoPolicyChosen is an Assign or Remove that named no policy.
var errAdminNoPolicyChosen = errors.New("no policy chosen")

// adminTargetPolicyRefusal is the status and message for an assignment
// from a group's or person's page that didn't go through.
func adminTargetPolicyRefusal(ctx context.Context, err error) (int, string) {
	if errors.Is(err, errAdminNoPolicyChosen) {
		return http.StatusBadRequest, i18n.T(ctx, "admin.target_policies.choose")
	}
	return adminPolicyRefusal(ctx, err, "changing who has")
}

// adminUserPolicySettings are a person's settings: the value that applies
// to them, where it comes from, and what to know about it.
func adminUserPolicySettings(ctx context.Context, person views.AdminUserData, found *auth.PersonPolicies) []views.AdminPolicySetting {
	var rows []views.AdminPolicySetting
	for _, tab := range adminPolicyTabs {
		for _, setting := range policy.Settings() {
			if _, shown := adminPolicySettings[setting.Key]; !shown || setting.InstanceOnly || setting.Category != tab.category {
				continue
			}
			rows = append(rows, adminUserPolicySetting(ctx, person, setting, found))
		}
	}
	return rows
}

func adminUserPolicySetting(ctx context.Context, person views.AdminUserData, setting *policy.Setting, found *auth.PersonPolicies) views.AdminPolicySetting {
	resolved := found.Effective.Get(setting)
	applied := resolved
	row := views.AdminPolicySetting{Key: setting.Key, Label: i18n.T(ctx, setting.Label), Help: i18n.T(ctx, setting.Help)}
	if adminPolicyNotApplied[setting.Key] && resolved.Source.Kind == policy.SourcePolicy {
		// Saved on their policy, but only the Default policy's value
		// takes effect yet.
		applied = found.Instance.Get(setting)
		row.Note = i18n.T(ctx, "admin.user.policy.not_applied", "policy", resolved.Source.PolicyName, "value", adminPolicyValueLabel(ctx, setting, resolved.Value))
	}
	row.Value = adminPolicyValueText(ctx, setting, applied.Value)
	row.Enforced = applied.Source.Enforced
	tab := adminPolicyTabOf(setting)
	switch applied.Source.Kind {
	case policy.SourceDefault:
		row.Source, row.SourceHref = i18n.T(ctx, "admin.policies.source.default"), views.AdminDefaultPolicyTabPath(tab)
	case policy.SourcePolicy:
		row.SourceHref = views.AdminPolicyPath(applied.Source.PolicyID, tab)
		row.Source = i18n.T(ctx, "admin.user.policy.source_direct", "policy", applied.Source.PolicyName)
		if applied.Source.GroupID != "" {
			row.Source = i18n.T(ctx, "admin.user.policy.source_group", "policy", applied.Source.PolicyName, "group", applied.Source.GroupName)
		}
	default:
		row.Source = i18n.T(ctx, "admin.policies.source.builtin")
	}
	if setting == policy.MFARequired && strings.HasPrefix(person.MFADetail, "individual_policy") {
		// Their own requirement, which the Sign-in methods tab shows.
		individual := i18n.T(ctx, "admin.user.policy.mfa_individual")
		if row.Note == "" {
			row.Note, row.NoteHref = individual, views.AdminUserPagePath(person.ID, views.AdminUserTabSignIn, "")
		} else {
			row.Note += " " + individual
		}
	}
	return row
}

// adminPolicyValueLabel is a value as a named policy's choices word it.
func adminPolicyValueLabel(ctx context.Context, setting *policy.Setting, value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return adminPolicyChoiceLabel(ctx, setting.Key, raw)
}

// adminUserPolicyRemove is where a person's page takes a policy away.
const adminUserPolicyRemove = views.AdminUserTabPolicy + "/remove"

func (h *Handler) registerAdminUserPolicyRoutes(adminRoute func(string, http.HandlerFunc)) {
	adminRoute("GET /admin/users/{userID}/"+views.AdminUserTabPolicy, h.handleAdminUserPage(views.AdminUserTabPolicy))
	adminRoute("GET /admin/users/{userID}/"+views.AdminUserPolicyAssign, h.handleAdminUserPolicyAssignDialog)
	adminRoute("POST /admin/users/{userID}/"+views.AdminUserPolicyAssign, h.handleChangeAdminUserPolicy(true))
	adminRoute("POST /admin/users/{userID}/"+adminUserPolicyRemove, h.handleChangeAdminUserPolicy(false))
	adminRoute("GET "+views.AdminPolicyLookupPath, h.handleAdminPolicyLookup)
}

// loadAdminUserPolicy reads a person's Policy tab into data: each setting
// for them, and the policies that apply. Administrators have none.
func (h *Handler) loadAdminUserPolicy(ctx context.Context, actor admin.Actor, person auth.AdministratorUserSummary, row views.AdminUserData, list string, assignOpen bool, data *views.AdminUserPageData) error {
	if person.UserType != auth.UserTypeWebmail {
		return nil
	}
	found, err := h.adminService().PoliciesOfPerson(ctx, actor, person.ID)
	if err != nil {
		return err
	}
	assignPath := views.AdminUserPagePath(person.ID, views.AdminUserPolicyAssign, "")
	removePath := views.AdminUserPagePath(person.ID, adminUserPolicyRemove, "")
	data.Policy = views.AdminUserPolicyData{
		Applies:  true,
		Settings: adminUserPolicySettings(ctx, row, found),
		Policies: views.AdminTargetPolicies{
			Person: true, Name: person.Username, Policies: adminTargetPolicyViews(ctx, found.Policies),
			CanAssign: !person.DeletionPending, AssignOpen: assignOpen && !person.DeletionPending, List: list,
			AssignPath: views.AdminUserPagePath(person.ID, views.AdminUserPolicyAssign, list),
			AssignCSRF: auth.CSRFToken(ctx, http.MethodPost, assignPath),
			RemovePath: removePath, RemoveCSRF: auth.CSRFToken(ctx, http.MethodPost, removePath),
		},
	}
	if !data.Policy.Policies.AssignOpen {
		return nil
	}
	assigned := map[string]bool{}
	for _, item := range found.Policies {
		if item.Direct {
			assigned[item.ID] = true
		}
	}
	return h.adminTargetPolicyChoices(ctx, actor, assigned, &data.Policy.Policies)
}

// handleAdminUserPolicyAssignDialog is a person's Policy tab with the
// Assign dialog open.
func (h *Handler) handleAdminUserPolicyAssignDialog(w http.ResponseWriter, r *http.Request) {
	h.renderAdminUserPage(w, r, r.PathValue("userID"), views.AdminUserTabPolicy, adminUserPolicyList(r.URL.Query().Get("list")), adminUserPageExtras{policyAssignOpen: true})
}

// adminUserPolicyList is the Users list a person's page came from, as a
// change from it names it.
func adminUserPolicyList(value string) string {
	list := adminUsersListFrom(value)
	if strings.HasPrefix(list, adminUsersPath+"/") {
		return ""
	}
	return list
}

// handleChangeAdminUserPolicy assigns a person the policy the form names,
// or takes it away, and goes back to their Policy tab.
func (h *Handler) handleChangeAdminUserPolicy(assign bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := h.adminActor(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		userID := r.PathValue("userID")
		list := adminUserPolicyList(r.PostFormValue("list"))
		change, err := h.adminChangeTargetPolicy(r, actor, policy.Target{Kind: policy.TargetUser, ID: userID}, assign)
		if err != nil {
			status, message := adminTargetPolicyRefusal(ctx, err)
			code := admin.CodeOf(err)
			h.renderAdminUserPage(w, r, userID, views.AdminUserTabPolicy, list, adminUserPageExtras{
				status: status, pageError: message, verify: code == admin.CodeVerificationRequired,
				policyAssignOpen: assign && code != admin.CodeForbidden && code != admin.CodeUnavailable,
			})
			return
		}
		h.setAdminFlashSuccess(w, adminPolicyAssignmentMessage(ctx, assign, change))
		redirectAdmin(w, r, views.AdminUserPagePath(userID, views.AdminUserTabPolicy, list))
	}
}

// handleAdminPolicyLookup is the Policies list with the Check a person
// dialog open, finding people by username.
func (h *Handler) handleAdminPolicyLookup(w http.ResponseWriter, r *http.Request) {
	h.renderAdminPolicies(w, r, adminPoliciesExtras{lookupOpen: true})
}

// adminPolicyLookupPeople fills in the Check a person dialog: the people
// policies apply to whose username holds search.
func (h *Handler) adminPolicyLookupPeople(ctx context.Context, actor admin.Actor, search string, data *views.AdminPoliciesData) error {
	data.LookupSearch = strings.TrimSpace(search)
	page, err := h.auth.ListAdministratorUserPage(ctx, actor.UserID, actor.SessionID, auth.AdministratorUserQuery{
		Search: data.LookupSearch, Groupable: true, Limit: adminPolicyCandidatesShown,
	})
	if err != nil {
		return err
	}
	for _, person := range page.Users {
		data.LookupPeople = append(data.LookupPeople, views.AdminPolicyTarget{
			Value: "user:" + person.ID, ID: person.ID, Name: person.Username, Status: string(person.Status),
		})
	}
	data.LookupMore = page.Matching > len(page.Users)
	return nil
}
