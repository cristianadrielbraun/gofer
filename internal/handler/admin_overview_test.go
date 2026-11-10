package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

func TestAdminOverviewIsTheConsolesFrontPage(t *testing.T) {
	manager, db, stack, cookie, _ := completedSecuritySettingsStack(t)
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, username, username_normalized, name, status, auth_version,
			mfa_required, password_reset_requested_at, user_type, is_admin, created_at, updated_at
		) VALUES ('forgetful-user', 'forgetful-user', 'forgetful-user', 'Forgetful User',
			'active', 1, 0, ?, 'webmail', 0, ?, ?),
		         ('unprotected-user', 'unprotected-user', 'unprotected-user', 'Unprotected User',
			'active', 1, 1, NULL, 'webmail', 0, ?, ?)`, now, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	page := getSecuritySettingsPath(t, stack, "/admin/security", cookie)
	form := url.Values{"domain": {"mail.sample.test"}, "acknowledge": {"yes"}}
	form.Set(auth.CSRFFormFieldName, csrfProofFromForm(t, page.Body.String(), "/admin/security/http-discovery"))
	if response := postSecuritySettings(t, stack, "/admin/security/http-discovery", form, cookie); response.Code != http.StatusSeeOther {
		t.Fatalf("add exception = %d", response.Code)
	}

	root := getSecuritySettingsPath(t, stack, "/admin", cookie)
	if root.Code != http.StatusFound || root.Header().Get("Location") != "/admin/overview" {
		t.Fatalf("admin root = %d %q", root.Code, root.Header().Get("Location"))
	}
	overview := getSecuritySettingsPath(t, stack, "/admin/overview", cookie)
	html := overview.Body.String()
	for _, want := range []string{
		"data-admin-overview", "1 person asked for a password reset", `data-admin-overview-item="reset_requests"`,
		"1 person still has to set up two-factor authentication",
		"Allowed HTTP discovery for mail.sample.test", `href="/admin/activity/administration"`,
		`data-admin-navigation-label="Overview"`,
	} {
		if overview.Code != http.StatusOK || !strings.Contains(html, want) {
			t.Fatalf("overview = %d, missing %q in %q", overview.Code, want, html)
		}
	}
	if !strings.Contains(html[strings.Index(html, `data-admin-navigation-label="Overview"`):], `aria-current="page"`) {
		t.Fatal("the navigation doesn't mark the Overview")
	}
	people := getSecuritySettingsPath(t, stack, "/admin/users", cookie).Body.String()
	if strings.Count(people, "data-admin-user-mfa-setup-pending") != 1 || !strings.Contains(people, "Not set up yet") {
		t.Fatalf("the people list doesn't show who still has to set up two-factor authentication: %q", people)
	}

	session, err := manager.GetSessionByToken(t.Context(), cookie.Value)
	if err != nil || session == nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `UPDATE sessions SET step_up_at = ? WHERE id = ?`, now.Add(-11*time.Minute), session.ID); err != nil {
		t.Fatal(err)
	}
	stale := getSecuritySettingsPath(t, stack, "/admin/overview", cookie)
	staleHTML := stale.Body.String()
	if stale.Code != http.StatusOK || strings.Contains(staleHTML, "mail.sample.test") ||
		!strings.Contains(staleHTML, "Verify again on the Activity page") ||
		!strings.Contains(staleHTML, "1 person asked for a password reset") {
		t.Fatalf("stale overview = %d %q", stale.Code, staleHTML)
	}
}
