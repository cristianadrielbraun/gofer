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
