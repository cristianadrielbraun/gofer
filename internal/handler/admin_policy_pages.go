package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/cristianadrielbraun/gofer/internal/admin"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/policy"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// adminPolicyCandidatesShown is how many people the Assign dialog offers
// at once; a search narrows them. Groups are all offered.
const adminPolicyCandidatesShown = 30

// adminPolicyNotApplied are the settings a named policy saves that don't
// take effect per person yet: sign-in and proxies only read the Default
// policy's value until they're wired to the resolver.
var adminPolicyNotApplied = map[string]bool{
	policy.MFARequired.Key: true,
	policy.OwnProxies.Key:  true,
}

// adminPolicyChoices are what a named policy can set each setting to.
var adminPolicyChoices = map[string][]struct{ value, label string }{
	policy.MFARequired.Key: {{"true", "admin.policies.mfa.required"}, {"false", "admin.policies.mfa.not_required"}},
	policy.OwnProxies.Key:  {{"true", "admin.policies.own_proxies.on"}, {"false", "admin.policies.own_proxies.off"}},
}

func (h *Handler) registerAdminPolicyPageRoutes(adminRoute func(string, http.HandlerFunc)) {
	adminRoute("GET "+views.AdminPoliciesPath, h.handleAdminPolicies)
	adminRoute("GET "+views.AdminPoliciesPath+"/{$}", h.handleAdminPolicies)
	adminRoute("POST "+views.AdminPoliciesPath, h.handleCreateAdminPolicy)
	adminRoute("POST "+views.AdminPoliciesPath+"/{policyID}/move", h.handleMoveAdminPolicy)
	adminRoute("GET "+views.AdminPoliciesPath+"/{policyID}", h.handleNamedPolicy(adminNamedPolicyExtras{}))
	adminRoute("GET "+views.AdminPoliciesPath+"/{policyID}/{tab}", h.handleNamedPolicyTab)
	adminRoute("GET "+views.AdminPoliciesPath+"/{policyID}/settings/{key}", h.handleNamedPolicySettingDialog)
	adminRoute("POST "+views.AdminPoliciesPath+"/{policyID}/settings/{key}", h.handleSetNamedPolicySetting)
	adminRoute("POST "+views.AdminPoliciesPath+"/{policyID}/assign", h.handleChangeAdminPolicyAssignments(true))
	adminRoute("POST "+views.AdminPoliciesPath+"/{policyID}/unassign", h.handleChangeAdminPolicyAssignments(false))
	adminRoute("POST "+views.AdminPoliciesPath+"/{policyID}/"+views.AdminNamedPolicyTabDetails, h.handleUpdateAdminPolicy)
	adminRoute("POST "+views.AdminPoliciesPath+"/{policyID}/delete", h.handleDeleteAdminPolicy)
}

// adminPoliciesExtras are what the Policies list shows over itself: the
// New policy dialog as it was sent, and a change that was refused.
type adminPoliciesExtras struct {
	status     int
	form       views.AdminGroupForm
	createOpen bool
	lookupOpen bool
	pageError  string
}

func (h *Handler) handleAdminPolicies(w http.ResponseWriter, r *http.Request) {
	h.renderAdminPolicies(w, r, adminPoliciesExtras{})
}

