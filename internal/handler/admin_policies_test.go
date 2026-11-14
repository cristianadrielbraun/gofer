package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const (
	twoFactorSettingPath  = "/admin/policies/default/settings/auth.mfa_required"
	retentionSettingPath  = "/admin/policies/default/settings/audit.security_retention_days"
	ownProxiesSettingPath = "/admin/policies/default/settings/mail.own_proxies"
)

func TestPoliciesPageRequiresTwoFactorOfEveryoneAfterAPreview(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	addBulkPeople(t, db, "active", "ines.moreau", "pavel.arden")
	addBulkPeople(t, db, "disabled", "left.already")
	if _, err := manager.CreateAuthenticatedSession(t.Context(), "ines.moreau", "Ines's laptop", auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor); err != nil {
		t.Fatal(err)
	}

	if list := getSecuritySettingsPath(t, stack, views.AdminPoliciesPath, sessionCookie); list.Code != http.StatusOK ||
		!strings.Contains(list.Body.String(), `data-admin-policy-row="default"`) || !strings.Contains(list.Body.String(), "No policies yet") {
		t.Fatalf("Policies = %d %q", list.Code, list.Body.String())
	}
	// The Sign-in page's control moved here.
	for old, now := range map[string]string{"/admin/security/sign-in": views.AdminDefaultPolicyPath, "/admin/security/sign-in/two-factor": twoFactorSettingPath} {
		if moved := getSecuritySettingsPath(t, stack, old, sessionCookie); moved.Code != http.StatusFound || moved.Header().Get("Location") != now {
			t.Fatalf("%s = %d %q", old, moved.Code, moved.Header().Get("Location"))
		}
	}

	page := getSecuritySettingsPath(t, stack, views.AdminDefaultPolicyPath, sessionCookie)
	body := page.Body.String()
	for _, want := range []string{
		`data-admin-policy-setting="auth.mfa_required"`, "Two-factor sign-in", ">Administrators<", "Set on the Default policy",
		`href="/admin/users?attention=mfa_setup"`, "2 people", `href="` + twoFactorSettingPath + `"`,
		`href="/admin/policies/default/mail"`, "Mail accounts", `href="/admin/policies/default/activity"`, "Activity",
		`href="/admin/policies" data-admin-navigation-link`, `data-admin-policy-back`,
	} {
		if page.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("Default policy = %d, missing %q: %q", page.Code, want, body)
		}
	}
	if strings.Contains(body, `href="/admin/security/sign-in"`) || strings.Contains(body, "data-admin-two-factor-preview") {
		t.Fatalf("Default policy still links the Sign-in page, or opened a dialog: %q", body)
	}

	dialog := getSecuritySettingsPath(t, stack, twoFactorSettingPath, sessionCookie).Body.String()
	for _, want := range []string{
		`data-tui-dialog-open="true"`, "data-admin-two-factor-preview", "2 people have no authenticator app yet",
		`href="/admin/users/ines.moreau/sign-in"`, "1 session that didn&#39;t use the authenticator app will end now.",
		`name="value" value="true"`,
	} {
		if !strings.Contains(dialog, want) {
			t.Fatalf("two-factor dialog missing %q: %q", want, dialog)
		}
	}
	if strings.Contains(dialog, "left.already") {
		t.Fatal("the preview named someone disabled")
	}

	if refused := postSecuritySettings(t, stack, twoFactorSettingPath, url.Values{"value": {"true"}}, sessionCookie); refused.Code != http.StatusForbidden {
		t.Fatalf("change without CSRF = %d", refused.Code)
	}
	csrf := csrfProofFromForm(t, dialog, twoFactorSettingPath)
	if invalid := postSecuritySettings(t, stack, twoFactorSettingPath, url.Values{"value": {"some_people"}, auth.CSRFFormFieldName: {csrf}}, sessionCookie); invalid.Code != http.StatusBadRequest {
		t.Fatalf("unknown value = %d", invalid.Code)
	}
	changed := postSecuritySettings(t, stack, twoFactorSettingPath, url.Values{"value": {"true"}, auth.CSRFFormFieldName: {csrf}}, sessionCookie)
	if changed.Code != http.StatusSeeOther || changed.Header().Get("Location") != views.AdminDefaultPolicyPath {
		t.Fatalf("require of everyone = %d %q %q", changed.Code, changed.Header().Get("Location"), changed.Body.String())
	}
	if flash, ok := adminFlashOf(t, changed); !ok || !strings.Contains(flash.Message, "1 session") {
		t.Fatalf("result = %+v %v", flash, ok)
	}
	var stored string
	var recorded int
	if err := db.Read().QueryRow(`SELECT mfa_policy FROM auth_system_state WHERE id = 1`).Scan(&stored); err != nil || stored != "all_users" {
		t.Fatalf("stored policy = %q, %v", stored, err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'instance.mfa_policy_changed'`).Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("audit records = %d, %v", recorded, err)
	}
	body = getSecuritySettingsPath(t, stack, views.AdminDefaultPolicyPath, sessionCookie).Body.String()
	if !strings.Contains(body, ">Everyone<") || !strings.Contains(body, "Enforced") {
		t.Fatalf("Default policy after the change = %q", body)
	}
	if dialog := getSecuritySettingsPath(t, stack, twoFactorSettingPath, sessionCookie).Body.String(); !strings.Contains(dialog, `name="value" value="false"`) ||
		strings.Contains(dialog, "data-admin-two-factor-sessions") {
		t.Fatalf("two-factor dialog once required of everyone = %q", dialog)
	}

	// A session that must verify again is asked to, and led back here.
	session, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || session == nil {
		t.Fatalf("session = %#v, %v", session, err)
	}
	if _, err := db.Write().Exec(`UPDATE sessions SET step_up_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Hour), session.ID); err != nil {
		t.Fatal(err)
	}
	stale := postSecuritySettings(t, stack, twoFactorSettingPath, url.Values{"value": {"false"}, auth.CSRFFormFieldName: {csrf}}, sessionCookie)
	if body := stale.Body.String(); stale.Code != http.StatusForbidden || !strings.Contains(body, `admin-security-verification-dialog`) ||
		!strings.Contains(body, `name="return_to" value="`+twoFactorSettingPath+`"`) {
		t.Fatalf("change from a stale session = %d %q", stale.Code, body)
	}
}

