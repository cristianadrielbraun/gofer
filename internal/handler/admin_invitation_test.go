package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

func TestAdministratorInvitationLeadsToTheAdministratorSignUp(t *testing.T) {
	_, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	page := getSecuritySettingsPath(t, stack, "/admin/users", sessionCookie).Body.String()
	if !strings.Contains(page, `name="role" value="administrator"`) || !strings.Contains(page, "data-admin-invitation-role") {
		t.Fatalf("the invitation dialog doesn't offer inviting an administrator: %q", page)
	}
	created := postSecuritySettings(t, stack, adminUserInvitationPath, url.Values{
		"name": {"Ines Varga"}, "username": {"ines.varga"}, "role": {"administrator"},
		auth.CSRFFormFieldName: {csrfProofFromForm(t, page, adminUserInvitationPath)},
	}, sessionCookie)
	body := created.Body.String()
	match := administratorInvitationTokenPattern.FindStringSubmatch(body)
	if created.Code != http.StatusCreated || match == nil || !strings.Contains(body, "/admin/enroll") ||
		!strings.Contains(body, "data-admin-invitation-administrator") || !strings.Contains(body, "Gofer administrator invitation for ines.varga") {
		t.Fatalf("administrator invitation = %d %q", created.Code, body)
	}
	token := match[1]
	var userType string
	if err := db.Read().QueryRow(`SELECT user_type FROM users WHERE username_normalized = 'ines.varga'`).Scan(&userType); err != nil || userType != "management" {
		t.Fatalf("invited administrator's type = %q, %v", userType, err)
	}
	if refused := postSecuritySettings(t, stack, adminUserInvitationPath, url.Values{
		"name": {"Odd Role"}, "username": {"odd.role"}, "role": {"owner"},
		auth.CSRFFormFieldName: {csrfProofFromForm(t, page, adminUserInvitationPath)},
	}, sessionCookie); refused.Code != http.StatusBadRequest {
		t.Fatalf("an unknown role = %d", refused.Code)
	}

	// The sign-up is open without a session, and only for administrators.
	signUp := getSecuritySettingsPath(t, stack, auth.AdminEnrollmentPath)
	if signUp.Code != http.StatusOK || !strings.Contains(signUp.Body.String(), `action="/admin/enroll"`) ||
		!strings.Contains(signUp.Body.String(), "Become a Gofer administrator") || strings.Contains(signUp.Body.String(), "/account/enroll/google") {
		t.Fatalf("administrator sign-up page = %d %q", signUp.Code, signUp.Body.String())
	}
	password := "an invented administrator passphrase"
	form := url.Values{"token": {token}, "new_password": {password}, "confirm_password": {password}}
	if wrong := postSecuritySettings(t, stack, invitationEnrollmentPath, form); wrong.Code != http.StatusBadRequest {
		t.Fatalf("mail-user sign-up with an administrator invitation = %d %q", wrong.Code, wrong.Body.String())
	}
	accepted := postSecuritySettings(t, stack, auth.AdminEnrollmentPath, form)
	if accepted.Code != http.StatusSeeOther || accepted.Header().Get("Location") != auth.AdminEnrollmentPath+"/complete" {
		t.Fatalf("administrator sign-up = %d %q %q", accepted.Code, accepted.Header().Get("Location"), accepted.Body.String())
	}
	if done := getSecuritySettingsPath(t, stack, auth.AdminEnrollmentPath+"/complete").Body.String(); !strings.Contains(done, `href="/admin/login"`) {
		t.Fatalf("completion page = %q", done)
	}

	// Signing in, they set up two-factor authentication before anything
	// else.
	signIn := postSecuritySettings(t, stack, "/admin/login", url.Values{"identifier": {"ines.varga"}, "password": {password}})
	if signIn.Code != http.StatusSeeOther || signIn.Header().Get("Location") != "/login/mfa/enroll" {
		t.Fatalf("first sign-in = %d %q %q", signIn.Code, signIn.Header().Get("Location"), signIn.Body.String())
	}
	for _, cookie := range signIn.Result().Cookies() {
		if cookie.Name == "gofer_session" && cookie.Value != "" {
			t.Fatalf("first sign-in gave a session before two-factor setup")
		}
	}
}