func (h *Handler) renderAdminPolicies(w http.ResponseWriter, r *http.Request, extras adminPoliciesExtras) {
	ctx := r.Context()
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	summaries, err := h.adminService().ListPolicies(ctx, actor)
	if err != nil {
		h.adminPolicyLoadFailed(w, r, err)
		return
	}
	data := views.AdminPoliciesData{
		Form: extras.form, CreateOpen: extras.createOpen, LookupOpen: extras.lookupOpen,
		CreateCSRF: auth.CSRFToken(ctx, http.MethodPost, views.AdminPoliciesPath),
	}
	if data.LookupOpen {
		if err := h.adminPolicyLookupPeople(ctx, actor, r.URL.Query().Get("q"), &data); err != nil {
			h.adminPolicyLoadFailed(w, r, err)
			return
		}
	}
	named := 0
	for _, summary := range summaries {
		if !summary.Default {
			named++
		}
	}
	for _, summary := range summaries {
		row := views.AdminPolicyRow{
			ID: summary.ID, Name: summary.Name, Description: summary.Description, Priority: summary.Priority,
			Settings: summary.Settings, Groups: summary.Groups, People: summary.People,
		}
		if summary.Default {
			row.Name = i18n.T(ctx, "admin.policies.default_name")
			row.Description = i18n.T(ctx, "admin.policies.default_description")
			data.Default = row
			continue
		}
		row.First, row.Last = summary.Priority == 1, summary.Priority == named
		row.MoveAction = views.AdminPolicyPath(summary.ID, "move")
		row.MoveCSRF = auth.CSRFToken(ctx, http.MethodPost, row.MoveAction)
		data.Policies = append(data.Policies, row)
	}
	verification, stepUp, ok := h.adminStepUpVerification(w, r, views.AdminPoliciesPath, extras.pageError != "")
	if !ok {
		return
	}
	data.StepUpRequired = stepUp
	h.renderAdminPage(w, r, adminStatus(extras.status), views.AdminShellPage{
		Section: views.AdminSectionPolicies,
		Content: views.AdminPoliciesPage(data, verification),
		Toasts:  adminErrorToasts(extras.pageError),
	})
}

func (h *Handler) handleCreateAdminPolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	form := views.AdminGroupForm{Name: r.PostFormValue("name"), Description: r.PostFormValue("description")}
	created, err := h.adminService().CreatePolicy(ctx, actor, form.Name, form.Description)
	if err == nil {
		h.setAdminFlashSuccess(w, i18n.T(ctx, "admin.policies.created", "policy", created.Name))
		redirectAdmin(w, r, views.AdminPolicyPath(created.ID, ""))
		return
	}
	extras := adminPoliciesExtras{form: form, createOpen: true, status: http.StatusBadRequest}
	if !adminPolicyFormError(ctx, err, &extras.form) {
		extras.status, extras.pageError = adminPolicyRefusal(ctx, err, "creating")
		extras.createOpen = extras.status != http.StatusForbidden
	}
	h.renderAdminPolicies(w, r, extras)
}

// adminPolicyFormError puts a refused name or description on its field,
// and says whether it did.
func adminPolicyFormError(ctx context.Context, err error, form *views.AdminGroupForm) bool {
	switch {
	case errors.Is(err, policy.ErrNameTaken):
		form.NameError = i18n.T(ctx, "admin.policies.error.name_taken")
	case errors.Is(err, policy.ErrNameInvalid):
		form.NameError = i18n.T(ctx, "admin.groups.error.name_invalid")
	case errors.Is(err, policy.ErrDescriptionLong):
		form.DescriptionError = i18n.T(ctx, "admin.groups.error.description_invalid")
	default:
		return false
	}
	return true
}

// adminPolicyRefusal is the status and message for a change to policies
// that didn't go through for a reason no field shows.
func adminPolicyRefusal(ctx context.Context, err error, doing string) (int, string) {
	switch admin.CodeOf(err) {
	case admin.CodeForbidden:
		return http.StatusForbidden, i18n.T(ctx, "server.error.admin_access_required")
	case admin.CodeVerificationRequired:
		return http.StatusForbidden, i18n.T(ctx, "admin.policies.verify")
	case admin.CodeStale:
		return http.StatusConflict, i18n.T(ctx, "admin.policies.error.stale")
	case admin.CodeUnavailable:
		return http.StatusConflict, i18n.T(ctx, "admin.policies.error.unavailable")
	case admin.CodeInvalid:
		return http.StatusBadRequest, i18n.T(ctx, "admin.policies.error.invalid")
	case admin.CodeTooMany:
		return http.StatusBadRequest, i18n.N(ctx, "admin.groups.error.too_many", admin.MaxBulkPeople, "count", i18n.Number(ctx, admin.MaxBulkPeople))
	}
	log.Printf("%s a policy: %v", doing, err)
	return http.StatusInternalServerError, i18n.T(ctx, "admin.groups.error.failed")
}

