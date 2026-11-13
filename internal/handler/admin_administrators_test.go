package handler

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func TestAdministratorsPageListsAndInvitesAdministrators(t *testing.T) {
	_, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	addBulkPeople(t, db, "active", "mail-only-person")
	page := getSecuritySettingsPath(t, stack, views.AdminAdministratorsPath, sessionCookie)
	body := page.Body.String()
	if page.Code != http.StatusOK || strings.Count(body, "data-admin-administrator>") != 1 || strings.Contains(body, "mail-only-person") ||
		!strings.Contains(body, `href="/admin/administrators"`) || !strings.Contains(body, `name="role" value="administrator" checked`) {
		t.Fatalf("administrators page = %d %q", page.Code, body)
	}

	// Inviting from here comes back here, with the invitation.
	created := postSecuritySettings(t, stack, adminUserInvitationPath, url.Values{
		"name": {"Tamsin Brook"}, "username": {"tamsin.brook"}, "role": {"administrator"},
		auth.CSRFFormFieldName: {csrfProofFromForm(t, body, adminUserInvitationPath)},
	}, sessionCookie)
	body = created.Body.String()
	if created.Code != http.StatusCreated || !strings.Contains(body, "data-admin-administrators") || !strings.Contains(body, "/admin/enroll") ||
		strings.Count(body, "data-admin-administrator>") != 2 {
		t.Fatalf("invitation from the page = %d %q", created.Code, body)
	}
	// The pending administrator's invitation can be replaced from here, and
	// the new one shows here too.
	rotate := regexp.MustCompile(`href="(/admin/users/invitations/[A-Za-z0-9_-]{43}/rotate)\?list=%2Fadmin%2Fadministrators"`).FindStringSubmatch(body)
	if rotate == nil {
		t.Fatalf("no way to replace the pending administrator's invitation: %q", body)
	}
	dialog := getSecuritySettingsPath(t, stack, rotate[1]+"?list="+url.QueryEscape(views.AdminAdministratorsPath), sessionCookie).Body.String()
	if !strings.Contains(dialog, "data-admin-administrators") || !strings.Contains(dialog, `name="list" value="/admin/administrators"`) {
		t.Fatalf("replace dialog at its address from the page = %q", dialog)
	}
	replaced := postSecuritySettings(t, stack, rotate[1], url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, dialog, rotate[1])}, "list": {views.AdminAdministratorsPath},
	}, sessionCookie)
	if replaced.Code != http.StatusCreated || !strings.Contains(replaced.Body.String(), "data-admin-administrators") ||
		!strings.Contains(replaced.Body.String(), "Gofer administrator invitation for tamsin.brook") {
		t.Fatalf("replaced invitation = %d %q", replaced.Code, replaced.Body.String())
	}

	// A person's page opened from here leads back here.
	link := regexp.MustCompile(`href="(/admin/users/[^"/?]+\?list=%2Fadmin%2Fadministrators)"`).FindStringSubmatch(body)
	if link == nil {
		t.Fatalf("no link to an administrator's page: %q", body)
	}
	personal := getSecuritySettingsPath(t, stack, html.UnescapeString(link[1]), sessionCookie).Body.String()
	if !strings.Contains(personal, `href="/admin/administrators"`) || !strings.Contains(personal, "data-admin-user-back") {
		t.Fatalf("administrator's page = %q", personal)
	}
}

func TestAdministratorSignInResetFromTheAdministratorsPage(t *testing.T) {
	_, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	if _, err := db.Write().Exec(`INSERT INTO users (id, username, username_normalized, name, status, auth_version, user_type, is_admin, created_at, updated_at)
		VALUES ('keeper-admin', 'keeper.admin', 'keeper.admin', '', 'active', 1, 'management', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO totp_credentials (id, user_id, encrypted_seed, key_version, enabled, created_at)
		VALUES ('keeper-totp', 'keeper-admin', x'01', 1, 1, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}

	// Only the other administrator can be reset; nobody resets themselves.
	body := getSecuritySettingsPath(t, stack, views.AdminAdministratorsPath, sessionCookie).Body.String()
	resets := map[string]bool{}
	for _, match := range regexp.MustCompile(`/admin/users/([^/"?]+)/credential-reset`).FindAllStringSubmatch(body, -1) {
		resets[match[1]] = true
	}
	if len(resets) != 1 || !resets["keeper-admin"] || !strings.Contains(body, "Reset sign-in") {
		t.Fatalf("sign-in resets offered = %v in %q", resets, body)
	}
	resetPath := "/admin/users/keeper-admin/credential-reset"
	dialog := getSecuritySettingsPath(t, stack, resetPath+"?list="+url.QueryEscape(views.AdminAdministratorsPath), sessionCookie).Body.String()
	if !strings.Contains(dialog, "data-admin-administrators") || !strings.Contains(dialog, "authenticator app, recovery codes and passkeys stop working") {
		t.Fatalf("reset dialog = %q", dialog)
	}
	issued := postSecuritySettings(t, stack, resetPath, url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, dialog, resetPath)}, "list": {views.AdminAdministratorsPath},
	}, sessionCookie)
	body = issued.Body.String()
	token := regexp.MustCompile(`Reset token: ([A-Za-z0-9_-]{20,})`).FindStringSubmatch(body)
	if issued.Code != http.StatusCreated || token == nil || !strings.Contains(body, "data-admin-administrators") ||
		!strings.Contains(body, "Gofer administrator sign-in reset for keeper.admin") || !strings.Contains(body, auth.AdminResetPath) {
		t.Fatalf("issued reset = %d %q", issued.Code, body)
	}

	// The reset page is open without a session.
	page := getSecuritySettingsPath(t, stack, auth.AdminResetPath)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `action="/admin/reset"`) || !strings.Contains(page.Body.String(), `href="/admin/login"`) {
		t.Fatalf("reset page = %d %q", page.Code, page.Body.String())
	}
	password := "an invented passphrase for the keeper"
	used := postSecuritySettings(t, stack, auth.AdminResetPath, url.Values{"token": {token[1]}, "new_password": {password}, "confirm_password": {password}})
	if used.Code != http.StatusSeeOther || used.Header().Get("Location") != auth.AdminResetPath+"/complete" {
		t.Fatalf("using the reset = %d %q %q", used.Code, used.Header().Get("Location"), used.Body.String())
	}
	if done := getSecuritySettingsPath(t, stack, auth.AdminResetPath+"/complete").Body.String(); !strings.Contains(done, `href="/admin/login"`) {
		t.Fatalf("completion page = %q", done)
	}
	// Signing in, they set up two-factor authentication again first.
	signIn := postSecuritySettings(t, stack, "/admin/login", url.Values{"identifier": {"keeper.admin"}, "password": {password}})
	if signIn.Code != http.StatusSeeOther || signIn.Header().Get("Location") != "/login/mfa/enroll" {
		t.Fatalf("sign-in after the reset = %d %q", signIn.Code, signIn.Header().Get("Location"))
	}
}
