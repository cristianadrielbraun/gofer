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

	if moved := getSecuritySettingsPath(t, stack, views.AdminPoliciesPath, sessionCookie); moved.Code != http.StatusFound || moved.Header().Get("Location") != views.AdminDefaultPolicyPath {
		t.Fatalf("Policies = %d %q", moved.Code, moved.Header().Get("Location"))
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
		`href="/admin/policies/default" data-admin-navigation-link`,
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
