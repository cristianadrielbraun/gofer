package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func TestGroupAndPersonPagesShowAndChangePolicies(t *testing.T) {
	_, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	addBulkPeople(t, db, "active", "ines-moreau", "pavel-arden")
	for _, statement := range []string{
		`INSERT INTO directory_groups (id, name, name_normalized, created_at, updated_at) VALUES ('g-field', 'Field team', 'field team', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO directory_group_members (group_id, user_id, added_at) VALUES ('g-field', 'ines-moreau', CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	list := getSecuritySettingsPath(t, stack, views.AdminPoliciesPath, sessionCookie).Body.String()
	createCSRF := csrfProofFromForm(t, list, views.AdminPoliciesPath)
	create := func(name string) string {
		t.Helper()
		response := postSecuritySettings(t, stack, views.AdminPoliciesPath, url.Values{"name": {name}, auth.CSRFFormFieldName: {createCSRF}}, sessionCookie)
		if response.Code != http.StatusSeeOther {
			t.Fatalf("create %s = %d %q", name, response.Code, response.Body.String())
		}
		return strings.TrimPrefix(response.Header().Get("Location"), views.AdminPoliciesPath+"/")
	}
	finance := create("Finance")
	contractors := create("Contractors")
	settingPath := views.AdminNamedPolicySettingPath(finance, "auth.mfa_required")
	dialog := getSecuritySettingsPath(t, stack, settingPath, sessionCookie).Body.String()
	if saved := postSecuritySettings(t, stack, settingPath, url.Values{"value": {"true"}, "version": {"1"}, auth.CSRFFormFieldName: {csrfProofFromForm(t, dialog, settingPath)}}, sessionCookie); saved.Code != http.StatusSeeOther {
		t.Fatalf("set two-factor on Finance = %d %q", saved.Code, saved.Body.String())
	}

	// The group starts with no policy, and isn't protected.
	groupPolicies := views.AdminGroupPath("g-field", views.AdminGroupTabPolicies)
	page := getSecuritySettingsPath(t, stack, groupPolicies, sessionCookie)
	if body := page.Body.String(); page.Code != http.StatusOK || !strings.Contains(body, "No policy is assigned to this group.") || strings.Contains(body, "data-admin-group-protected") {
		t.Fatalf("group Policies tab = %d %q", page.Code, body)
	}
	assignPath := views.AdminGroupPath("g-field", views.AdminGroupPolicyAssign)
	assign := getSecuritySettingsPath(t, stack, assignPath, sessionCookie).Body.String()
	for _, want := range []string{`data-admin-target-policy-choice="` + finance + `"`, `data-admin-target-policy-choice="` + contractors + `"`, `data-tui-dialog-open="true"`, "Sets: Two-factor sign-in"} {
		if !strings.Contains(assign, want) {
			t.Fatalf("group Assign dialog missing %q: %q", want, assign)
		}
	}
	assignCSRF := csrfProofFromForm(t, assign, assignPath)
	if refused := postSecuritySettings(t, stack, assignPath, url.Values{"policy_id": {finance}}, sessionCookie); refused.Code != http.StatusForbidden {
		t.Fatalf("assign without CSRF = %d", refused.Code)
	}
	if nothing := postSecuritySettings(t, stack, assignPath, url.Values{auth.CSRFFormFieldName: {assignCSRF}}, sessionCookie); nothing.Code != http.StatusBadRequest ||
		!strings.Contains(nothing.Body.String(), "Choose a policy first.") {
		t.Fatalf("assign nothing = %d %q", nothing.Code, nothing.Body.String())
	}
	assigned := postSecuritySettings(t, stack, assignPath, url.Values{"policy_id": {finance}, auth.CSRFFormFieldName: {assignCSRF}}, sessionCookie)
	if assigned.Code != http.StatusSeeOther || assigned.Header().Get("Location") != groupPolicies {
		t.Fatalf("assign Finance to the group = %d %q %q", assigned.Code, assigned.Header().Get("Location"), assigned.Body.String())
	}
	body := getSecuritySettingsPath(t, stack, groupPolicies, sessionCookie).Body.String()
	for _, want := range []string{`data-admin-target-policy="` + finance + `"`, "data-admin-group-protected", `data-admin-target-policy-remove="` + finance + `"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("group Policies tab after assigning missing %q: %q", want, body)
		}
	}
	if groups := getSecuritySettingsPath(t, stack, views.AdminGroupsPath, sessionCookie).Body.String(); !strings.Contains(groups, ">Protected<") {
		t.Fatalf("Groups list doesn't mark the group protected: %q", groups)
	}
	if again := getSecuritySettingsPath(t, stack, assignPath, sessionCookie).Body.String(); strings.Contains(again, `data-admin-target-policy-choice="`+finance+`"`) {
		t.Fatalf("Assign dialog offers a policy the group has: %q", again)
	}
	if confirm := getSecuritySettingsPath(t, stack, views.AdminGroupPath("g-field", "delete"), sessionCookie).Body.String(); !strings.Contains(confirm, "Its members lose the policy assigned to it: Finance.") {
		t.Fatalf("group delete confirmation = %q", confirm)
	}

	// Ines gets Finance through the group: saved, but not applied yet, so
	// her two-factor sign-in still follows the Default policy.
	personPolicy := views.AdminUserPagePath("ines-moreau", views.AdminUserTabPolicy, "")
	person := getSecuritySettingsPath(t, stack, personPolicy, sessionCookie)
	body = person.Body.String()
	for _, want := range []string{
		`data-admin-policy-setting="auth.mfa_required"`, "Set on the Default policy", "Finance sets this to “Required”, but that isn",
		`data-admin-target-policy="` + finance + `"`, "Through the group", `href="/admin/groups/g-field/policies"`,
	} {
		if person.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("Ines's Policy tab missing %q: %d %q", want, person.Code, body)
		}
	}
	if strings.Contains(body, `data-admin-target-policy-remove="`+finance+`"`) || strings.Contains(body, "audit.security_retention_days") {
		t.Fatalf("Ines's Policy tab offers to remove a group's policy, or shows a whole-server setting: %q", body)
	}

	// Contractors assigned to her directly, from her page, then removed.
	list = "/admin/users?q=ines"
	personAssign := views.AdminUserPagePath("ines-moreau", views.AdminUserPolicyAssign, list)
	dialog = getSecuritySettingsPath(t, stack, personAssign, sessionCookie).Body.String()
	if !strings.Contains(dialog, `data-admin-target-policy-choice="`+finance+`"`) || !strings.Contains(dialog, `data-admin-target-policy-choice="`+contractors+`"`) {
		t.Fatalf("Ines's Assign dialog = %q", dialog)
	}
	assignAction := "/admin/users/ines-moreau/" + views.AdminUserPolicyAssign
	personAssigned := postSecuritySettings(t, stack, assignAction, url.Values{"policy_id": {contractors}, "list": {list}, auth.CSRFFormFieldName: {csrfProofFromForm(t, dialog, personAssign)}}, sessionCookie)
	if want := views.AdminUserPagePath("ines-moreau", views.AdminUserTabPolicy, list); personAssigned.Code != http.StatusSeeOther || personAssigned.Header().Get("Location") != want {
		t.Fatalf("assign Contractors to Ines = %d %q %q", personAssigned.Code, personAssigned.Header().Get("Location"), personAssigned.Body.String())
	}
	body = getSecuritySettingsPath(t, stack, personPolicy, sessionCookie).Body.String()
	removeAction := "/admin/users/ines-moreau/policy/remove"
	if !strings.Contains(body, `data-admin-target-policy-remove="`+contractors+`"`) || !strings.Contains(body, "Assigned to them") {
		t.Fatalf("Ines's Policy tab after assigning = %q", body)
	}
	if removed := postSecuritySettings(t, stack, removeAction, url.Values{"policy_id": {contractors}, auth.CSRFFormFieldName: {csrfProofFromForm(t, body, removeAction)}}, sessionCookie); removed.Code != http.StatusSeeOther {
		t.Fatalf("remove Contractors from Ines = %d %q", removed.Code, removed.Body.String())
	}
	if body := getSecuritySettingsPath(t, stack, personPolicy, sessionCookie).Body.String(); strings.Contains(body, `data-admin-target-policy="`+contractors+`"`) {
		t.Fatalf("Contractors still applies to Ines: %q", body)
	}

	// Policies don't reach administrators.
	var administrator string
	if err := db.Read().QueryRow(`SELECT id FROM users WHERE is_admin = 1`).Scan(&administrator); err != nil {
		t.Fatal(err)
	}
	if body := getSecuritySettingsPath(t, stack, views.AdminUserPagePath(administrator, views.AdminUserTabPolicy, ""), sessionCookie).Body.String(); !strings.Contains(body, "Policies apply to the people who use mail.") ||
		strings.Contains(body, "data-admin-target-policy-assign") {
		t.Fatalf("an administrator's Policy tab = %q", body)
	}

	// Check a person leads to their Policy tab.
	lookup := getSecuritySettingsPath(t, stack, views.AdminPolicyLookupPath, sessionCookie).Body.String()
	if !strings.Contains(lookup, `data-admin-policy-lookup-person="ines-moreau"`) || !strings.Contains(lookup, `href="/admin/users/ines-moreau/policy"`) ||
		strings.Contains(lookup, `data-admin-policy-lookup-person="`+administrator+`"`) {
		t.Fatalf("Check a person = %q", lookup)
	}
	if search := getSecuritySettingsPath(t, stack, views.AdminPolicyLookupSearchPath("pav"), sessionCookie).Body.String(); strings.Contains(search, `data-admin-policy-lookup-person="ines-moreau"`) ||
		!strings.Contains(search, `data-admin-policy-lookup-person="pavel-arden"`) {
		t.Fatalf("Check a person, searching = %q", search)
	}

	// The group's policy removed from its page.
	body = getSecuritySettingsPath(t, stack, groupPolicies, sessionCookie).Body.String()
	groupRemove := views.AdminGroupPath("g-field", "policies/remove")
	if removed := postSecuritySettings(t, stack, groupRemove, url.Values{"policy_id": {finance}, auth.CSRFFormFieldName: {csrfProofFromForm(t, body, groupRemove)}}, sessionCookie); removed.Code != http.StatusSeeOther {
		t.Fatalf("remove Finance from the group = %d %q", removed.Code, removed.Body.String())
	}
	if body := getSecuritySettingsPath(t, stack, groupPolicies, sessionCookie).Body.String(); strings.Contains(body, "data-admin-group-protected") {
		t.Fatalf("the group is still protected: %q", body)
	}

	var recorded int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action IN ('policy.assigned', 'policy.unassigned')`).Scan(&recorded); err != nil || recorded != 4 {
		t.Fatalf("assignment records = %d, %v", recorded, err)
	}
}

func TestVerificationLeadsBackToPolicyAssignments(t *testing.T) {
	for value, want := range map[string]string{
		"/admin/groups/g-field/policies":            "/admin/groups/g-field/policies",
		"/admin/groups/g-field/policies/assign":     "/admin/groups/g-field/policies/assign",
		"/admin/groups/g-field/policies/remove":     "",
		"/admin/users/ines-moreau/policy":           "/admin/users/ines-moreau/policy",
		"/admin/users/ines-moreau/policy/assign":    "/admin/users/ines-moreau/policy/assign",
		"/admin/users/ines-moreau/policy/remove":    "",
		"/admin/users/ines-moreau/policy/elsewhere": "",
	} {
		if got := adminSecurityVerificationReturnTo(value); got != want {
			t.Errorf("return to %q = %q, want %q", value, got, want)
		}
	}
}
