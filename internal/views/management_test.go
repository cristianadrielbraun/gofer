package views

import (
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
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

func TestAdminShellOwnsItsNavigation(t *testing.T) {
	var out strings.Builder
	page := AdminShellPage{Section: AdminSectionUsers, Content: AdminUsersPage(AdminUsersData{})}
	if err := AdminShell(page).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		"Dedicated management workspace.",
		"Sign out of Admin",
		`href="/admin/users" data-admin-navigation-link data-admin-navigation-label="Users" aria-current="page"`,
		`href="/admin/activity"`,
		`href="/admin/account"`,
		">People</p>", ">Security</p>", ">System</p>",
		`data-management-shell`,
		`src="/assets/js/passkey-authentication.js`,
		`id="main-content" class="relative flex min-h-0 min-w-0 flex-1"`,
		`body class="h-screen overflow-hidden`,
		`class="flex h-full min-h-0 overflow-hidden bg-background"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("admin shell omitted %q", want)
		}
	}
	if strings.Contains(html, "Back to mail") {
		t.Fatal("admin shell exposed the legacy Back to mail action")
	}
}

func TestAdminShellDoesNotNameTheAdministrator(t *testing.T) {
	ctx := auth.ContextWithUser(context.Background(), &auth.User{
		ID: "admin-1", Name: "Ada Example", Username: "ada.admin",
		UserType: auth.UserTypeManagement, IsAdmin: true,
	})
	var out strings.Builder
	if err := AdminAccountShell(nil, PasswordSecurityVerification(PasswordSecurityVerificationData{})).Render(ctx, &out); err != nil {
		t.Fatal(err)
	}
	// A stale session sees the shell before it verifies again; the account
	// page keeps the username hidden until then, so the shell must too.
	for _, forbidden := range []string{"Ada Example", "ada.admin"} {
		if strings.Contains(out.String(), forbidden) {
			t.Fatalf("admin shell named the administrator: %q", forbidden)
		}
	}
}

func TestAdminNavigationLabelsAreTranslated(t *testing.T) {
	ctx := context.Background()
	keys := []string{adminAccountNavItem.label}
	for _, group := range adminNavigation {
		keys = append(keys, group.label)
		for _, item := range group.items {
			keys = append(keys, item.label)
			if item.short != "" {
				keys = append(keys, item.short)
			}
		}
	}
	for _, key := range keys {
		if i18n.T(ctx, key) == key {
			t.Errorf("navigation label %s is not in the catalog", key)
		}
	}
}

func TestLocalAdminShellShowsOnlyOperationalNavigation(t *testing.T) {
	ctx := auth.ContextWithUser(context.Background(), &auth.User{
		ID: "default", UserType: auth.UserTypeWebmail, IsAdmin: true,
	})
	var out strings.Builder
	page := AdminShellPage{Section: AdminSectionAvatars, Content: AdminPage(models.AvatarStatus{}, "")}
	if err := AdminShell(page).Render(ctx, &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		`href="/admin/system/diagnostics/avatars/"`, `href="/admin/system/diagnostics/contacts"`, `href="/admin/system/diagnostics/labels"`,
		`href="/admin/system/diagnostics/operations"`, `href="/admin/security"`, "Back to webmail",
		"Local administration workspace.", ">Local</span>",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("local admin shell omitted %q", want)
		}
	}
	for _, forbidden := range []string{
		`href="/admin/users"`, `href="/admin/activity"`, `href="/admin/account"`,
		`action="/auth/logout"`, "Sign out of Admin", ">People</p>",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("local admin shell exposed managed-only control %q", forbidden)
		}
	}
}

func TestAdminShellCarriesWebmailScopeAcrossDiagnostics(t *testing.T) {
	var out strings.Builder
	scope := models.AdminWebmailScope{SelectedUserID: "mail user"}
	page := AdminShellPage{
		Section: AdminSectionAvatars, Scope: scope,
		Content: AdminPage(models.AvatarStatus{Scope: scope}, "overview"),
	}
	if err := AdminShell(page).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		`href="/admin/system/diagnostics/avatars/?user_id=mail+user"`,
		`href="/admin/system/diagnostics/contacts?user_id=mail+user"`,
		`href="/admin/system/diagnostics/labels?user_id=mail+user"`,
		`href="/admin/system/diagnostics/operations?user_id=mail+user"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("scoped management navigation omitted %q", want)
		}
	}
	if strings.Contains(html, `/admin/security?user_id=`) || strings.Contains(html, `/admin/users?user_id=`) {
		t.Fatal("webmail scope leaked into non-operational administration routes")
	}
}

func TestAdminAccountShellSuppressesExternalSignInSettings(t *testing.T) {
	var out strings.Builder
	if err := AdminAccountShell(nil, PasswordSecuritySettings(PasswordSecurityData{})).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if !strings.Contains(html, `[data-management-shell] [data-federated\-identity-settings]{display:none!important}`) {
		t.Fatal("management security layout did not suppress the webmail identity settings card")
	}
	for _, want := range []string{
		`body class="h-screen overflow-hidden`,
		`href="/admin/account" data-admin-navigation-link data-admin-navigation-label="Account security" aria-current="page"`,
		`src="/assets/js/htmx.min.js`,
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