func TestPoliciesPageConfirmsAShorterRetentionWithWhatItRemoves(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	page := getSecuritySettingsPath(t, stack, "/admin/policies/default/activity", sessionCookie).Body.String()
	if !strings.Contains(page, `data-admin-policy-setting="audit.security_retention_days"`) || !strings.Contains(page, "180 days") {
		t.Fatalf("Activity tab = %q", page)
	}
	dialog := getSecuritySettingsPath(t, stack, retentionSettingPath, sessionCookie).Body.String()
	csrf := csrfProofFromForm(t, dialog, retentionSettingPath)
	post := func(values url.Values) string {
		t.Helper()
		values.Set(auth.CSRFFormFieldName, csrf)
		response := postSecuritySettings(t, stack, retentionSettingPath, values, sessionCookie)
		return response.Header().Get("Location") + response.Body.String()
	}
	if got := post(url.Values{"days": {"366"}}); !strings.Contains(got, "Enter a retention period between 1 and 365 days") {
		t.Fatalf("too long = %q", got)
	}
	if got := post(url.Values{"days": {"365"}}); got != "/admin/policies/default/activity" {
		t.Fatalf("longer = %q", got)
	}

	// Two events older than 30 days, one newer.
	for i, age := range []time.Duration{40 * 24 * time.Hour, 31 * 24 * time.Hour, time.Hour} {
		if _, err := db.Write().Exec(`INSERT INTO auth_events (id, occurred_at, event_type, success) VALUES (?, ?, 'login', 1)`,
			"old-event-"+string(rune('a'+i)), time.Now().UTC().Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	confirm := post(url.Values{"days": {"30"}})
	if !strings.Contains(confirm, "removes 2 events") || !strings.Contains(confirm, `name="confirm" value="1"`) {
		t.Fatalf("shorter, not confirmed = %q", confirm)
	}
	if current, err := manager.AuthenticationEventRetention(t.Context()); err != nil || current.Days != 365 {
		t.Fatalf("retention before confirming = %+v, %v", current, err)
	}
	if got := post(url.Values{"days": {"30"}, "confirm": {"1"}}); got != "/admin/policies/default/activity" {
		t.Fatalf("shorter, confirmed = %q", got)
	}
	if current, err := manager.AuthenticationEventRetention(t.Context()); err != nil || current.Days != 30 {
		t.Fatalf("retention after confirming = %+v, %v", current, err)
	}
	// Longer needs no confirmation, even with old events still there:
	// pruning hasn't run in the test.
	if got := post(url.Values{"days": {"60"}}); got != "/admin/policies/default/activity" {
		t.Fatalf("longer again = %q", got)
	}
}

func TestPoliciesPageAllowsPeoplesOwnProxies(t *testing.T) {
	_, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	page := getSecuritySettingsPath(t, stack, "/admin/policies/default/mail", sessionCookie).Body.String()
	if !strings.Contains(page, `data-admin-policy-setting="mail.own_proxies"`) || !strings.Contains(page, "Not allowed") {
		t.Fatalf("Mail accounts tab = %q", page)
	}
	dialog := getSecuritySettingsPath(t, stack, ownProxiesSettingPath, sessionCookie).Body.String()
	if !strings.Contains(dialog, "Nobody has saved a proxy of their own.") || !strings.Contains(dialog, `name="value" value="false" checked`) {
		t.Fatalf("own proxies dialog = %q", dialog)
	}
	csrf := csrfProofFromForm(t, dialog, ownProxiesSettingPath)
	allowed := postSecuritySettings(t, stack, ownProxiesSettingPath, url.Values{"value": {"true"}, auth.CSRFFormFieldName: {csrf}}, sessionCookie)
	if allowed.Code != http.StatusSeeOther || allowed.Header().Get("Location") != "/admin/policies/default/mail" {
		t.Fatalf("allow = %d %q %q", allowed.Code, allowed.Header().Get("Location"), allowed.Body.String())
	}
	var stored, recorded int
	if err := db.Read().QueryRow(`SELECT people_add FROM outbound_proxy_policy WHERE singleton = 1`).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored = %d, %v", stored, err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'security.proxy_policy_changed'`).Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("audit records = %d, %v", recorded, err)
	}
	if page := getSecuritySettingsPath(t, stack, "/admin/policies/default/mail", sessionCookie).Body.String(); !strings.Contains(page, ">Allowed<") {
		t.Fatalf("Mail accounts tab after allowing = %q", page)
	}
	// The same value again changes nothing and records nothing.
	postSecuritySettings(t, stack, ownProxiesSettingPath, url.Values{"value": {"true"}, auth.CSRFFormFieldName: {csrf}}, sessionCookie)
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'security.proxy_policy_changed'`).Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("audit records after the same value = %d, %v", recorded, err)
	}
	if missing := getSecuritySettingsPath(t, stack, "/admin/policies/default/settings/auth.unknown", sessionCookie); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown setting = %d", missing.Code)
	}
}

func TestVerificationLeadsBackToPolicyPages(t *testing.T) {
	for value, want := range map[string]string{
		"/admin/policies/default":                               "/admin/policies/default",
		"/admin/policies/default/mail":                          "/admin/policies/default/mail",
		"/admin/policies/default/settings/auth.mfa_required":    "/admin/policies/default/settings/auth.mfa_required",
		"/admin/policies/default/settings/auth.nope":            "",
		"/admin/policies/default/elsewhere":                     "",
		"/admin/policies/default/settings/mail.own_proxies?x=1": "",
	} {
		if got := adminSecurityVerificationReturnTo(value); got != want {
			t.Errorf("return to %q = %q, want %q", value, got, want)
		}
	}
}

func TestPoliciesAreCreatedOrderedAssignedAndDeleted(t *testing.T) {
	_, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	addBulkPeople(t, db, "active", "ines.moreau", "pavel.arden")
	if _, err := db.Write().Exec(`INSERT INTO directory_groups (id, name, name_normalized, created_at, updated_at) VALUES ('g-field', 'Field team', 'field team', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}

	list := getSecuritySettingsPath(t, stack, views.AdminPoliciesPath, sessionCookie).Body.String()
	csrf := csrfProofFromForm(t, list, views.AdminPoliciesPath)
	create := func(name string) string {
		t.Helper()
		response := postSecuritySettings(t, stack, views.AdminPoliciesPath, url.Values{"name": {name}, auth.CSRFFormFieldName: {csrf}}, sessionCookie)
		location := response.Header().Get("Location")
		if response.Code != http.StatusSeeOther || !strings.HasPrefix(location, views.AdminPoliciesPath+"/") {
			t.Fatalf("create %s = %d %q %q", name, response.Code, location, response.Body.String())
		}
		return strings.TrimPrefix(location, views.AdminPoliciesPath+"/")
	}
	contractors := create("Contractors")
	finance := create("Finance")
	if taken := postSecuritySettings(t, stack, views.AdminPoliciesPath, url.Values{"name": {"default POLICY"}, auth.CSRFFormFieldName: {csrf}}, sessionCookie); taken.Code != http.StatusBadRequest ||
		!strings.Contains(taken.Body.String(), "Another policy already has this name.") {
		t.Fatalf("the Default policy's name = %d %q", taken.Code, taken.Body.String())
	}

	// Finance moves above Contractors.
	list = getSecuritySettingsPath(t, stack, views.AdminPoliciesPath, sessionCookie).Body.String()
	if strings.Index(list, `data-admin-policy-row="`+contractors+`"`) > strings.Index(list, `data-admin-policy-row="`+finance+`"`) ||
		strings.Index(list, `data-admin-policy-row="`+finance+`"`) > strings.Index(list, `data-admin-policy-row="default"`) {
		t.Fatalf("order before moving = %q", list)
	}
	move := views.AdminPolicyPath(finance, "move")
	if moved := postSecuritySettings(t, stack, move, url.Values{"move": {"up"}, auth.CSRFFormFieldName: {csrfProofFromForm(t, list, move)}}, sessionCookie); moved.Code != http.StatusSeeOther {
		t.Fatalf("move up = %d %q", moved.Code, moved.Body.String())
	}
	list = getSecuritySettingsPath(t, stack, views.AdminPoliciesPath, sessionCookie).Body.String()
	if strings.Index(list, `data-admin-policy-row="`+finance+`"`) > strings.Index(list, `data-admin-policy-row="`+contractors+`"`) {
		t.Fatalf("order after moving = %q", list)
	}

	// A setting on a named policy: saved, flagged as not applied yet.
	page := getSecuritySettingsPath(t, stack, views.AdminPolicyPath(finance, ""), sessionCookie).Body.String()
	if !strings.Contains(page, `data-admin-policy-setting="auth.mfa_required"`) || !strings.Contains(page, "Not set") ||
		!strings.Contains(page, "not applied yet") || strings.Contains(page, "audit.security_retention_days") {
		t.Fatalf("Finance's Sign-in tab = %q", page)
	}
	settingPath := views.AdminNamedPolicySettingPath(finance, "auth.mfa_required")
	dialog := getSecuritySettingsPath(t, stack, settingPath, sessionCookie).Body.String()
	if !strings.Contains(dialog, `data-admin-policy-not-applied`) || !strings.Contains(dialog, `name="version" value="1"`) {
		t.Fatalf("setting dialog = %q", dialog)
	}
	settingCSRF := csrfProofFromForm(t, dialog, settingPath)
	if saved := postSecuritySettings(t, stack, settingPath, url.Values{"value": {"true"}, "version": {"1"}, auth.CSRFFormFieldName: {settingCSRF}}, sessionCookie); saved.Code != http.StatusSeeOther {
		t.Fatalf("set = %d %q", saved.Code, saved.Body.String())
	}
	if stale := postSecuritySettings(t, stack, settingPath, url.Values{"value": {"false"}, "version": {"1"}, auth.CSRFFormFieldName: {settingCSRF}}, sessionCookie); stale.Code != http.StatusConflict ||
		!strings.Contains(stale.Body.String(), "This policy changed while you had it open") {
		t.Fatalf("set from an old page = %d %q", stale.Code, stale.Body.String())
	}
	if page := getSecuritySettingsPath(t, stack, views.AdminPolicyPath(finance, ""), sessionCookie).Body.String(); !strings.Contains(page, ">Required<") {
		t.Fatalf("Finance after setting it = %q", page)
	}
	if missing := getSecuritySettingsPath(t, stack, views.AdminNamedPolicySettingPath(finance, "audit.security_retention_days"), sessionCookie); missing.Code != http.StatusNotFound {
		t.Fatalf("a whole-server setting on a named policy = %d", missing.Code)
	}

	// Assigned to a group and a person, then one taken away.
	assign := getSecuritySettingsPath(t, stack, views.AdminPolicyAssignPath(finance, ""), sessionCookie).Body.String()
	for _, want := range []string{`data-admin-policy-candidate="group:g-field"`, `data-admin-policy-candidate="user:ines.moreau"`, `data-tui-dialog-open="true"`} {
		if !strings.Contains(assign, want) {
			t.Fatalf("Assign dialog missing %q: %q", want, assign)
		}
	}
	assignAction := views.AdminPolicyPath(finance, "assign")
	if assigned := postSecuritySettings(t, stack, assignAction, url.Values{"target": {"group:g-field", "user:ines.moreau"}, auth.CSRFFormFieldName: {csrfProofFromForm(t, assign, assignAction)}}, sessionCookie); assigned.Code != http.StatusSeeOther {
		t.Fatalf("assign = %d %q", assigned.Code, assigned.Body.String())
	}
	if search := getSecuritySettingsPath(t, stack, views.AdminPolicyAssignPath(finance, "ines"), sessionCookie).Body.String(); strings.Contains(search, `data-admin-policy-candidate="user:ines.moreau"`) ||
		!strings.Contains(search, "Nothing else matches the search.") {
		t.Fatalf("searching for someone assigned = %q", search)
	}
	assigned := getSecuritySettingsPath(t, stack, views.AdminPolicyPath(finance, "assigned"), sessionCookie).Body.String()
	if !strings.Contains(assigned, `data-admin-policy-target="group:g-field"`) || !strings.Contains(assigned, `data-admin-policy-target="user:ines.moreau"`) {
		t.Fatalf("Assigned to = %q", assigned)
	}
	unassign := views.AdminPolicyPath(finance, "unassign")
	if removed := postSecuritySettings(t, stack, unassign, url.Values{"target": {"user:ines.moreau"}, auth.CSRFFormFieldName: {csrfProofFromForm(t, assigned, unassign)}}, sessionCookie); removed.Code != http.StatusSeeOther {
		t.Fatalf("unassign = %d %q", removed.Code, removed.Body.String())
	}
	list = getSecuritySettingsPath(t, stack, views.AdminPoliciesPath, sessionCookie).Body.String()
	if !strings.Contains(list, "Sets 1 setting") || !strings.Contains(list, "Assigned to 1 group") {
		t.Fatalf("list after the changes = %q", list)
	}

	// Renamed, then deleted.
	details := getSecuritySettingsPath(t, stack, views.AdminPolicyPath(finance, "details"), sessionCookie).Body.String()
	detailsAction := views.AdminPolicyPath(finance, "details")
	if renamed := postSecuritySettings(t, stack, detailsAction, url.Values{"name": {"Finance team"}, "version": {"2"}, auth.CSRFFormFieldName: {csrfProofFromForm(t, details, detailsAction)}}, sessionCookie); renamed.Code != http.StatusSeeOther {
		t.Fatalf("rename = %d %q", renamed.Code, renamed.Body.String())
	}
	confirm := getSecuritySettingsPath(t, stack, views.AdminPolicyPath(finance, "delete"), sessionCookie).Body.String()
	if !strings.Contains(confirm, "It&#39;s assigned to 1 group;") || !strings.Contains(confirm, `name="version" value="3"`) {
		t.Fatalf("delete confirmation = %q", confirm)
	}
	deleteAction := views.AdminPolicyPath(finance, "delete")
	if deleted := postSecuritySettings(t, stack, deleteAction, url.Values{"version": {"3"}, auth.CSRFFormFieldName: {csrfProofFromForm(t, confirm, deleteAction)}}, sessionCookie); deleted.Code != http.StatusSeeOther ||
		deleted.Header().Get("Location") != views.AdminPoliciesPath {
		t.Fatalf("delete = %d %q %q", deleted.Code, deleted.Header().Get("Location"), deleted.Body.String())
	}
	if gone := getSecuritySettingsPath(t, stack, views.AdminPolicyPath(finance, ""), sessionCookie); gone.Code != http.StatusNotFound {
		t.Fatalf("deleted policy's page = %d", gone.Code)
	}
	if moved := getSecuritySettingsPath(t, stack, views.AdminPoliciesPath+"/default/details", sessionCookie); moved.Code == http.StatusOK {
		t.Fatalf("the Default policy has a details tab: %d", moved.Code)
	}

	var recorded int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action LIKE 'policy.%'`).Scan(&recorded); err != nil || recorded != 8 {
		t.Fatalf("policy records = %d, %v", recorded, err)
	}
	if got := adminSecurityVerificationReturnTo(views.AdminPolicyAssignPath(contractors, "")); got != views.AdminPolicyAssignPath(contractors, "") {
		t.Fatalf("verification back to the Assign dialog = %q", got)
	}
}