// handleMoveAdminPolicy moves a policy one place up or down the order.
func (h *Handler) handleMoveAdminPolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	service := h.adminService()
	summaries, err := service.ListPolicies(ctx, actor)
	if err != nil {
		h.adminPolicyLoadFailed(w, r, err)
		return
	}
	var order []string
	for _, summary := range summaries {
		if !summary.Default {
			order = append(order, summary.ID)
		}
	}
	at := -1
	for i, id := range order {
		if id == r.PathValue("policyID") {
			at = i
		}
	}
	to := at - 1
	if r.PostFormValue("move") == "down" {
		to = at + 1
	}
	if at < 0 || to < 0 || to >= len(order) {
		redirectAdmin(w, r, views.AdminPoliciesPath)
		return
	}
	order[at], order[to] = order[to], order[at]
	if err := service.ReorderPolicies(ctx, actor, order); err != nil {
		status, message := adminPolicyRefusal(ctx, err, "moving")
		h.renderAdminPolicies(w, r, adminPoliciesExtras{status: status, pageError: message})
		return
	}
	redirectAdmin(w, r, views.AdminPoliciesPath)
}

// adminNamedPolicyExtras are what a named policy's page shows over its
// tab: a dialog open, the details as they were sent, and a refusal.
type adminNamedPolicyExtras struct {
	status     int
	tab        string
	setting    string
	assignOpen bool
	deleteOpen bool
	details    *views.AdminGroupForm
	pageError  string
	verify     bool
}

// adminNamedPolicyTabs are a named policy's tabs: each category with a
// setting a named policy can set, then who it's assigned to and its
// details.
func adminNamedPolicyTabs(ctx context.Context) []views.AdminPolicyTab {
	var tabs []views.AdminPolicyTab
	for _, candidate := range adminPolicyTabs {
		for _, setting := range policy.Settings() {
			if _, shown := adminPolicySettings[setting.Key]; shown && !setting.InstanceOnly && setting.Category == candidate.category {
				tabs = append(tabs, views.AdminPolicyTab{Tab: candidate.tab, Label: i18n.T(ctx, candidate.label)})
				break
			}
		}
	}
	return append(tabs,
		views.AdminPolicyTab{Tab: views.AdminNamedPolicyTabAssigned, Label: i18n.T(ctx, "admin.policies.tab.assigned")},
		views.AdminPolicyTab{Tab: views.AdminNamedPolicyTabDetails, Label: i18n.T(ctx, "admin.policies.tab.details")},
	)
}

func (h *Handler) handleNamedPolicy(extras adminNamedPolicyExtras) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.renderNamedPolicy(w, r, extras)
	}
}

// handleNamedPolicyTab is a tab of a named policy's page, or one of the
// addresses that opens a dialog.
func (h *Handler) handleNamedPolicyTab(w http.ResponseWriter, r *http.Request) {
	extras := adminNamedPolicyExtras{tab: r.PathValue("tab")}
	switch extras.tab {
	case "assign":
		extras.tab, extras.assignOpen = views.AdminNamedPolicyTabAssigned, true
	case "delete":
		extras.tab, extras.deleteOpen = views.AdminNamedPolicyTabDetails, true
	}
	known := false
	for _, tab := range adminNamedPolicyTabs(r.Context()) {
		known = known || tab.Tab == extras.tab
	}
	if !known || extras.tab == views.AdminPolicyTabSignIn {
		http.NotFound(w, r)
		return
	}
	h.renderNamedPolicy(w, r, extras)
}

