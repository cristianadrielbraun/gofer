package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func TestSignInPageRequiresTwoFactorOfEveryoneAfterAPreview(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	addBulkPeople(t, db, "active", "ines.moreau", "pavel.arden")
	addBulkPeople(t, db, "disabled", "left.already")
	if _, err := manager.CreateAuthenticatedSession(t.Context(), "ines.moreau", "Ines's laptop", auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor); err != nil {
		t.Fatal(err)
	}

	page := getSecuritySettingsPath(t, stack, views.AdminSignInPath, sessionCookie)
	body := page.Body.String()
	for _, want := range []string{
		`data-admin-two-factor-scope="administrators"`, `href="/admin/security/sign-in"`, "Require of everyone",
		"2 people have no authenticator app yet", `href="/admin/users/ines.moreau/sign-in"`, `href="/admin/users/pavel.arden/sign-in"`,
		"1 session that didn&#39;t use the authenticator app will end now.", `action="/admin/security/sign-in/two-factor"`,
	} {
		if page.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("sign-in page = %d, missing %q: %q", page.Code, want, body)
		}
	}
	if strings.Contains(body, "left.already") {
		t.Fatal("the preview named someone disabled")
	}
	if strings.Contains(body, `data-tui-dialog-open="true"`) {
		t.Fatal("the confirmation opened by itself")
	}
	if dialog := getSecuritySettingsPath(t, stack, views.AdminSignInTwoFactorPath, sessionCookie).Body.String(); !strings.Contains(dialog, `data-tui-dialog-open="true"`) {
		t.Fatalf("confirmation at its address = %q", dialog)
	}

	if refused := postSecuritySettings(t, stack, views.AdminSignInTwoFactorPath, url.Values{"policy": {"all_users"}}, sessionCookie); refused.Code != http.StatusForbidden {
		t.Fatalf("change without CSRF = %d", refused.Code)
	}
	csrf := csrfProofFromForm(t, body, views.AdminSignInTwoFactorPath)
	if invalid := postSecuritySettings(t, stack, views.AdminSignInTwoFactorPath, url.Values{"policy": {"some_people"}, auth.CSRFFormFieldName: {csrf}}, sessionCookie); invalid.Code != http.StatusBadRequest {
		t.Fatalf("unknown policy = %d", invalid.Code)
	}
	changed := postSecuritySettings(t, stack, views.AdminSignInTwoFactorPath, url.Values{"policy": {"all_users"}, auth.CSRFFormFieldName: {csrf}}, sessionCookie)
	if changed.Code != http.StatusSeeOther || changed.Header().Get("Location") != views.AdminSignInPath {
		t.Fatalf("require of everyone = %d %q %q", changed.Code, changed.Header().Get("Location"), changed.Body.String())
	}
	var policy string
	if err := db.Read().QueryRow(`SELECT mfa_policy FROM auth_system_state WHERE id = 1`).Scan(&policy); err != nil || policy != "all_users" {
		t.Fatalf("stored policy = %q, %v", policy, err)
	}
	var recorded int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'instance.mfa_policy_changed'`).Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("audit records = %d, %v", recorded, err)
	}

	page = getSecuritySettingsPath(t, stack, views.AdminSignInPath, sessionCookie)
	body = page.Body.String()
	if !strings.Contains(body, `data-admin-two-factor-scope="all_users"`) || !strings.Contains(body, "Require of administrators only") ||
		strings.Contains(body, "data-admin-two-factor-preview") {
		t.Fatalf("sign-in page after the change = %q", body)
	}
}
