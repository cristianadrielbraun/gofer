package views

import (
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestManagementLoginUsesDedicatedLocalOnlySurface(t *testing.T) {
	var out strings.Builder
	if err := ManagementLoginPage("Unable to sign in", "owner@example.com").Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		"Sign in to Gofer Admin",
		"Management accounts are separate from webmail accounts.",
		`action="/admin/login"`,
		`href="/login"`,
		"Unable to sign in",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("management login omitted %q", want)
		}
	}
	for _, forbidden := range []string{"/auth/google", "/auth/microsoft", "/auth/oidc"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("management login exposed federated webmail action %q", forbidden)
		}
	}
}

func TestManagementAdminLayoutOwnsItsNavigationShell(t *testing.T) {
	var out strings.Builder
	if err := ManagementAdminLayout(
		nil,
		AdminUsersData{},
		models.AvatarStatus{},
		models.ContactAdminStatus{},
		models.LabelAdminStatus{},
		models.MailSecurityAdminData{},
		models.MailOperationsAdminStatus{},
		"users",
		"",
	).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		"Dedicated management workspace.",
		"Sign out of Admin",
		`href="/admin/users"`,
		`href="/admin/activity"`,
		`href="/admin/account/security"`,
		`aria-current="page"`,
		`data-management-shell`,
		`src="/assets/js/passkey-authentication.js"`,
		`id="main-content" class="relative flex min-h-0 min-w-0 flex-1"`,
		`body class="h-screen overflow-hidden`,
		`class="flex h-full min-h-0 overflow-hidden bg-background"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("management shell omitted %q", want)
		}
	}
	if strings.Contains(html, "Back to mail") {
		t.Fatal("management shell exposed the legacy Back to mail action")
	}
}

func TestLocalAdminLayoutShowsOnlyOperationalNavigation(t *testing.T) {
	ctx := auth.ContextWithUser(context.Background(), &auth.User{
		ID: "default", UserType: auth.UserTypeWebmail, IsAdmin: true,
	})
	var out strings.Builder
	if err := ManagementAdminLayout(
		nil,
		AdminUsersData{},
		models.AvatarStatus{},
		models.ContactAdminStatus{},
		models.LabelAdminStatus{},
		models.MailSecurityAdminData{},
		models.MailOperationsAdminStatus{},
		"avatars",
		"",
	).Render(ctx, &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		`href="/admin/avatars/"`, `href="/admin/contacts"`, `href="/admin/labels"`,
		`href="/admin/operations"`, `href="/admin/security"`, "Back to webmail",
		"Local administration workspace.", ">Local</span>",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("local admin shell omitted %q", want)
		}
	}
	for _, forbidden := range []string{
		`href="/admin/users"`, `href="/admin/activity"`, `href="/admin/account/security"`,
		`action="/auth/logout"`, "Sign out of Admin",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("local admin shell exposed managed-only control %q", forbidden)
		}
	}
}

func TestManagementAdminLayoutCarriesWebmailScopeAcrossOperationalNavigation(t *testing.T) {
	var out strings.Builder
	if err := ManagementAdminLayout(
		nil,
		AdminUsersData{},
		models.AvatarStatus{Scope: models.AdminWebmailScope{SelectedUserID: "mail user"}},
		models.ContactAdminStatus{},
		models.LabelAdminStatus{},
		models.MailSecurityAdminData{},
		models.MailOperationsAdminStatus{},
		"avatars",
		"overview",
	).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		`href="/admin/avatars/?user_id=mail+user"`,
		`href="/admin/contacts?user_id=mail+user"`,
		`href="/admin/labels?user_id=mail+user"`,
		`href="/admin/operations?user_id=mail+user"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("scoped management navigation omitted %q", want)
		}
	}
	if strings.Contains(html, `/admin/security?user_id=`) || strings.Contains(html, `/admin/users?user_id=`) {
		t.Fatal("webmail scope leaked into non-operational administration routes")
	}
}

func TestManagementSecurityLayoutSuppressesExternalSignInSettings(t *testing.T) {
	var out strings.Builder
	if err := ManagementSecurityLayout(nil, PasswordSecuritySettings(PasswordSecurityData{})).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if !strings.Contains(html, `[data-management-shell] [data-federated\-identity-settings]{display:none!important}`) {
		t.Fatal("management security layout did not suppress the webmail identity settings card")
	}
	for _, want := range []string{
		`body class="h-screen overflow-hidden`,
		`src="/assets/js/htmx.min.js"`,
		`hx-get="/settings/security/activity?page=1"`,
		`id="security-activity-dialog-body"`,
		`class="flex h-full min-h-0 overflow-hidden bg-background"`,
		`id="main-content" class="min-h-0 min-w-0 flex-1 overflow-y-auto overscroll-contain"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("management security layout omitted bounded scroll contract %q", want)
		}
	}
	for _, forbidden := range []string{"/settings/security/identities/google/link", "/settings/security/identities/microsoft/link", "/settings/security/identities/oidc/link"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("management security layout exposed identity-link action %q", forbidden)
		}
	}
}

func TestSeparatedSetupOwnerExplainsManagementOnlyAccount(t *testing.T) {
	var out strings.Builder
	if err := SeparatedSetupOwnerPage(SetupOwnerData{}).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		"Create the management owner",
		"used only for Gofer administration",
		"Management users cannot own mailboxes",
		`name="owner_target" value="create"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("separated setup page omitted %q", want)
		}
	}
}
