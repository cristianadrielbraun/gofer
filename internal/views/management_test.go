package views

import (
	"context"
	"strings"
	"testing"

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
		`href="/admin/account/security"`,
		`aria-current="page"`,
		`data-management-shell`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("management shell omitted %q", want)
		}
	}
	if strings.Contains(html, "Back to mail") {
		t.Fatal("management shell exposed the legacy Back to mail action")
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