func (h *Handler) handleNamedPolicySettingDialog(w http.ResponseWriter, r *http.Request) {
	setting, ok := adminNamedPolicySettingFor(r.PathValue("key"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.renderNamedPolicy(w, r, adminNamedPolicyExtras{tab: adminPolicyTabOf(setting), setting: setting.Key})
}

// adminNamedPolicySettingFor is a setting a named policy can set.
func adminNamedPolicySettingFor(key string) (*policy.Setting, bool) {
	setting, ok := adminPolicySettingFor(key)
	if !ok || setting.InstanceOnly {
		return nil, false
	}
	if _, choices := adminPolicyChoices[key]; !choices {
		return nil, false
	}
	return setting, true
}

// renderNamedPolicy draws a named policy's page.
func (h *Handler) renderNamedPolicy(w http.ResponseWriter, r *http.Request, extras adminNamedPolicyExtras) {
	ctx := r.Context()
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	policyID := r.PathValue("policyID")
	if policyID == policy.DefaultID {
		redirectAdmin(w, r, views.AdminDefaultPolicyPath)
		return
	}
	detail, err := h.adminService().Policy(ctx, actor, policyID)
	switch admin.CodeOf(err) {
	case "":
	case admin.CodeUnavailable:
		if extras.pageError != "" {
			h.setAdminFlashError(w, extras.pageError)
			redirectAdmin(w, r, views.AdminPoliciesPath)
			return
		}
		http.NotFound(w, r)
		return
	default:
		h.adminPolicyLoadFailed(w, r, err)
		return
	}
	found := detail.Policy
	data := views.AdminNamedPolicyData{
		ID: found.ID, Name: found.Name, Description: found.Description, Priority: found.Priority, Version: found.Version,
		Tab: extras.tab, Tabs: adminNamedPolicyTabs(ctx),
		CreatedAt: found.CreatedAt, CreatedBy: h.adminUsername(ctx, found.CreatedBy),
		UpdatedAt: found.UpdatedAt, UpdatedBy: h.adminUsername(ctx, found.UpdatedBy),
		AssignOpen: extras.assignOpen, DeleteOpen: extras.deleteOpen,
		Details: views.AdminGroupForm{Name: found.Name, Description: found.Description},
		CSRF: views.AdminNamedPolicyCSRF{
			Assign:   auth.CSRFToken(ctx, http.MethodPost, views.AdminPolicyPath(found.ID, "assign")),
			Unassign: auth.CSRFToken(ctx, http.MethodPost, views.AdminPolicyPath(found.ID, "unassign")),
			Details:  auth.CSRFToken(ctx, http.MethodPost, views.AdminPolicyPath(found.ID, views.AdminNamedPolicyTabDetails)),
			Delete:   auth.CSRFToken(ctx, http.MethodPost, views.AdminPolicyPath(found.ID, "delete")),
		},
	}
	if extras.details != nil {
		data.Details = *extras.details
	}
	assigned := map[string]bool{}
	for _, assignment := range detail.Assignments {
		target := adminPolicyTargetView(assignment.Target, assignment.Name)
		target.AddedAt, target.AddedBy = assignment.CreatedAt, assignment.CreatedByUsername
		data.Assigned = append(data.Assigned, target)
		assigned[target.Value] = true
	}
	values := map[string]policy.Value{}
	for _, value := range detail.Values {
		values[value.Key] = value
	}
	for _, setting := range policy.Settings() {
		if _, ok := adminNamedPolicySettingFor(setting.Key); !ok || adminPolicyTabOf(setting) != data.Tab {
			continue
		}
		data.Settings = append(data.Settings, adminNamedPolicyRow(ctx, found.ID, setting, values))
		if setting.Key == extras.setting {
			data.Dialog = adminNamedPolicyDialog(ctx, found, setting, values)
		}
	}
	if data.AssignOpen {
		if err := h.adminPolicyCandidates(ctx, actor, r.URL.Query().Get("q"), assigned, &data); err != nil {
			h.adminPolicyLoadFailed(w, r, err)
			return
		}
	}

	returnTo := views.AdminPolicyPath(found.ID, data.Tab)
	switch {
	case extras.setting != "":
		returnTo = views.AdminNamedPolicySettingPath(found.ID, extras.setting)
	case extras.assignOpen:
		returnTo = views.AdminPolicyAssignPath(found.ID, "")
	case extras.deleteOpen:
		returnTo = views.AdminPolicyPath(found.ID, "delete")
	}
	verification, stepUp, ok := h.adminStepUpVerification(w, r, returnTo, extras.verify || extras.pageError != "")
	if !ok {
		return
	}
	data.StepUpRequired = stepUp
	h.renderAdminPage(w, r, adminStatus(extras.status), views.AdminShellPage{
		Section: views.AdminSectionPolicies,
		Content: views.AdminNamedPolicyPage(data, verification),
		Toasts:  adminErrorToasts(extras.pageError),
	})
}

// adminNamedPolicyRow is a setting's row on a named policy: its value, or
// that the policy leaves it to the ones after it.
func adminNamedPolicyRow(ctx context.Context, policyID string, setting *policy.Setting, values map[string]policy.Value) views.AdminPolicySetting {
	row := views.AdminPolicySetting{
		Key: setting.Key, Label: i18n.T(ctx, setting.Label), Help: i18n.T(ctx, setting.Help),
		EditPath: views.AdminNamedPolicySettingPath(policyID, setting.Key),
		Value:    i18n.T(ctx, "admin.policies.not_set"),
		Source:   i18n.T(ctx, "admin.policies.not_set_detail"),
	}
	if stored, ok := values[setting.Key]; ok {
		row.Value, row.Source = adminPolicyChoiceLabel(ctx, setting.Key, stored.JSON), ""
	}
	if adminPolicyNotApplied[setting.Key] {
		row.Note = i18n.T(ctx, "admin.policies.not_applied")
	}
	return row
}

func adminPolicyChoiceLabel(ctx context.Context, key string, raw json.RawMessage) string {
	for _, choice := range adminPolicyChoices[key] {
		if strings.TrimSpace(string(raw)) == choice.value {
			return i18n.T(ctx, choice.label)
		}
	}
	return string(raw)
}

func adminNamedPolicyDialog(ctx context.Context, found policy.Policy, setting *policy.Setting, values map[string]policy.Value) *views.AdminNamedPolicySettingDialog {
	action := views.AdminNamedPolicySettingPath(found.ID, setting.Key)
	dialog := &views.AdminNamedPolicySettingDialog{
		Key: setting.Key, Title: i18n.T(ctx, setting.Label), Help: i18n.T(ctx, setting.Help),
		Action: action, CSRFToken: auth.CSRFToken(ctx, http.MethodPost, action), Version: found.Version,
		NotApplied: adminPolicyNotApplied[setting.Key],
	}
	for _, choice := range adminPolicyChoices[setting.Key] {
		dialog.Choices = append(dialog.Choices, views.AdminPolicyChoice{Value: choice.value, Label: i18n.T(ctx, choice.label)})
	}
	if stored, ok := values[setting.Key]; ok {
		dialog.Current = strings.TrimSpace(string(stored.JSON))
	}
	return dialog
}

// adminPolicyTargetView is a group or person as the page names them.
func adminPolicyTargetView(target policy.Target, name string) views.AdminPolicyTarget {
	return views.AdminPolicyTarget{Value: string(target.Kind) + ":" + target.ID, Group: target.Kind == policy.TargetGroup, ID: target.ID, Name: name}
}

// adminPolicyTargetsFrom reads the targets a form names, as "group:ID" or
// "user:ID", leaving out anything else. The store refuses an ID that names
// no group or webmail user.
func adminPolicyTargetsFrom(values []string) []policy.Target {
	var targets []policy.Target
	for _, value := range values {
		kind, id, ok := strings.Cut(strings.TrimSpace(value), ":")
		if !ok || id == "" || len(id) > 128 || strings.IndexFunc(id, unicode.IsControl) >= 0 {
			continue
		}
		switch policy.TargetKind(kind) {
		case policy.TargetGroup, policy.TargetUser:
			targets = append(targets, policy.Target{Kind: policy.TargetKind(kind), ID: id})
		}
	}
	return targets
}

// adminPolicyCandidates fills in the Assign dialog: the groups whose name
// holds search, then the people whose username does, leaving out the ones
// the policy has.
func (h *Handler) adminPolicyCandidates(ctx context.Context, actor admin.Actor, search string, assigned map[string]bool, data *views.AdminNamedPolicyData) error {
	data.AssignSearch = strings.TrimSpace(search)
	needle := strings.ToLower(data.AssignSearch)
	groups, err := h.adminService().ListGroups(ctx, actor)
	if err != nil {
		return err
	}
	for _, group := range groups {
		target := adminPolicyTargetView(policy.Target{Kind: policy.TargetGroup, ID: group.ID}, group.Name)
		if assigned[target.Value] || (needle != "" && !strings.Contains(strings.ToLower(group.Name), needle)) {
			continue
		}
		data.Candidates = append(data.Candidates, target)
	}
	page, err := h.auth.ListAdministratorUserPage(ctx, actor.UserID, actor.SessionID, auth.AdministratorUserQuery{
		Search: data.AssignSearch, Groupable: true, Limit: adminPolicyCandidatesShown + len(assigned),
	})
	if err != nil {
		return err
	}
	shown := 0
	for _, person := range page.Users {
		target := adminPolicyTargetView(policy.Target{Kind: policy.TargetUser, ID: person.ID}, person.Username)
		if assigned[target.Value] {
			continue
		}
		if shown == adminPolicyCandidatesShown {
			data.CandidatesMore = true
			break
		}
		target.Status = string(person.Status)
		data.Candidates = append(data.Candidates, target)
		shown++
	}
	data.CandidatesMore = data.CandidatesMore || page.Matching > len(page.Users)
	return nil
}

// handleSetNamedPolicySetting sets a setting on a named policy, or leaves
// it to the policies after it when the value is empty.
func (h *Handler) handleSetNamedPolicySetting(w http.ResponseWriter, r *http.Request) {
	setting, ok := adminNamedPolicySettingFor(r.PathValue("key"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	change := policy.ValueChange{Key: setting.Key}
	if value := strings.TrimSpace(r.PostFormValue("value")); value != "" {
		change.JSON = json.RawMessage(value)
	}
	version, _ := strconv.ParseInt(r.PostFormValue("version"), 10, 64)
	policyID := r.PathValue("policyID")
	if _, err := h.adminService().SetPolicyValues(ctx, actor, policyID, version, []policy.ValueChange{change}); err != nil {
		status, message := adminPolicyRefusal(ctx, err, "changing a setting of")
		extras := adminNamedPolicyExtras{tab: adminPolicyTabOf(setting), status: status, pageError: message}
		if admin.CodeOf(err) == admin.CodeVerificationRequired || admin.CodeOf(err) == admin.CodeInvalid {
			extras.setting = setting.Key
		}
		h.renderNamedPolicy(w, r, extras)
		return
	}
	h.setAdminFlashSuccess(w, i18n.T(ctx, "admin.policies.setting_saved", "setting", i18n.T(ctx, setting.Label)))
	redirectAdmin(w, r, views.AdminPolicyPath(policyID, adminPolicyTabOf(setting)))
}

func (h *Handler) handleChangeAdminPolicyAssignments(assign bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := h.adminActor(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		if err := r.ParseForm(); err != nil {
			http.Error(w, i18n.T(ctx, "server.error.invalid_form"), http.StatusBadRequest)
			return
		}
		policyID := r.PathValue("policyID")
		targets := adminPolicyTargetsFrom(r.PostForm["target"])
		service := h.adminService()
		var change *auth.PolicyAssignmentChange
		var err error
		if assign {
			change, err = service.AssignPolicy(ctx, actor, policyID, targets)
		} else {
			change, err = service.UnassignPolicy(ctx, actor, policyID, targets)
		}
		if err != nil {
			status, message := adminPolicyRefusal(ctx, err, "changing who has")
			if admin.CodeOf(err) == admin.CodeInvalid && len(targets) == 0 {
				message = i18n.T(ctx, "admin.groups.error.nobody")
			}
			h.renderNamedPolicy(w, r, adminNamedPolicyExtras{
				tab: views.AdminNamedPolicyTabAssigned, status: status, pageError: message,
				assignOpen: assign && admin.CodeOf(err) != admin.CodeForbidden,
			})
			return
		}
		h.setAdminFlashSuccess(w, adminPolicyAssignmentMessage(ctx, assign, change))
		redirectAdmin(w, r, views.AdminPolicyPath(policyID, views.AdminNamedPolicyTabAssigned))
	}
}

// adminPolicyAssignmentMessage says who a change to a policy's
// assignments affected, naming one and counting more.
func adminPolicyAssignmentMessage(ctx context.Context, assign bool, change *auth.PolicyAssignmentChange) string {
	key := "admin.policies.unassigned"
	if assign {
		key = "admin.policies.assigned"
	}
	switch len(change.Changed) {
	case 0:
		return i18n.T(ctx, "admin.policies.unchanged")
	case 1:
		return i18n.T(ctx, key+"_one", "name", change.Changed[0].Name, "policy", change.Policy.Name)
	}
	return i18n.N(ctx, key, len(change.Changed), "count", i18n.Number(ctx, len(change.Changed)), "policy", change.Policy.Name)
}

func (h *Handler) handleUpdateAdminPolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	policyID := r.PathValue("policyID")
	form := views.AdminGroupForm{Name: r.PostFormValue("name"), Description: r.PostFormValue("description")}
	version, _ := strconv.ParseInt(r.PostFormValue("version"), 10, 64)
	updated, err := h.adminService().UpdatePolicy(ctx, actor, policyID, version, form.Name, form.Description)
	if err == nil {
		h.setAdminFlashSuccess(w, i18n.T(ctx, "admin.policies.saved", "policy", updated.Name))
		redirectAdmin(w, r, views.AdminPolicyPath(policyID, views.AdminNamedPolicyTabDetails))
		return
	}
	extras := adminNamedPolicyExtras{tab: views.AdminNamedPolicyTabDetails, status: http.StatusBadRequest, details: &form}
	if !adminPolicyFormError(ctx, err, &form) {
		extras.status, extras.pageError = adminPolicyRefusal(ctx, err, "changing")
		if admin.CodeOf(err) == admin.CodeStale {
			extras.details = nil
		}
	}
	h.renderNamedPolicy(w, r, extras)
}

func (h *Handler) handleDeleteAdminPolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	policyID := r.PathValue("policyID")
	service := h.adminService()
	name := ""
	if detail, err := service.Policy(ctx, actor, policyID); err == nil {
		name = detail.Policy.Name
	}
	version, _ := strconv.ParseInt(r.PostFormValue("version"), 10, 64)
	if err := service.DeletePolicy(ctx, actor, policyID, version); err != nil {
		status, message := adminPolicyRefusal(ctx, err, "deleting")
		h.renderNamedPolicy(w, r, adminNamedPolicyExtras{
			tab: views.AdminNamedPolicyTabDetails, status: status, pageError: message,
			deleteOpen: admin.CodeOf(err) == admin.CodeVerificationRequired,
		})
		return
	}
	h.setAdminFlashSuccess(w, i18n.T(ctx, "admin.policies.deleted", "policy", name))
	redirectAdmin(w, r, views.AdminPoliciesPath)
}
