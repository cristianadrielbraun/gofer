package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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
	// Finance requires two-factor sign-in of Ines, who's in the group: the
	// page asks first, then signs her out as chosen.
	if _, err := db.Write().Exec(`INSERT INTO sessions (id, user_id, token_hash, auth_version, authentication_method, assurance_level, user_agent, authenticated_at, last_used_at, idle_expires_at, absolute_expires_at, created_at)
		VALUES ('ines-laptop', 'ines-moreau', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 1, 'password', 'single_factor', 'laptop', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, datetime('now', '+1 day'), datetime('now', '+30 days'), CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	asks := postSecuritySettings(t, stack, assignPath, url.Values{"policy_id": {finance}, auth.CSRFFormFieldName: {assignCSRF}}, sessionCookie)
	if body := asks.Body.String(); asks.Code != http.StatusOK || !strings.Contains(body, "Require two-factor sign-in of 1 more person?") ||
		!strings.Contains(body, `href="/admin/users/ines-moreau/sign-in"`) || !strings.Contains(body, "Ends 1 session that didn") {
		t.Fatalf("assign Finance to the group asks = %d %q", asks.Code, body)
	}
	assigned := postSecuritySettings(t, stack, assignPath, url.Values{"policy_id": {finance}, "confirm_two_factor": {"1"}, "sign_out": {"1"},
		auth.CSRFFormFieldName: {csrfProofFromForm(t, asks.Body.String(), assignPath)}}, sessionCookie)
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

	var revoked int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = 'ines-laptop' AND revoked_at IS NOT NULL`).Scan(&revoked); err != nil || revoked != 1 {
		t.Fatalf("Ines's weak session wasn't ended: %d, %v", revoked, err)
	}

	// Ines gets Finance through the group, and its two-factor sign-in
	// applies to her: her pages and the Users list say so.
	personPolicy := views.AdminUserPagePath("ines-moreau", views.AdminUserTabPolicy, "")
	person := getSecuritySettingsPath(t, stack, personPolicy, sessionCookie)
	body = person.Body.String()
	for _, want := range []string{
		`data-admin-policy-setting="auth.mfa_required"`, ">Required, always ask for the app code<", "From Finance, through Field team",
		`data-admin-target-policy="` + finance + `"`, "Through the group", `href="/admin/groups/g-field/policies"`,
	} {
		if person.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("Ines's Policy tab missing %q: %d %q", want, person.Code, body)
		}
	}
	if strings.Contains(body, `data-admin-target-policy-remove="`+finance+`"`) || strings.Contains(body, "audit.security_retention_days") || strings.Contains(body, "Finance sets this to") {
		t.Fatalf("Ines's Policy tab offers to remove a group's policy, shows a whole-server setting, or says two-factor isn't applied: %q", body)
	}
	// Which policy requires it is named, and on her page leads to it.
	if signIn := getSecuritySettingsPath(t, stack, views.AdminUserPagePath("ines-moreau", views.AdminUserTabSignIn, ""), sessionCookie).Body.String(); !strings.Contains(signIn, `href="/admin/policies/`+finance+`" class="font-medium text-primary underline-offset-4 hover:underline" data-admin-user-mfa-policy>Finance</a>: always app code`) {
		t.Fatalf("Ines's Sign-in methods tab = %q", signIn)
	}
	if list := getSecuritySettingsPath(t, stack, "/admin/users?attention=mfa_setup", sessionCookie).Body.String(); !strings.Contains(list, "ines-moreau") || strings.Contains(list, "pavel-arden") ||
		!strings.Contains(list, "Required by Finance: always app code") {
		t.Fatalf("people to set up two-factor sign-in = %q", list)
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

	// Adding Pavel to the group asks first too, and keeps who was chosen.
	addPath := views.AdminGroupAddPath("g-field", "")
	addAction := views.AdminGroupPath("g-field", "members")
	addDialog := getSecuritySettingsPath(t, stack, addPath, sessionCookie).Body.String()
	addAsks := postSecuritySettings(t, stack, addAction, url.Values{"user_id": {"pavel-arden"}, auth.CSRFFormFieldName: {csrfProofFromForm(t, addDialog, addAction)}}, sessionCookie)
	if body := addAsks.Body.String(); addAsks.Code != http.StatusOK || !strings.Contains(body, `name="user_id" value="pavel-arden"`) || !strings.Contains(body, "None of their sessions needs to end") {
		t.Fatalf("adding Pavel asks = %d %q", addAsks.Code, body)
	}
	if added := postSecuritySettings(t, stack, addAction, url.Values{"user_id": {"pavel-arden"}, "confirm_two_factor": {"1"},
		auth.CSRFFormFieldName: {csrfProofFromForm(t, addAsks.Body.String(), addAction)}}, sessionCookie); added.Code != http.StatusSeeOther {
		t.Fatalf("adding Pavel = %d %q", added.Code, added.Body.String())
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

// A policy's two-factor sign-in offers the same choices as a person's own
// requirement, and its pages and the person's say which applies.
func TestAPolicysTwoFactorChoiceMatchesThePersonsOwn(t *testing.T) {
	_, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	addBulkPeople(t, db, "active", "ines-moreau")
	list := getSecuritySettingsPath(t, stack, views.AdminPoliciesPath, sessionCookie).Body.String()
	created := postSecuritySettings(t, stack, views.AdminPoliciesPath, url.Values{"name": {"Phones"}, auth.CSRFFormFieldName: {csrfProofFromForm(t, list, views.AdminPoliciesPath)}}, sessionCookie)
	phones := strings.TrimPrefix(created.Header().Get("Location"), views.AdminPoliciesPath+"/")
	settingPath := views.AdminNamedPolicySettingPath(phones, "auth.mfa_required")
	save := func(value string, version int, extra url.Values) *httptest.ResponseRecorder {
		t.Helper()
		dialog := getSecuritySettingsPath(t, stack, settingPath, sessionCookie).Body.String()
		form := url.Values{"value": {value}, "version": {strconv.Itoa(version)}, auth.CSRFFormFieldName: {csrfProofFromForm(t, dialog, settingPath)}}
		for name, values := range extra {
			form[name] = values
		}
		return postSecuritySettings(t, stack, settingPath, form, sessionCookie)
	}
	dialog := getSecuritySettingsPath(t, stack, settingPath, sessionCookie).Body.String()
	for _, want := range []string{
		`value="passkey"`, "Required, a passkey is enough", "Required, always ask for the app code", "Not required",
		// Each choice says what it means, and the preview follows the
		// choice, as on a person's own requirement.
		"Signing in with a password still asks for the authenticator app code", "After every sign-in, passkeys included.",
		"data-admin-mfa-choice", `data-admin-mfa-for="passkey true"`, "For the people it&#39;s assigned to, signing in will need:",
		`data-admin-mfa-when="passkey"`, `data-admin-mfa-when="true"`, "Password + app code",
	} {
		if !strings.Contains(dialog, want) {
			t.Fatalf("two-factor dialog missing %q: %q", want, dialog)
		}
	}
	if saved := save("passkey", 1, nil); saved.Code != http.StatusSeeOther {
		t.Fatalf("save a passkey is enough = %d %q", saved.Code, saved.Body.String())
	}
	var passkey string
	if err := db.Read().QueryRow(`SELECT value_json FROM policy_values WHERE policy_id = ? AND setting_key = 'auth.mfa_passkey_counts'`, phones).Scan(&passkey); err != nil || passkey != "true" {
		t.Fatalf("stored passkey choice = %q, %v", passkey, err)
	}
	if tab := getSecuritySettingsPath(t, stack, views.AdminPolicyPath(phones, ""), sessionCookie).Body.String(); !strings.Contains(tab, ">Required, a passkey is enough<") {
		t.Fatalf("Phones's Sign-in tab = %q", tab)
	}
	if policies := getSecuritySettingsPath(t, stack, views.AdminPoliciesPath, sessionCookie).Body.String(); !strings.Contains(policies, "Sets 1 setting") {
		t.Fatalf("the passkey choice counted on its own: %q", policies)
	}

	// Assigned to Ines, who has no second step yet.
	assignPath := "/admin/users/ines-moreau/" + views.AdminUserPolicyAssign
	assignDialog := getSecuritySettingsPath(t, stack, views.AdminUserPagePath("ines-moreau", views.AdminUserPolicyAssign, ""), sessionCookie).Body.String()
	asks := postSecuritySettings(t, stack, assignPath, url.Values{"policy_id": {phones}, auth.CSRFFormFieldName: {csrfProofFromForm(t, assignDialog, assignPath)}}, sessionCookie)
	if asks.Code != http.StatusOK || !strings.Contains(asks.Body.String(), "data-admin-two-factor-confirm") {
		t.Fatalf("assign asks = %d %q", asks.Code, asks.Body.String())
	}
	if done := postSecuritySettings(t, stack, assignPath, url.Values{"policy_id": {phones}, "confirm_two_factor": {"1"}, auth.CSRFFormFieldName: {csrfProofFromForm(t, asks.Body.String(), assignPath)}}, sessionCookie); done.Code != http.StatusSeeOther {
		t.Fatalf("assign = %d %q", done.Code, done.Body.String())
	}
	person := getSecuritySettingsPath(t, stack, views.AdminUserPagePath("ines-moreau", views.AdminUserTabPolicy, ""), sessionCookie).Body.String()
	if !strings.Contains(person, ">Required, a passkey is enough<") || !strings.Contains(person, "From Phones, assigned to them") {
		t.Fatalf("Ines's Policy tab = %q", person)
	}
	if users := getSecuritySettingsPath(t, stack, "/admin/users", sessionCookie).Body.String(); !strings.Contains(users, "Required by Phones: passkey is enough") {
		t.Fatalf("the Users list doesn't name the policy: %q", users)
	}

	// Her own requirement accepts a passkey; Phones taking the app code
	// is stricter, so it asks, and then it's the one named, and her own
	// requirement's dialog says a passkey alone won't do.
	if _, err := db.Write().Exec(`UPDATE users SET mfa_required = 1, mfa_passkey_counts = 1 WHERE id = 'ines-moreau'`); err != nil {
		t.Fatal(err)
	}
	if asks := save("true", 2, nil); asks.Code != http.StatusOK || !strings.Contains(asks.Body.String(), "Require two-factor sign-in of 1 more person?") {
		t.Fatalf("taking the passkey away asks = %d %q", asks.Code, asks.Body.String())
	}
	if saved := save("true", 2, url.Values{"confirm_two_factor": {"1"}}); saved.Code != http.StatusSeeOther {
		t.Fatalf("app code only = %d %q", saved.Code, saved.Body.String())
	}
	var leftover int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM policy_values WHERE policy_id = ? AND setting_key = 'auth.mfa_passkey_counts'`, phones).Scan(&leftover); err != nil || leftover != 0 {
		t.Fatalf("the passkey choice stayed after choosing the app code: %d, %v", leftover, err)
	}
	if users := getSecuritySettingsPath(t, stack, "/admin/users", sessionCookie).Body.String(); !strings.Contains(users, "Required by Phones: always app code") {
		t.Fatalf("the stricter policy isn't named: %q", users)
	}
	own := getSecuritySettingsPath(t, stack, "/admin/users/ines-moreau/mfa-policy", sessionCookie).Body.String()
	if !strings.Contains(own, "data-admin-mfa-policy-note") || !strings.Contains(own, "Phones also requires two-factor sign-in, always with the app code") ||
		!strings.Contains(own, "data-admin-mfa-policy-code") {
		t.Fatalf("Ines's own requirement dialog = %q", own)
	}
}
